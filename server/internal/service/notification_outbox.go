package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// These interfaces reuse channel transports without granting a producer access
// to credentials or letting it select a destination.
var ErrNotificationNotSend = errors.New("notification gate deferred or suppressed delivery")

type NotificationTransport interface {
	Prepare(context.Context, TaskStatusNotificationRoute) (NotificationSender, error)
}
type NotificationSender interface {
	Chunks(string) []string
	Send(context.Context, string) NotificationSendResult
}
type NotificationSendResult struct {
	MessageID string
	// Only a proved rejection can be retried. Every unclassified failure is unknown.
	Rejected   bool
	RetryAfter time.Duration
	Err        error
}

// QueueDirect is invoked only by the explicitly enabled bounded worker.
// Activation needs a separately approved topic route. Existing issue-result
// consent cannot authorize this producer. Canonical message and outbox are atomic.
func (s *NotificationEventService) QueueDirect(ctx context.Context, workspace, event pgtype.UUID, expectedRevision int64) (db.NotificationOutbox, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	e, err := q.GetNotificationEvent(ctx, db.GetNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	if err = q.LockNotificationScope(ctx, notificationScope(e)); err != nil {
		return db.NotificationOutbox{}, err
	}
	e, err = q.LockNotificationEvent(ctx, db.LockNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	if e.Revision != expectedRevision || e.RouteOwner != "shadow" {
		return db.NotificationOutbox{}, ErrNotificationConflict
	}
	topic, err := q.GetNotificationTopicRoute(ctx, db.GetNotificationTopicRouteParams{WorkspaceID: workspace, UserID: e.UserID, AgentID: e.AgentID, Producer: e.Producer})
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	if !topic.Enabled || topic.RouteOwner != "direct" || (topic.EligibleAfter.InfinityModifier == pgtype.Finite && e.CreatedAt.Time.Before(topic.EligibleAfter.Time)) {
		return db.NotificationOutbox{}, routeRefused("topic_not_authorized")
	}
	route, err := ResolveTaskStatusNotificationRoute(ctx, q, workspace, e.UserID, e.AgentID, topic.InstallationID, true)
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	var input NotificationEventInput
	if err = json.Unmarshal(e.Payload, &input); err != nil {
		return db.NotificationOutbox{}, err
	}
	text, err := RenderQuotaNotification(input)
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	d, err := s.evaluate(ctx, tx, e, "queued")
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	if d.Verdict != NotificationSend {
		if err = tx.Commit(ctx); err != nil {
			return db.NotificationOutbox{}, err
		}
		return db.NotificationOutbox{}, ErrNotificationNotSend
	}
	expectedRevision++
	n, err := q.SetNotificationRouteOwner(ctx, db.SetNotificationRouteOwnerParams{WorkspaceID: workspace, ID: event, ExpectedRevision: expectedRevision, ExpectedOwner: "shadow", RouteOwner: "direct"})
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	if n != 1 {
		return db.NotificationOutbox{}, ErrNotificationConflict
	}
	if err = q.AppendNotificationDecisionAudit(ctx, db.AppendNotificationDecisionAuditParams{ID: dbid.NewV7(), WorkspaceID: workspace, EventID: event, Verdict: string(NotificationDefer), Reason: "route_owner:shadow_to_direct", EventRevision: expectedRevision + 1, Phase: "capture"}); err != nil {
		return db.NotificationOutbox{}, err
	}
	msg, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: route.Session.ID, Role: "assistant", Content: text, ChannelIngested: pgtype.Bool{Bool: true, Valid: true}})
	if err != nil {
		return db.NotificationOutbox{}, err
	}
	out, err := q.InsertNotificationOutbox(ctx, db.InsertNotificationOutboxParams{EventID: event, WorkspaceID: workspace, RouteID: topic.ID, RouteRevision: topic.Revision, InstallationID: topic.InstallationID, ChatSessionID: route.Session.ID, ChatMessageID: msg.ID, ChannelChatID: route.UserBinding.ChannelUserID})
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// DeliverDirect reserves durably, then holds the ack scope lock from final gate
// through the bounded network call. An ack committed before that gate suppresses
// the call. A later ack waits; a request already accepted by Telegram is irreversible.
// A crash after reservation leaves an expiring unknown, never an automatic resend.
func (s *NotificationEventService) DeliverDirect(ctx context.Context, workspace, event pgtype.UUID, transport NotificationTransport) error {
	if transport == nil {
		return errors.New("notification transport unavailable")
	}
	qpool := db.New(s.Pool)
	if err := qpool.ExpireNotificationOutbox(ctx); err != nil {
		return err
	}
	out, err := qpool.ReserveNotificationOutbox(ctx, db.ReserveNotificationOutboxParams{WorkspaceID: workspace, EventID: event, LeaseToken: dbid.NewV7()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Reserve budget covers the gate, all chunks and recording. No hidden retries.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	finish := func(state, reason string, retry *time.Time) error {
		if err := s.finishOutbox(ctx, q, out, state, reason, retry); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	e, err := q.GetNotificationEvent(ctx, db.GetNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return err
	}
	if err = q.LockNotificationScope(ctx, notificationScope(e)); err != nil {
		return err
	}
	e, err = q.LockNotificationEvent(ctx, db.LockNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return err
	}
	if e.RouteOwner != "direct" {
		return finish("failed", "route_owner_changed", nil)
	}
	topic, err := q.GetNotificationTopicRoute(ctx, db.GetNotificationTopicRouteParams{WorkspaceID: workspace, UserID: e.UserID, AgentID: e.AgentID, Producer: e.Producer})
	if errors.Is(err, pgx.ErrNoRows) {
		return finish("failed", "topic_route_missing", nil)
	}
	if err != nil {
		return err
	}
	if !topic.Enabled || topic.RouteOwner != "direct" || topic.ID != out.RouteID || topic.Revision != out.RouteRevision || topic.InstallationID != out.InstallationID {
		return finish("failed", "topic_route_changed", nil)
	}
	route, err := ResolveTaskStatusNotificationRoute(ctx, q, workspace, e.UserID, e.AgentID, out.InstallationID, true)
	if err != nil {
		var refused *TaskStatusNotificationRouteError
		if errors.As(err, &refused) {
			return finish("failed", refused.Reason, nil)
		}
		return err
	}
	if route.Session.ID != out.ChatSessionID || route.UserBinding.ChannelUserID != out.ChannelChatID {
		return finish("failed", "destination_changed", nil)
	}
	// Re-read the reservation after waiting for any competing ack/sender.
	current, err := q.GetNotificationOutbox(ctx, db.GetNotificationOutboxParams{WorkspaceID: workspace, EventID: event})
	if err != nil {
		return err
	}
	if current.State != "sending" || current.LeaseToken != out.LeaseToken || !time.Now().Before(current.LeaseUntil.Time) {
		return errors.New("notification lease lost")
	}
	d, err := s.evaluate(ctx, tx, e, "reserved")
	if err != nil {
		return err
	}
	if d.Verdict != NotificationSend {
		state := "suppressed"
		if d.Verdict == NotificationDefer {
			state = "pending"
		}
		if err = s.finishOutbox(ctx, q, out, state, d.Reason, d.RetryAt); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	msg, err := q.GetChatMessage(ctx, out.ChatMessageID)
	if err != nil {
		return err
	}
	if msg.ChatSessionID != out.ChatSessionID || msg.Role != "assistant" {
		return errors.New("canonical message mismatch")
	}
	sender, err := transport.Prepare(ctx, route)
	if err != nil {
		at := s.now().Add(30 * time.Second)
		return finish("pending", "transport_prepare_failed", &at)
	}
	chunks := sender.Chunks(msg.Content)
	if len(chunks) == 0 {
		return finish("failed", "empty_message", nil)
	}
	for i := len(out.MessageIds); i < len(chunks); i++ {
		result := sender.Send(ctx, chunks[i])
		if result.Err != nil || result.MessageID == "" {
			state := "unknown"
			reason := "transport outcome unknown"
			var retry *time.Time
			if result.Err != nil {
				reason = result.Err.Error()
			}
			if result.Rejected {
				state = "failed"
				if result.RetryAfter > 0 {
					state = "pending"
					at := s.now().Add(result.RetryAfter)
					retry = &at
				}
			}
			if err = s.finishOutbox(ctx, q, out, state, reason, retry); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		// A chunk ledger is durable even if the gate transaction later rolls back.
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		rows, recordErr := qpool.RecordNotificationOutboxChunk(recordCtx, db.RecordNotificationOutboxChunkParams{WorkspaceID: workspace, EventID: event, LeaseToken: out.LeaseToken, MessageID: result.MessageID, ChunkIndex: int32(i)})
		recordCancel()
		if recordErr != nil || rows != 1 {
			return fmt.Errorf("record chunk (send outcome unknown): %w", recordErr)
		}
		out.MessageIds = append(out.MessageIds, result.MessageID)
	}
	if err = q.SetChatMessageChannelOutbound(ctx, db.SetChatMessageChannelOutboundParams{ID: out.ChatMessageID, ChannelOutboundType: pgtype.Text{String: "telegram", Valid: true}, ChannelOutboundInstallationID: out.InstallationID, ChannelOutboundChatID: pgtype.Text{String: out.ChannelChatID, Valid: true}, ChannelOutboundMessageIds: out.MessageIds}); err != nil {
		return err
	}
	if err = s.finishOutbox(ctx, q, out, "sent", "", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *NotificationEventService) finishOutbox(ctx context.Context, q *db.Queries, out db.NotificationOutbox, state, reason string, retry *time.Time) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	at := s.now()
	if retry != nil {
		at = *retry
	}
	rows, err := q.SetNotificationOutboxOutcome(recordCtx, db.SetNotificationOutboxOutcomeParams{WorkspaceID: out.WorkspaceID, EventID: out.EventID, LeaseToken: out.LeaseToken, State: state, LastError: pgtype.Text{String: reason, Valid: reason != ""}, NextAttemptAt: notificationTime(at), MessageIds: out.MessageIds})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("notification lease lost recording outcome")
	}
	e, err := q.GetNotificationEvent(recordCtx, db.GetNotificationEventParams{WorkspaceID: out.WorkspaceID, ID: out.EventID})
	if err != nil {
		return err
	}
	eventState := state
	verdict := NotificationSend
	if state == "pending" {
		eventState = "deferred"
		verdict = NotificationDefer
	}
	if state == "suppressed" {
		verdict = NotificationSuppress
	}
	updated, err := q.RecordNotificationDecision(recordCtx, db.RecordNotificationDecisionParams{WorkspaceID: e.WorkspaceID, ID: e.ID, ExpectedRevision: e.Revision, State: eventState, DecisionReason: pgtype.Text{String: "transport_" + state + ":" + reason, Valid: true}, AckMessageID: e.AckMessageID, RetryAt: notificationTime(at)})
	if err != nil {
		return err
	}
	return q.AppendNotificationDecisionAudit(recordCtx, db.AppendNotificationDecisionAuditParams{ID: dbid.NewV7(), WorkspaceID: e.WorkspaceID, EventID: e.ID, Verdict: string(verdict), Reason: "transport_" + state + ":" + reason, AckMessageID: e.AckMessageID, EventRevision: updated.Revision, Phase: "authorize"})
}

// RollbackDirect only returns unsent pending rows to shadow. Unknown/sent rows
// stay owned by direct and need reconciliation; rollback cannot duplicate them.
func (s *NotificationEventService) RollbackDirect(ctx context.Context, workspace, event pgtype.UUID) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	e, err := q.GetNotificationEvent(ctx, db.GetNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return err
	}
	if err = q.LockNotificationScope(ctx, notificationScope(e)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM chat_message WHERE id IN (SELECT chat_message_id FROM notification_outbox WHERE workspace_id=$1 AND event_id=$2 AND state='pending' AND cardinality(message_ids)=0)`, workspace, event)
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `DELETE FROM notification_outbox WHERE workspace_id=$1 AND event_id=$2 AND state='pending' AND cardinality(message_ids)=0`, workspace, event)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotificationConflict
	}
	_, err = tx.Exec(ctx, `UPDATE notification_event SET route_owner='shadow',state='queued',revision=revision+1 WHERE workspace_id=$1 AND id=$2 AND route_owner='direct'`, workspace, event)
	if err != nil {
		return err
	}
	current, err := q.GetNotificationEvent(ctx, db.GetNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return err
	}
	if err = q.AppendNotificationDecisionAudit(ctx, db.AppendNotificationDecisionAuditParams{ID: dbid.NewV7(), WorkspaceID: workspace, EventID: event, Verdict: string(NotificationDefer), Reason: "route_owner:direct_to_shadow", EventRevision: current.Revision, Phase: "capture"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
