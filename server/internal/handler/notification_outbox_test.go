package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/telegram"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestNotificationDirectOutboxFakeTelegram(t *testing.T) {
	for _, scenario := range []string{"ack_before_send", "send", "unknown", "rejected", "retry_then_ack", "concurrent_senders", "rollback", "changed_consent", "abandoned", "partial_retry", "ack_during_send"} {
		t.Run(scenario, func(t *testing.T) {
			e := newTSNEnv(t)
			e.inbound("hello", 0)
			session := e.sessionID()
			s := &service.NotificationEventService{Pool: testPool}
			now := time.Now().UTC().Truncate(time.Second)
			zero, twenty := 0.0, 20.0
			input := service.NotificationEventInput{Producer: "subscription-limits", EventKey: "quota-1", Facts: service.NotificationFacts{Subject: "claude/account/five_hour", Period: now.Add(time.Hour).Format(time.RFC3339), Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), FreshUntil: now.Add(time.Minute), Urgency: "routine"}, Claude: &service.ClaudeQuotaSnapshot{FiveHour: service.QuotaWindow{RemainingPercent: &zero, ResetAt: now.Add(time.Hour)}, SevenDay: service.QuotaWindow{RemainingPercent: &twenty, ResetAt: now.Add(24 * time.Hour)}}}
			w, u, a := parseUUID(e.workspaceID), parseUUID(e.userID), parseUUID(e.mikaID)
			event, err := s.CaptureShadow(e.ctx, w, u, a, input)
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"notification_outbox", "notification_topic_route", "notification_decision_audit", "notification_awareness", "notification_event"} {
				e.fx.Cleanup(t, "DELETE FROM "+table+" WHERE workspace_id=$1", e.workspaceID)
			}
			// A normal issue-result subscription is not event-topic consent.
			if _, err = s.QueueDirect(e.ctx, w, event.ID, event.Revision); err == nil {
				t.Fatal("queued without topic consent")
			}
			e.fx.Insert(t, "notification_topic_route", testutil.Cols{"id": dbid.NewV7(), "workspace_id": e.workspaceID, "user_id": e.userID, "agent_id": e.mikaID, "producer": "subscription-limits", "installation_id": e.installationID, "enabled": true, "route_owner": "direct"})
			out, err := s.QueueDirect(e.ctx, w, event.ID, event.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.QueueDirect(e.ctx, w, event.ID, event.Revision); !errors.Is(err, service.ErrNotificationConflict) {
				t.Fatalf("duplicate cutover: %v", err)
			}
			ack := func() {
				id := e.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": session, "role": "user", "message_kind": "message", "content": "я в курсе, лимиты Claude account на 5 часов исчерпаны до " + input.Facts.ResetAt.Format(time.RFC3339)})
				if err := s.Acknowledge(e.ctx, w, u, a, event.ID, parseUUID(session), parseUUID(id)); err != nil {
					t.Fatal(err)
				}
			}
			transport := &telegram.EventTransport{APIBase: e.bot.server.URL, Client: e.bot.server.Client()}
			deliver := func() error { return s.DeliverDirect(e.ctx, w, event.ID, transport) }
			want := "sent"
			calls := 1
			switch scenario {
			case "ack_before_send":
				ack()
				want = "suppressed"
				calls = 0
			case "unknown":
				e.bot.script(fakeBotReply{503, `{"ok":false,"error_code":503,"description":"lost result"}`})
				want = "unknown"
				calls = 0
			case "rejected":
				e.bot.script(fakeBotReply{403, `{"ok":false,"error_code":403,"description":"blocked"}`})
				want = "failed"
				calls = 0
			case "changed_consent":
				e.fx.Exec(t, "UPDATE notification_topic_route SET enabled=false,revision=revision+1 WHERE workspace_id=$1", e.workspaceID)
				want = "failed"
				calls = 0
			case "abandoned":
				e.fx.Exec(t, "UPDATE notification_outbox SET state='sending',lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", event.ID)
				want = "unknown"
				calls = 0
			case "rollback":
				if err = s.RollbackDirect(e.ctx, w, event.ID); err != nil {
					t.Fatal(err)
				}
				row, err := e.q.GetNotificationEvent(e.ctx, db.GetNotificationEventParams{WorkspaceID: w, ID: event.ID})
				if err != nil || row.RouteOwner != "shadow" {
					t.Fatalf("rollback: %+v %v", row, err)
				}
				return
			case "retry_then_ack", "partial_retry":
				if scenario == "partial_retry" {
					e.fx.Exec(t, "UPDATE chat_message SET content=$2 WHERE id=$1", out.ChatMessageID, strings.Repeat("quota snapshot ", 600))
					e.bot.script(fakeBotReply{200, ""}, fakeBotReply{429, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`})
				} else {
					e.bot.script(fakeBotReply{429, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`})
				}
				if err = deliver(); err != nil {
					t.Fatal(err)
				}
				row, err := e.q.GetNotificationOutbox(e.ctx, db.GetNotificationOutboxParams{WorkspaceID: w, EventID: event.ID})
				if err != nil || row.State != "pending" {
					t.Fatalf("retry: %+v %v", row, err)
				}
				if scenario == "retry_then_ack" {
					ack()
					want = "suppressed"
					calls = 0
				} else {
					calls = 3
				}
				e.fx.Exec(t, "UPDATE notification_outbox SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE event_id=$1", event.ID)
				e.fx.Exec(t, "UPDATE notification_event SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", event.ID)
			}
			if scenario == "ack_during_send" {
				entered, release := make(chan struct{}), make(chan struct{})
				paused := &pausedEventTransport{base: transport, entered: entered, release: release}
				done := make(chan error, 1)
				go func() { done <- s.DeliverDirect(e.ctx, w, event.ID, paused) }()
				<-entered
				// The sender holds exactly the same database lock used by acknowledgements.
				key, _ := json.Marshal([]string{e.workspaceID, e.userID, e.mikaID, event.Subject, event.Period})
				var acquired bool
				if err = testPool.QueryRow(e.ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1::text,0))", string(key)).Scan(&acquired); err != nil || acquired {
					t.Fatalf("send did not hold ack gate: %v %v", acquired, err)
				}
				id := e.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": session, "role": "user", "message_kind": "message", "content": "ack " + uuidToString(event.ID)})
				ackDone := make(chan error, 1)
				go func() { ackDone <- s.Acknowledge(e.ctx, w, u, a, event.ID, parseUUID(session), parseUUID(id)) }()
				close(release)
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				if err = <-ackDone; err != nil {
					t.Fatal(err)
				}
			} else if scenario == "concurrent_senders" {
				var wg sync.WaitGroup
				errs := make(chan error, 8)
				for range 8 {
					wg.Add(1)
					go func() { defer wg.Done(); errs <- deliver() }()
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
			} else if err = deliver(); err != nil {
				t.Fatal(err)
			}
			row, err := e.q.GetNotificationOutbox(e.ctx, db.GetNotificationOutboxParams{WorkspaceID: w, EventID: event.ID})
			if err != nil || row.State != want {
				t.Fatalf("state %s want %s: %v", row.State, want, err)
			}
			if err = deliver(); err != nil {
				t.Fatal(err)
			}
			if got := len(e.bot.sentMessages()); got != calls {
				t.Fatalf("accepted calls %d want %d", got, calls)
			}
			if want == "sent" && scenario != "partial_retry" {
				body := fmt.Sprint(e.bot.sentMessages()[0]["text"])
				if !strings.Contains(body, "5 часов") || !strings.Contains(body, "неделя") || !strings.Contains(body, "20%") {
					t.Fatalf("missing windows: %s", body)
				}
				msg, err := e.q.GetChatMessage(context.Background(), out.ChatMessageID)
				if err != nil || len(msg.ChannelOutboundMessageIds) != 1 {
					t.Fatalf("canonical delivery missing: %+v %v", msg, err)
				}
			}
			if want == "unknown" || want == "sent" {
				if err = s.RollbackDirect(e.ctx, w, event.ID); !errors.Is(err, service.ErrNotificationConflict) {
					t.Fatalf("unsafe rollback: %v", err)
				}
			}
			audit, err := e.q.ListNotificationDecisionAudit(e.ctx, db.ListNotificationDecisionAuditParams{WorkspaceID: w, EventID: event.ID})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("outbox=%s chunks=%v audit=%+v", row.State, row.MessageIds, audit)
		})
	}
}

// Pause a real fake-Telegram sender inside its final authorization interval.
type pausedEventTransport struct {
	base             service.NotificationTransport
	entered, release chan struct{}
}

func (p *pausedEventTransport) Prepare(ctx context.Context, r service.TaskStatusNotificationRoute) (service.NotificationSender, error) {
	sender, err := p.base.Prepare(ctx, r)
	return &pausedEventSender{NotificationSender: sender, p: p}, err
}

type pausedEventSender struct {
	service.NotificationSender
	p *pausedEventTransport
}

func (s *pausedEventSender) Send(ctx context.Context, text string) service.NotificationSendResult {
	close(s.p.entered)
	select {
	case <-s.p.release:
		return s.NotificationSender.Send(ctx, text)
	case <-ctx.Done():
		return service.NotificationSendResult{Err: ctx.Err()}
	}
}
