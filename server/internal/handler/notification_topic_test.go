package handler

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/telegram"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestNotificationTopicWorkerCutoverAndConsent(t *testing.T) {
	e := newTSNEnv(t)
	e.inbound("hello", 0)
	for _, table := range []string{"notification_outbox", "notification_topic_route_audit", "notification_topic_route", "notification_decision_audit", "notification_awareness", "notification_event"} {
		e.fx.Cleanup(t, "DELETE FROM "+table+" WHERE workspace_id=$1", e.workspaceID)
	}
	s := &service.NotificationEventService{Pool: testPool}
	w, u, a := parseUUID(e.workspaceID), parseUUID(e.userID), parseUUID(e.mikaID)
	member, err := e.q.GetMemberByUserAndWorkspace(e.ctx, db.GetMemberByUserAndWorkspaceParams{WorkspaceID: w, UserID: u})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body any) *http.Request {
		r := newRequest(method, path, body)
		r.Header.Set("X-User-ID", e.userID)
		r.Header.Set("X-Workspace-ID", e.workspaceID)
		return r.WithContext(middleware.SetMemberContext(r.Context(), e.workspaceID, member))
	}
	body := map[string]any{"agent_id": e.mikaID, "producer": "subscription-limits", "installation_id": e.installationID, "expected_revision": 0, "enabled": true, "route_owner": "direct", "legacy_stopped": false}
	configure := func(want int) db.NotificationTopicRoute {
		var result struct {
			Route db.NotificationTopicRoute `json:"route"`
		}
		testutil.Call(t, testHandler.ConfigureNotificationTopic, request("PUT", "/api/notification-topics", body)).Want(want).JSON(&result)
		return result.Route
	}
	now := time.Now().UTC().Truncate(time.Second)
	input := service.NotificationEventInput{Producer: "subscription-limits", EventKey: "old", Facts: service.NotificationFacts{Subject: "claude/account/five_hour", Period: "period-1", Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), FreshUntil: now.Add(time.Minute), Urgency: "routine"}}
	capture := func() db.NotificationEvent {
		t.Helper()
		row, err := s.CaptureShadow(e.ctx, w, u, a, input)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	old := capture()
	configure(409)
	machine := request("PUT", "/api/notification-topics", body)
	machine.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.ConfigureNotificationTopic, machine).Want(403)
	body["legacy_stopped"] = true
	// An operator attestation cannot override an unresolved legacy delivery.
	e.fx.Exec(t, `INSERT INTO task_status_notification
	 (id,workspace_id,subscription_id,user_id,agent_id,installation_id,event_id,issue_id,issue_status,dedup_key,state)
	 VALUES (gen_random_uuid(),$1,gen_random_uuid(),$2,$3,$4,
	 gen_random_uuid(),gen_random_uuid(),'done','cutover-unresolved','unknown')`, e.workspaceID, e.userID, e.mikaID, e.installationID)
	configure(409)
	e.fx.Exec(t, "DELETE FROM task_status_notification WHERE workspace_id=$1 AND dedup_key='cutover-unresolved'", e.workspaceID)
	topic := configure(200)
	if topic.EligibleAfter.Time.Before(old.CreatedAt.Time) {
		t.Fatal("activation lacks backlog cutoff")
	}
	// A stale human configuration cannot overwrite current consent.
	configure(409)
	worker := &service.NotificationEventWorker{Events: s, Transport: &telegram.EventTransport{APIBase: e.bot.server.URL, Client: e.bot.server.Client()}}
	if n, err := worker.Tick(e.ctx); err != nil || n != 0 {
		t.Fatal("zero-value worker ran", n, err)
	}
	worker.Enabled = true
	if n, err := worker.Tick(e.ctx); err != nil || n != 0 {
		t.Fatal("old shadow backlog sent", n, err)
	}
	input.EventKey = "ack-before-queue"
	known := capture()
	source := e.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": e.sessionID(), "role": "user", "message_kind": "message", "content": "ack " + uuidToString(known.ID)})
	if err = s.Acknowledge(e.ctx, w, u, a, known.ID, parseUUID(e.sessionID()), parseUUID(source)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueDirect(e.ctx, w, known.ID, known.Revision); !errors.Is(err, service.ErrNotificationNotSend) {
		t.Fatalf("initial ack lost: %v", err)
	}
	var assistantCount int
	e.fx.QueryRow(t, "SELECT count(*) FROM chat_message WHERE chat_session_id=$1 AND role='assistant'", e.sessionID()).Scan(&assistantCount)
	if assistantCount != 0 {
		t.Fatal("ack-before-queue created a canonical duplicate")
	}
	input.EventKey = "new-period"
	input.Facts.Period = "period-2"
	input.Facts.ResetAt = now.Add(2 * time.Hour)
	next := capture()
	if n, err := worker.Tick(e.ctx); err != nil || n != 1 {
		t.Fatalf("new period lost: %d %v", n, err)
	}
	replay := capture()
	if replay.ID != next.ID || replay.RouteOwner != "direct" || replay.State != "sent" {
		t.Fatalf("publish retry stole identity: %+v", replay)
	}
	input.EventKey = "independent-window"
	input.Facts.Subject = "claude/account/seven_day"
	input.Facts.Period = "weekly"
	input.Facts.ResetAt = now.Add(24 * time.Hour)
	capture()
	if n, err := worker.Tick(e.ctx); err != nil || n != 1 {
		t.Fatalf("independent window lost: %d %v", n, err)
	}
	if len(e.bot.sentMessages()) != 2 {
		t.Fatal("wrong external send count")
	}
	if _, err = e.q.GetNotificationOutbox(e.ctx, db.GetNotificationOutboxParams{WorkspaceID: w, EventID: old.ID}); err == nil {
		t.Fatal("historical event queued")
	}
	// Pending rows from revoked consent must remain available for explicit rollback.
	input.EventKey = "pending-at-revocation"
	input.Facts.Period = "pending-at-revocation"
	pending := capture()
	if _, err = s.QueueDirect(e.ctx, w, pending.ID, pending.Revision); err != nil {
		t.Fatal(err)
	}
	// Unknown outcome forbids return to the old producer, even after disabling.
	e.fx.Exec(t, "UPDATE notification_outbox SET state='unknown' WHERE event_id=$1", next.ID)
	body["expected_revision"] = topic.Revision
	body["route_owner"] = "shadow"
	body["enabled"] = false
	topic = configure(200)
	if n, err := worker.Tick(e.ctx); err != nil || n != 0 {
		t.Fatal("revoked topic attempted delivery", n, err)
	}
	out, err := e.q.GetNotificationOutbox(e.ctx, db.GetNotificationOutboxParams{WorkspaceID: w, EventID: pending.ID})
	if err != nil || out.State != "pending" || len(out.MessageIds) != 0 {
		t.Fatalf("revoked pending row lost: %+v %v", out, err)
	}
	if err = s.RollbackDirect(e.ctx, w, pending.ID); err != nil {
		t.Fatal(err)
	}
	retry := capture()
	if retry.ID != pending.ID || retry.RouteOwner != "shadow" {
		t.Fatal("rollback publish lost identity", retry)
	}
	body["expected_revision"] = topic.Revision
	body["route_owner"] = "legacy"
	configure(409)
	// A reconciliation is simulated in fixtures, never an automatic retry.
	e.fx.Exec(t, "UPDATE notification_outbox SET state='sent' WHERE event_id=$1", next.ID)
	topic = configure(200)
	input.EventKey = "legacy-interval"
	input.Facts.Period = "legacy-interval"
	capture()
	if n, err := worker.Tick(e.ctx); err != nil || n != 0 {
		t.Fatal("legacy owner allowed direct send", n, err)
	}
	// Concurrent owner updates have exactly one winning revision.
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ConfigureTopic(e.ctx, w, u, a, "subscription-limits", service.NotificationTopicInput{InstallationID: parseUUID(e.installationID), ExpectedRevision: topic.Revision, Owner: "shadow", Enabled: false})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, service.ErrNotificationConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent owner wins %d", wins)
	}
	audit, err := e.q.ListNotificationTopicRouteAudit(e.ctx, db.ListNotificationTopicRouteAuditParams{WorkspaceID: w, RouteID: topic.ID})
	if err != nil || len(audit) != 4 {
		t.Fatalf("topic audit len %d: %v", len(audit), err)
	}
}

func TestNotificationWorkerDisabledDoesNotTouchStorage(t *testing.T) {
	worker := service.NotificationEventWorker{}
	if n, err := worker.Tick(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}
