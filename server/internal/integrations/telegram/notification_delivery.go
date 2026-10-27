package telegram

// notification_delivery.go — the outbox worker for task-status notifications
// (service/task_status_notification.go). The agent's reply is already the
// canonical chat message when a row reaches 'pending'; this worker moves
// its text and bound files into the member's private chat. The text outbox
// state and per-file Bot API outcomes are recorded separately.
//
// The rules are the reply path's (delivery.go), applied to a durable row:
//
//   - The send is published ('sending' with a lease) before Telegram is
//     called, so a process that dies mid-call leaves evidence, not silence.
//   - A refused send (4xx) is final; a 429 is deferred by Telegram's
//     retry_after and resumes from the first chunk not yet delivered.
//   - A send whose result was lost (5xx, timeout, connection reset) ends in
//     'unknown' and is never retried: sendMessage takes no idempotency key.
//   - Every chunk's message id is recorded against the canonical chat message
//     the moment Telegram returns it, so a reply to any part resolves.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// NotificationDelivery sends composed task-status notifications.
type NotificationDelivery struct {
	q       *db.Queries
	tx      engine.TxStarter
	decrypt Decrypter
	apiBase string
	client  *http.Client
	logger  *slog.Logger
	// leaseTTL bounds one chunk's send plus its recording; injectable so a
	// test can watch an abandoned lease expire.
	leaseTTL time.Duration
	now      func() time.Time
	media    *Outbound
}

// NewNotificationDelivery builds the worker. apiBase and client override the
// Bot API host and transport (tests); empty and nil use production defaults.
func NewNotificationDelivery(q *db.Queries, tx engine.TxStarter, decrypt Decrypter, apiBase string, client *http.Client, logger *slog.Logger) *NotificationDelivery {
	if logger == nil {
		logger = slog.Default()
	}
	return &NotificationDelivery{
		q: q, tx: tx, decrypt: decrypt, apiBase: apiBase, client: client, logger: logger,
		leaseTTL: deliveryLeaseTTL, now: time.Now,
	}
}

// EnableFileDelivery uses the same attachment transport as ordinary chat replies.
// Configure at boot, before Run. Files are attempted only after the text is
// durably settled, so an uncertain text send never leaks or duplicates files.
func (d *NotificationDelivery) EnableFileDelivery(objects objectStore) {
	d.media = NewOutbound(d.q, d.decrypt, d.apiBase, d.client, d.logger)
	d.media.EnableFileDelivery(objects)
	// Complete this hop before Tick returns; it is bounded by attachmentBudget.
	d.media.spawn = func(f func()) { f() }
}

// Run delivers due notifications until ctx ends.
func (d *NotificationDelivery) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			d.logger.WarnContext(ctx, "telegram notifications: tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick settles abandoned sends, then delivers every due notification. It
// returns how many deliveries it attempted.
func (d *NotificationDelivery) Tick(ctx context.Context) (int, error) {
	expired, err := d.q.ExpireTaskStatusNotificationSends(ctx)
	if err != nil {
		return 0, fmt.Errorf("expire abandoned sends: %w", err)
	}
	for _, n := range expired {
		d.logger.WarnContext(ctx, "telegram notifications: inherited a send from a process that stopped; outcome unknown",
			"notification_id", util.UUIDToString(n.ID))
	}
	attempted := 0
	for ctx.Err() == nil {
		n, err := d.q.ClaimTaskStatusNotificationDelivery(ctx, d.leaseTTL.Seconds())
		if errors.Is(err, pgx.ErrNoRows) {
			return attempted, nil
		}
		if err != nil {
			return attempted, fmt.Errorf("claim notification: %w", err)
		}
		attempted++
		d.deliver(ctx, n)
	}
	return attempted, ctx.Err()
}

// deliver owns one claimed notification until it leaves 'sending'.
func (d *NotificationDelivery) deliver(ctx context.Context, n db.TaskStatusNotification) {
	text, chatID, token, reason, err := d.prepare(ctx, n)
	if err != nil {
		// Infrastructure, not a verdict: hand the row back for a later tick.
		d.logger.WarnContext(ctx, "telegram notifications: preparing delivery failed; will retry",
			"notification_id", util.UUIDToString(n.ID), "error", err)
		d.postpone(ctx, n, d.now().Add(30*time.Second), "prepare failed: "+err.Error())
		return
	}
	if reason != "" {
		d.fail(ctx, n, reason)
		return
	}
	api := newBotAPI(d.apiBase, token, d.client)
	chunks := chunkMessage(text, maxMessageUnits)
	for i := int(n.ChunksSent); i < len(chunks); i++ {
		if !d.renew(ctx, n) {
			d.logger.WarnContext(ctx, "telegram notifications: lost the delivery lease; stopping",
				"notification_id", util.UUIDToString(n.ID))
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, d.callBudget())
		m, sendErr := sendNotificationChunk(callCtx, api, chatID, chunks[i])
		cancel()
		if sendErr != nil {
			if wait, ok := retryAfter(sendErr); ok {
				d.postpone(ctx, n, d.now().Add(wait), sendErr.Error())
				return
			}
			if isDefiniteRejection(sendErr) {
				d.fail(ctx, n, sendErr.Error())
				return
			}
			d.unknown(ctx, n, sendErr.Error())
			return
		}
		if err := d.recordChunk(ctx, n, chatID, i, m.MessageID); err != nil {
			// Telegram has the chunk; the ledger may not. Stop rather than
			// risk sending the next part on top of an unrecorded one.
			d.logger.ErrorContext(ctx, "telegram notifications: recording a delivered chunk failed",
				"notification_id", util.UUIDToString(n.ID), "chunk", i, "error", err)
			d.unknown(ctx, n, "recording a delivered chunk failed: "+err.Error())
			return
		}
	}
	if err := d.complete(ctx, n, chatID); err != nil {
		d.logger.ErrorContext(ctx, "telegram notifications: completing delivery failed",
			"notification_id", util.UUIDToString(n.ID), "error", err)
		return
	}
	if d.media != nil {
		d.media.deliverMessageAttachments(ctx, n.ChatMessageID, n.WorkspaceID, replyTarget{chatID: chatID, botToken: token})
	}
	d.logger.InfoContext(ctx, "telegram notifications: text delivered",
		"notification_id", util.UUIDToString(n.ID), "issue_id", util.UUIDToString(n.IssueID),
		"issue_status", n.IssueStatus, "chunks", len(chunks))
}

// prepare re-verifies the route and loads the canonical text. A non-empty
// reason is a refusal recorded on the row; err is an infrastructure failure.
func (d *NotificationDelivery) prepare(ctx context.Context, n db.TaskStatusNotification) (text string, chatID int64, token, reason string, err error) {
	sub, err := d.q.GetTaskStatusNotificationSubscription(ctx, n.SubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", "subscription_missing", nil
	}
	if err != nil {
		return "", 0, "", "", err
	}
	if !sub.Enabled || sub.UserID != n.UserID || sub.AgentID != n.AgentID || sub.InstallationID != n.InstallationID {
		return "", 0, "", "subscription_changed", nil
	}
	route, err := service.ResolveTaskStatusNotificationRoute(ctx, d.q, n.WorkspaceID, n.UserID, n.AgentID, n.InstallationID, false)
	var refused *service.TaskStatusNotificationRouteError
	if errors.As(err, &refused) {
		return "", 0, "", refused.Reason, nil
	}
	if err != nil {
		return "", 0, "", "", err
	}
	// The chat is the member's own private chat with the bot: Telegram's id
	// of a private chat is the user id the member linked. A relinked account
	// is a different person's chat as far as this row knows.
	if !n.ChannelChatID.Valid || route.UserBinding.ChannelUserID != n.ChannelChatID.String {
		return "", 0, "", "telegram_account_changed", nil
	}
	chatID, perr := strconv.ParseInt(n.ChannelChatID.String, 10, 64)
	if perr != nil || chatID <= 0 {
		return "", 0, "", "not_private_chat", nil
	}
	msg, err := d.q.GetChatMessage(ctx, n.ChatMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", "message_missing", nil
	}
	if err != nil {
		return "", 0, "", "", err
	}
	if msg.ChatSessionID != n.ChatSessionID || msg.Role != "assistant" || msg.Content == "" {
		return "", 0, "", "message_mismatch", nil
	}
	creds, err := decodeCredentials(route.Installation.Config, d.decrypt)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("decode credentials: %w", err)
	}
	return msg.Content, chatID, creds.BotToken, "", nil
}

// sendNotificationChunk posts one chunk as Telegram HTML, falling back to the
// raw text when Telegram rejects the markup — a 400 that proves nothing was
// posted, so the second request cannot duplicate the first.
func sendNotificationChunk(ctx context.Context, api *botAPI, chatID int64, chunk string) (Message, error) {
	m, err := api.SendMessage(ctx, sendMessageParams{ChatID: chatID, Text: formatHTML(chunk), ParseMode: "HTML"})
	if err != nil && isHTMLParseError(err) {
		return api.SendMessage(ctx, sendMessageParams{ChatID: chatID, Text: chunk})
	}
	return m, err
}

func (d *NotificationDelivery) callBudget() time.Duration {
	if d.leaseTTL < deliveryLeaseTTL {
		return d.leaseTTL / 3
	}
	return deliveryCallTimeout
}

func (d *NotificationDelivery) recordCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), deliveryRecordTimeout)
}

func (d *NotificationDelivery) renew(ctx context.Context, n db.TaskStatusNotification) bool {
	rows, err := d.q.RenewTaskStatusNotificationLease(ctx, db.RenewTaskStatusNotificationLeaseParams{
		ID: n.ID, LeaseToken: n.LeaseToken, LeaseSeconds: d.leaseTTL.Seconds(),
	})
	return err == nil && rows == 1
}

// recordChunk writes one delivered chunk to the ledger, then advances
// progress. The ledger row is a fact about Telegram, so it is written whether
// or not this process still holds the lease — a reply to that message must
// resolve even if the row was meanwhile marked unknown. Progress is
// owner-checked.
func (d *NotificationDelivery) recordChunk(ctx context.Context, n db.TaskStatusNotification, chatID int64, index int, messageID int64) error {
	ctx, cancel := d.recordCtx(ctx)
	defer cancel()
	if err := d.q.InsertTaskStatusNotificationMessage(ctx, db.InsertTaskStatusNotificationMessageParams{
		InstallationID:   n.InstallationID,
		ChannelChatID:    strconv.FormatInt(chatID, 10),
		ChannelMessageID: messageKey(chatID, messageID),
		NotificationID:   n.ID,
		ChatSessionID:    n.ChatSessionID,
		ChatMessageID:    n.ChatMessageID,
		ChunkIndex:       int32(index),
	}); err != nil {
		return err
	}
	rows, err := d.q.RecordTaskStatusNotificationChunk(ctx, db.RecordTaskStatusNotificationChunkParams{
		ID: n.ID, LeaseToken: n.LeaseToken, ChunksSent: int32(index + 1),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("delivery lease lost")
	}
	return nil
}

// complete marks the notification sent and stamps the canonical chat message
// with where it went, in one transaction.
func (d *NotificationDelivery) complete(ctx context.Context, n db.TaskStatusNotification, chatID int64) error {
	ctx, cancel := d.recordCtx(ctx)
	defer cancel()
	return d.inTx(ctx, func(q *db.Queries) error {
		ids, err := q.ListTaskStatusNotificationMessageIDs(ctx, n.ID)
		if err != nil {
			return err
		}
		if err := q.SetChatMessageChannelOutbound(ctx, db.SetChatMessageChannelOutboundParams{
			ID:                            n.ChatMessageID,
			ChannelOutboundType:           pgtype.Text{String: string(TypeTelegram), Valid: true},
			ChannelOutboundInstallationID: n.InstallationID,
			ChannelOutboundChatID:         pgtype.Text{String: strconv.FormatInt(chatID, 10), Valid: true},
			ChannelOutboundMessageIds:     ids,
		}); err != nil {
			return err
		}
		rows, err := q.CompleteTaskStatusNotificationSend(ctx, db.CompleteTaskStatusNotificationSendParams{
			ID: n.ID, LeaseToken: n.LeaseToken,
		})
		if err != nil {
			return err
		}
		if rows != 1 {
			return errors.New("delivery lease lost")
		}
		return nil
	})
}

func (d *NotificationDelivery) postpone(ctx context.Context, n db.TaskStatusNotification, at time.Time, reason string) {
	ctx, cancel := d.recordCtx(ctx)
	defer cancel()
	if _, err := d.q.DeferTaskStatusNotificationSend(ctx, db.DeferTaskStatusNotificationSendParams{
		ID: n.ID, LeaseToken: n.LeaseToken,
		NextAttemptAt: pgtype.Timestamptz{Time: at, Valid: true},
		LastError:     pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		d.logger.ErrorContext(ctx, "telegram notifications: deferring failed", "notification_id", util.UUIDToString(n.ID), "error", err)
	}
}

func (d *NotificationDelivery) fail(ctx context.Context, n db.TaskStatusNotification, reason string) {
	ctx, cancel := d.recordCtx(ctx)
	defer cancel()
	d.logger.WarnContext(ctx, "telegram notifications: delivery refused", "notification_id", util.UUIDToString(n.ID), "reason", reason)
	if _, err := d.q.FailTaskStatusNotificationSend(ctx, db.FailTaskStatusNotificationSendParams{
		ID: n.ID, LeaseToken: n.LeaseToken, LastError: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		d.logger.ErrorContext(ctx, "telegram notifications: recording refusal failed", "notification_id", util.UUIDToString(n.ID), "error", err)
	}
}

func (d *NotificationDelivery) unknown(ctx context.Context, n db.TaskStatusNotification, reason string) {
	ctx, cancel := d.recordCtx(ctx)
	defer cancel()
	d.logger.WarnContext(ctx, "telegram notifications: send result unknown; not retrying to avoid a duplicate",
		"notification_id", util.UUIDToString(n.ID), "reason", reason)
	if _, err := d.q.MarkTaskStatusNotificationSendUnknown(ctx, db.MarkTaskStatusNotificationSendUnknownParams{
		ID: n.ID, LastError: pgtype.Text{String: reason, Valid: true},
	}); err != nil {
		d.logger.ErrorContext(ctx, "telegram notifications: recording unknown outcome failed", "notification_id", util.UUIDToString(n.ID), "error", err)
	}
}

func (d *NotificationDelivery) inTx(ctx context.Context, fn func(*db.Queries) error) error {
	if d.tx == nil {
		return fn(d.q)
	}
	tx, err := d.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(d.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
