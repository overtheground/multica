package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

var ErrNotificationInvalid = errors.New("invalid notification input")
var ErrNotificationConflict = errors.New("notification event identity conflict")
var ErrNotificationAckSource = errors.New("notification acknowledgement requires an explicit member message in the bound chat")

// NotificationEventService never calls a transport. Shadow authorization is a
// durable decision, not a visible chat message or an issue lifecycle transition.
type NotificationEventDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type NotificationEventService struct {
	Pool          NotificationEventDB
	Now           func() time.Time
	readAwareness func(context.Context, pgx.Tx, *db.Queries, db.GetNotificationAwarenessParams) (db.NotificationAwareness, error)
}

func (s *NotificationEventService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

type NotificationEventInput struct {
	EventKey string               `json:"event_key"`
	Producer string               `json:"producer"`
	Facts    NotificationFacts    `json:"facts"`
	Claude   *ClaudeQuotaSnapshot `json:"claude,omitempty"`
}

func notificationTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// CaptureShadow is intentionally the only producer ingress in this phase.
// Identity and recipient come from the authenticated handler, never the payload.
func (s *NotificationEventService) CaptureShadow(ctx context.Context, workspace, user, agent pgtype.UUID, in NotificationEventInput) (db.NotificationEvent, error) {
	if err := in.Facts.Validate(s.now()); err != nil {
		return db.NotificationEvent{}, fmt.Errorf("%w: %v", ErrNotificationInvalid, err)
	}
	if in.Claude != nil {
		if _, err := RenderQuotaNotification(in); err != nil {
			return db.NotificationEvent{}, fmt.Errorf("%w: %v", ErrNotificationInvalid, err)
		}
	}
	if len(in.EventKey) == 0 || len(in.EventKey) > 200 || in.Producer != "subscription-limits" || in.Facts.ExplicitReminder {
		return db.NotificationEvent{}, fmt.Errorf("%w: invalid producer/key or untrusted schedule override", ErrNotificationInvalid)
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return db.NotificationEvent{}, err
	}
	row, err := db.New(s.Pool).CaptureNotificationEvent(ctx, db.CaptureNotificationEventParams{
		ID: dbid.NewV7(), WorkspaceID: workspace, UserID: user, AgentID: agent, Producer: in.Producer, EventKey: in.EventKey, Payload: payload,
		Subject: in.Facts.Subject, Period: in.Facts.Period, Action: in.Facts.Action, ObservedAt: notificationTime(in.Facts.ObservedAt),
		ResetAt: notificationTime(in.Facts.ResetAt), FreshUntil: notificationTime(in.Facts.FreshUntil), Urgency: in.Facts.Urgency, RouteOwner: "shadow",
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotificationConflict
	}
	return row, err
}

func notificationScope(e db.NotificationEvent) string {
	key, _ := json.Marshal([]string{util.UUIDToString(e.WorkspaceID), util.UUIDToString(e.UserID), util.UUIDToString(e.AgentID), e.Subject, e.Period})
	return string(key)
}

// Acknowledge checks stored user provenance rather than accepting role/author
// claims from a model. Only an explicit event ID or a complete subject/window/
// reset statement is accepted; vague awareness is not a global mute.
func (s *NotificationEventService) Acknowledge(ctx context.Context, workspace, user, agent, event, session, message pgtype.UUID) error {
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
	if e.UserID != user || e.AgentID != agent {
		return ErrNotificationAckSource
	}
	if err = q.LockNotificationScope(ctx, notificationScope(e)); err != nil {
		return err
	}
	m, err := q.GetNotificationAckSource(ctx, db.GetNotificationAckSourceParams{WorkspaceID: workspace, UserID: user, AgentID: agent, MessageID: message, SessionID: session})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotificationAckSource
		}
		return err
	}
	text := strings.TrimSpace(m.Content)
	if !matchesNotificationAck(text, e) {
		return ErrNotificationAckSource
	}
	now := s.now()
	if !now.Before(e.ResetAt.Time) || m.CreatedAt.Time.After(now) || !m.CreatedAt.Time.Before(e.ResetAt.Time) || m.CreatedAt.Time.Before(e.ObservedAt.Time) {
		return ErrNotificationAckSource
	}
	_, err = q.UpsertNotificationAwareness(ctx, db.UpsertNotificationAwarenessParams{
		ID: dbid.NewV7(), WorkspaceID: workspace, UserID: user, AgentID: agent, Subject: e.Subject, Period: e.Period, Action: e.Action,
		SourceMessageID: message, SourceSessionID: session, OccurredAt: m.CreatedAt, ExpiresAt: e.ResetAt,
	})
	// An equal/newer verified acknowledgement already exists; replay is a no-op.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return tx.Commit(ctx)
}

// AuthorizeShadow re-reads awareness under the same scope lock as ack writes.
// A shadow_send is terminal to workers, but can be re-evaluated explicitly so
// shadow replay exercises an ack that arrived after an earlier decision.
func (s *NotificationEventService) AuthorizeShadow(ctx context.Context, workspace, user, agent, event pgtype.UUID) (NotificationDecision, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NotificationDecision{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	e, err := q.GetNotificationEvent(ctx, db.GetNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return NotificationDecision{}, err
	}
	if e.UserID != user || e.AgentID != agent {
		return NotificationDecision{}, ErrNotificationAckSource
	}
	if err = q.LockNotificationScope(ctx, notificationScope(e)); err != nil {
		return NotificationDecision{}, err
	}
	e, err = q.LockNotificationEvent(ctx, db.LockNotificationEventParams{WorkspaceID: workspace, ID: event})
	if err != nil {
		return NotificationDecision{}, err
	}
	if e.RouteOwner != "shadow" {
		return NotificationDecision{}, fmt.Errorf("event owned by %s route", e.RouteOwner)
	}
	if e.State != "queued" && e.State != "deferred" && e.State != "shadow_send" {
		return NotificationDecision{Verdict: NotificationSuppress, Reason: "already_decided:" + e.State}, nil
	}
	d, err := s.evaluate(ctx, tx, e, "shadow_send")
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}

// evaluate runs under the event's scope lock; callers choose only the send state.
func (s *NotificationEventService) evaluate(ctx context.Context, tx pgx.Tx, e db.NotificationEvent, sendState string) (NotificationDecision, error) {
	q := db.New(tx)
	var err error
	now := s.now()
	if e.RetryAt.Valid && now.Before(e.RetryAt.Time) {
		retry := e.RetryAt.Time
		return NotificationDecision{Verdict: NotificationDefer, Reason: "retry_not_due", RetryAt: &retry}, nil
	}
	if _, err = tx.Exec(ctx, "SAVEPOINT awareness_read"); err != nil {
		return NotificationDecision{}, err
	}
	reader := s.readAwareness
	if reader == nil {
		reader = func(ctx context.Context, _ pgx.Tx, q *db.Queries, p db.GetNotificationAwarenessParams) (db.NotificationAwareness, error) {
			return q.GetNotificationAwareness(ctx, p)
		}
	}
	a, readErr := reader(ctx, tx, q, db.GetNotificationAwarenessParams{WorkspaceID: e.WorkspaceID, UserID: e.UserID, AgentID: e.AgentID, Subject: e.Subject, Period: e.Period})
	readFailed := readErr != nil && !errors.Is(readErr, pgx.ErrNoRows)
	firstFailure := e.FirstAckFailureAt
	if readFailed {
		// Recover the transaction before recording defer/fail-open. If the
		// connection itself failed, no authorization may escape this method.
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT awareness_read"); err != nil {
			return NotificationDecision{}, err
		}
		if !firstFailure.Valid {
			firstFailure = notificationTime(now)
		}
	}
	if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT awareness_read"); err != nil {
		return NotificationDecision{}, err
	}
	var awareness *NotificationAwareness
	if readErr == nil {
		awareness = &NotificationAwareness{Subject: a.Subject, Period: a.Period, Action: a.Action, OccurredAt: a.OccurredAt.Time, ExpiresAt: a.ExpiresAt.Time, MessageID: util.UUIDToString(a.SourceMessageID)}
	}
	f := NotificationFacts{Subject: e.Subject, Period: e.Period, Action: e.Action, ObservedAt: e.ObservedAt.Time, ResetAt: e.ResetAt.Time, FreshUntil: e.FreshUntil.Time, Urgency: e.Urgency}
	d := DecideNotification(f, awareness, readFailed, firstFailure.Time, now)
	state := sendState
	if d.Verdict == NotificationSuppress {
		state = "suppressed"
	}
	if d.Verdict == NotificationDefer {
		state = "deferred"
	}
	ackID := pgtype.UUID{}
	if d.AckMessageID != "" {
		ackID, err = util.ParseUUID(d.AckMessageID)
		if err != nil {
			return NotificationDecision{}, err
		}
	}
	retry := pgtype.Timestamptz{}
	if d.RetryAt != nil {
		retry = notificationTime(*d.RetryAt)
	}
	updated, err := q.RecordNotificationDecision(ctx, db.RecordNotificationDecisionParams{WorkspaceID: e.WorkspaceID, ID: e.ID, ExpectedRevision: e.Revision, State: state, DecisionReason: pgtype.Text{String: d.Reason, Valid: true}, AckMessageID: ackID, RetryAt: retry, FirstAckFailureAt: firstFailure})
	if err != nil {
		return NotificationDecision{}, err
	}
	err = q.AppendNotificationDecisionAudit(ctx, db.AppendNotificationDecisionAuditParams{ID: dbid.NewV7(), WorkspaceID: e.WorkspaceID, EventID: e.ID, Verdict: string(d.Verdict), Reason: d.Reason, AckMessageID: ackID, EventRevision: updated.Revision, Phase: "authorize"})
	if err != nil {
		return NotificationDecision{}, err
	}
	return d, nil
}
