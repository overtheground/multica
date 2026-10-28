package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type NotificationTopicInput struct {
	InstallationID   pgtype.UUID
	ExpectedRevision int64
	Enabled          bool
	Owner            string
	// Operator attestation, not a server claim that an external monitor stopped.
	LegacyStopped bool
}

// ConfigureTopic is called only for the authenticated human's own recipient.
// Missing routes have revision0. A direct activation starts a new eligibility
// interval; no old shadow backlog is silently promoted into external delivery.
func (s *NotificationEventService) ConfigureTopic(ctx context.Context, workspace, user, agent pgtype.UUID, producer string, in NotificationTopicInput) (db.NotificationTopicRoute, error) {
	if producer != "subscription-limits" || (in.Owner != "shadow" && in.Owner != "legacy" && in.Owner != "direct") || !in.InstallationID.Valid || in.ExpectedRevision < 0 {
		return db.NotificationTopicRoute{}, ErrNotificationInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return db.NotificationTopicRoute{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	// Route access is checked even for a disabled topic; no arbitrary installation
	// identity may be parked here for a future worker to trust.
	if _, err = ResolveTaskStatusNotificationRoute(ctx, q, workspace, user, agent, in.InstallationID, true); err != nil {
		return db.NotificationTopicRoute{}, err
	}
	current, err := q.GetNotificationTopicRouteForUpdate(ctx, db.GetNotificationTopicRouteForUpdateParams{WorkspaceID: workspace, UserID: user, AgentID: agent, Producer: producer})
	previous := "absent"
	if errors.Is(err, pgx.ErrNoRows) {
		if in.ExpectedRevision != 0 {
			return current, ErrNotificationConflict
		}
		current, err = q.CreateNotificationTopicRoute(ctx, db.CreateNotificationTopicRouteParams{ID: dbid.NewV7(), WorkspaceID: workspace, UserID: user, AgentID: agent, Producer: producer, InstallationID: in.InstallationID})
		if errors.Is(err, pgx.ErrNoRows) {
			return current, ErrNotificationConflict
		}
		if err != nil {
			return current, err
		}
	} else {
		if err != nil {
			return current, err
		}
		if current.Revision != in.ExpectedRevision {
			return current, ErrNotificationConflict
		}
		previous = current.RouteOwner
	}
	if in.Owner == "legacy" {
		unsettled, err := q.CountNotificationDirectUnsettled(ctx, db.CountNotificationDirectUnsettledParams{WorkspaceID: workspace, UserID: user, AgentID: agent, Producer: producer})
		if err != nil {
			return current, err
		}
		if unsettled != 0 {
			return current, routeRefused("direct_delivery_requires_reconciliation")
		}
	}
	activating := in.Enabled && in.Owner == "direct" && (!current.Enabled || current.RouteOwner != "direct" || current.InstallationID != in.InstallationID)
	cutoff := current.EligibleAfter
	if activating {
		if !in.LegacyStopped {
			return current, routeRefused("legacy_stop_confirmation_required")
		}
		pending, err := q.CountNotificationLegacyInFlight(ctx, db.CountNotificationLegacyInFlightParams{WorkspaceID: workspace, UserID: user, AgentID: agent})
		if err != nil {
			return current, err
		}
		if pending != 0 {
			return current, routeRefused("legacy_delivery_not_drained")
		}
		cutoff = notificationTime(s.now())
	}
	updated, err := q.UpdateNotificationTopicRoute(ctx, db.UpdateNotificationTopicRouteParams{WorkspaceID: workspace, ID: current.ID, ExpectedRevision: current.Revision, InstallationID: in.InstallationID, Enabled: in.Enabled, RouteOwner: in.Owner, EligibleAfter: cutoff})
	if errors.Is(err, pgx.ErrNoRows) {
		return current, ErrNotificationConflict
	}
	if err != nil {
		return updated, err
	}
	reason := "human_topic_configuration"
	if activating {
		reason = "human_confirmed_legacy_stopped_and_db_drained"
	}
	err = q.AppendNotificationTopicRouteAudit(ctx, db.AppendNotificationTopicRouteAuditParams{ID: dbid.NewV7(), WorkspaceID: workspace, RouteID: updated.ID, ActorUserID: user, Revision: updated.Revision, PreviousOwner: previous, RouteOwner: updated.RouteOwner, Enabled: updated.Enabled, Reason: reason})
	if err != nil {
		return updated, err
	}
	return updated, tx.Commit(ctx)
}

// NotificationEventWorker has bounded, sequential attempts. Zero-value workers
// do no work, including database scans. Register only behind an explicit flag.
type NotificationEventWorker struct {
	Events    *NotificationEventService
	Transport NotificationTransport
	Enabled   bool
	BatchSize int32
}

func (w *NotificationEventWorker) Tick(ctx context.Context) (int, error) {
	if !w.Enabled {
		return 0, nil
	}
	if w.Events == nil || w.Transport == nil {
		return 0, errors.New("notification worker dependencies missing")
	}
	batch := w.BatchSize
	if batch <= 0 || batch > 20 {
		batch = 20
	}
	q := db.New(w.Events.Pool)
	if err := q.ExpireNotificationOutbox(ctx); err != nil {
		return 0, err
	}
	candidates, err := q.ListNotificationShadowCandidates(ctx, batch)
	if err != nil {
		return 0, err
	}
	for _, e := range candidates {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		_, err := w.Events.QueueDirect(ctx, e.WorkspaceID, e.ID, e.Revision)
		if err != nil && !errors.Is(err, ErrNotificationNotSend) && !errors.Is(err, ErrNotificationConflict) {
			return 0, fmt.Errorf("queue structured notification: %w", err)
		}
	}
	due, err := q.ListNotificationOutboxDue(ctx, batch)
	if err != nil {
		return 0, err
	}
	attempted := 0
	for _, out := range due {
		if ctx.Err() != nil {
			return attempted, ctx.Err()
		}
		if err := w.Events.DeliverDirect(ctx, out.WorkspaceID, out.EventID, w.Transport); err != nil {
			return attempted, err
		}
		attempted++
	}
	return attempted, nil
}
func (w *NotificationEventWorker) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if !w.Enabled {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
