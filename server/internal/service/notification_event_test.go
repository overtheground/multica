package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type notificationFixture struct {
	s                *NotificationEventService
	fx               *testutil.Fixture
	w, u, a, session pgtype.UUID
	now              time.Time
	in               NotificationEventInput
}

func newNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()
	pool := sharedTestPool(t)
	base := testutil.New(pool, "", "")
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := base.User(t, "Notification member", "notice-"+suffix+"@test.invalid")
	workspace := base.Workspace(t, "Notification events", "notice-"+suffix)
	base.Member(t, workspace, user, "owner")
	fx := testutil.New(pool, workspace, user)
	agent := fx.Agent(t, "Mika", "", testutil.Cols{"visibility": "workspace"})
	session := fx.ChatSession(t, agent)
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := &notificationFixture{s: &NotificationEventService{Pool: pool}, fx: fx, w: util.MustParseUUID(workspace), u: util.MustParseUUID(user), a: util.MustParseUUID(agent), session: util.MustParseUUID(session), now: now}
	f.s.Now = func() time.Time { return f.now }
	f.in = NotificationEventInput{EventKey: "quota-1", Producer: "subscription-limits", Facts: NotificationFacts{Subject: "claude/a/five_hour", Period: "reset-1", Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), FreshUntil: now.Add(10 * time.Minute), Urgency: "routine"}}
	fx.Cleanup(t, "DELETE FROM notification_event WHERE workspace_id=$1", workspace)
	fx.Cleanup(t, "DELETE FROM notification_awareness WHERE workspace_id=$1", workspace)
	fx.Cleanup(t, "DELETE FROM notification_decision_audit WHERE workspace_id=$1", workspace)
	return f
}

func (f *notificationFixture) capture(t *testing.T) db.NotificationEvent {
	t.Helper()
	e, err := f.s.CaptureShadow(context.Background(), f.w, f.u, f.a, f.in)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (f *notificationFixture) memberAck(t *testing.T, e db.NotificationEvent) pgtype.UUID {
	t.Helper()
	m := f.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": util.UUIDToString(f.session), "role": "user", "message_kind": "message", "content": "ack " + util.UUIDToString(e.ID), "created_at": f.now.Add(-time.Second)})
	id := util.MustParseUUID(m)
	if err := f.s.Acknowledge(context.Background(), f.w, f.u, f.a, e.ID, f.session, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNotificationEventQueueAckSuppress(t *testing.T) {
	f := newNotificationFixture(t)
	ctx := context.Background()
	e := f.capture(t)
	d, err := f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, e.ID)
	if err != nil || d.Verdict != NotificationSend {
		t.Fatalf("initial shadow decision: %+v %v", d, err)
	}
	m := f.memberAck(t, e)
	d, err = f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, e.ID)
	if err != nil || d.Verdict != NotificationSuppress || d.AckMessageID != util.UUIDToString(m) {
		t.Fatalf("queued ack: %+v %v", d, err)
	}
	audit, err := db.New(f.s.Pool).ListNotificationDecisionAudit(ctx, db.ListNotificationDecisionAuditParams{WorkspaceID: f.w, EventID: e.ID})
	if err != nil || len(audit) != 2 || audit[1].Reason != "ack_same_period_and_action" {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	for _, table := range []string{"issue", "agent_task_queue", "task_status_notification"} {
		var n int
		query := "SELECT count(*) FROM " + table + " WHERE workspace_id=$1"
		if table == "agent_task_queue" {
			query = "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1"
			err = f.s.Pool.QueryRow(ctx, query, f.a).Scan(&n)
		} else {
			err = f.s.Pool.QueryRow(ctx, query, f.w).Scan(&n)
		}
		if err != nil || n != 0 {
			t.Fatalf("shadow created %s: %d %v", table, n, err)
		}
	}
	for _, change := range []string{"period", "subject", "action"} {
		in := f.in
		in.EventKey = change
		switch change {
		case "period":
			in.Facts.Period = "reset-2"
		case "subject":
			in.Facts.Subject = "claude/a/seven_day"
		case "action":
			in.Facts.Action = "reset_delayed"
		}
		next, err := f.s.CaptureShadow(ctx, f.w, f.u, f.a, in)
		if err != nil {
			t.Fatal(err)
		}
		d, err = f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, next.ID)
		if err != nil || d.Verdict != NotificationSend {
			t.Fatalf("%s lost: %+v %v", change, d, err)
		}
	}
}

func TestNotificationEventAckRequiresCanonicalMemberSource(t *testing.T) {
	f := newNotificationFixture(t)
	e := f.capture(t)
	ctx := context.Background()
	for _, tc := range []struct{ role, kind, text string }{
		{"assistant", "message", "ack " + util.UUIDToString(e.ID)},
		{"user", "task_status_event", "ack " + util.UUIDToString(e.ID)},
		{"user", "message", "> ack " + util.UUIDToString(e.ID)},
		{"user", "message", "Max said: ack " + util.UUIDToString(e.ID)},
	} {
		m := f.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": util.UUIDToString(f.session), "role": tc.role, "message_kind": tc.kind, "content": tc.text})
		if err := f.s.Acknowledge(ctx, f.w, f.u, f.a, e.ID, f.session, util.MustParseUUID(m)); !errors.Is(err, ErrNotificationAckSource) {
			t.Fatalf("accepted %+v: %v", tc, err)
		}
	}
	m := f.memberAck(t, e)
	if err := f.s.Acknowledge(ctx, f.w, f.u, f.a, e.ID, f.session, m); err != nil {
		t.Fatalf("replay: %v", err)
	}
	other := util.MustParseUUID(f.fx.User(t, "Other", "other-"+fmt.Sprint(time.Now().UnixNano())+"@test.invalid"))
	if err := f.s.Acknowledge(ctx, f.w, other, f.a, e.ID, f.session, m); !errors.Is(err, ErrNotificationAckSource) {
		t.Fatalf("wrong member: %v", err)
	}
}

func TestNotificationEventConcurrentCaptureRetryAndRouteOwner(t *testing.T) {
	f := newNotificationFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan db.NotificationEvent, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := f.s.CaptureShadow(ctx, f.w, f.u, f.a, f.in)
			results <- e
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id pgtype.UUID
	for e := range results {
		if id.Valid && id != e.ID {
			t.Fatal("duplicate durable event")
		}
		id = e.ID
	}
	changed := f.in
	changed.Facts.Action = "different"
	if _, err := f.s.CaptureShadow(ctx, f.w, f.u, f.a, changed); !errors.Is(err, ErrNotificationConflict) {
		t.Fatalf("collision accepted: %v", err)
	}
	f.fx.Exec(t, "UPDATE notification_event SET route_owner='legacy',state='legacy_owned' WHERE id=$1", id)
	if replay, err := f.s.CaptureShadow(ctx, f.w, f.u, f.a, f.in); err != nil || replay.ID != id || replay.RouteOwner != "legacy" {
		t.Fatalf("legacy retry changed owner: %+v %v", replay, err)
	}
	if _, err := f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, id); err == nil {
		t.Fatal("legacy event authorized by shadow route")
	}
}

func TestNotificationEventAckStoreFailureAndStaleSource(t *testing.T) {
	f := newNotificationFixture(t)
	ctx := context.Background()
	e := f.capture(t)
	f.s.readAwareness = func(ctx context.Context, tx pgx.Tx, q *db.Queries, p db.GetNotificationAwarenessParams) (db.NotificationAwareness, error) {
		// A real SQL error aborts the transaction until the service rolls back
		// its savepoint; a synthetic Go error alone would miss that regression.
		_, err := tx.Exec(ctx, "SELECT 1/0")
		return db.NotificationAwareness{}, err
	}
	d, err := f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, e.ID)
	if err != nil || d.Verdict != NotificationDefer {
		t.Fatalf("first store failure: %+v %v", d, err)
	}
	f.now = f.now.Add(time.Minute)
	d, err = f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, e.ID)
	if err != nil || d.Reason != "ack_store_unavailable_fail_open" {
		t.Fatalf("bounded retry: %+v %v", d, err)
	}
	f.s.readAwareness = nil
	f.now = f.now.Add(time.Hour)
	d, err = f.s.AuthorizeShadow(ctx, f.w, f.u, f.a, e.ID)
	if err != nil || d.Reason != "source_uncertain_use_historical_wording" {
		t.Fatalf("stale: %+v %v", d, err)
	}
}
