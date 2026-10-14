package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// notification_context.go — what a member's private message is answering when
// the agent spoke first (task-status notifications, notification_delivery.go).
//
// Telegram sends the quoted text of a reply, but that copy may be one chunk
// of a longer message, or edited by Telegram's own trimming. The canonical
// text lives in Multica: the ledger maps every delivered Telegram message id
// to the chat message the agent wrote, so a reply — minutes or weeks later,
// across a server restart — carries that exact text into the turn. An
// ordinary follow-up that quotes nothing carries the notifications delivered
// since the member last wrote, which is what such a message most likely
// answers.

// maxRecentNotifications bounds the unanswered notifications one message
// carries.
const maxRecentNotifications = 3

type notificationContextQueries interface {
	GetTaskStatusNotificationByChannelMessage(ctx context.Context, arg db.GetTaskStatusNotificationByChannelMessageParams) (db.GetTaskStatusNotificationByChannelMessageRow, error)
	ListUnansweredTaskStatusNotifications(ctx context.Context, arg db.ListUnansweredTaskStatusNotificationsParams) ([]db.ListUnansweredTaskStatusNotificationsRow, error)
}

type notificationContext struct {
	q      notificationContextQueries
	logger *slog.Logger
}

// enrich returns the body the member's message is stored and answered with.
// Best effort: a lookup that fails leaves the member's own text untouched
// rather than dropping the message. sessionID may be invalid for a message
// that opens a new chat, which has no unanswered notifications yet.
func (c *notificationContext) enrich(ctx context.Context, installationID, sessionID, sender pgtype.UUID, msg channel.InboundMessage) string {
	body := msg.Text
	if c == nil || c.q == nil || msg.Source.ChatType != channel.ChatTypeP2P {
		return body
	}
	if raw, err := decodeTelegramRaw(msg); err != nil || len(raw.Media) > 0 {
		// Media placeholders are matched by occurrence in the final body;
		// prepending agent text could shift them.
		return body
	}
	if msg.ReplyTo != nil && msg.ReplyTo.MessageID != "" {
		row, err := c.q.GetTaskStatusNotificationByChannelMessage(ctx, db.GetTaskStatusNotificationByChannelMessageParams{
			InstallationID: installationID, ChannelMessageID: msg.ReplyTo.MessageID,
		})
		switch {
		case err == nil && row.UserID == sender:
			return prependBlock(renderRepliedNotification(row), body)
		case err == nil:
			// Another member's notification can only be quoted by a forwarded
			// copy; its content is not this sender's to read.
			return body
		case errors.Is(err, pgx.ErrNoRows):
			// A reply to an ordinary bot message: nothing to resolve.
		default:
			c.logger.WarnContext(ctx, "telegram: resolving a replied-to notification failed", "error", err)
			return body
		}
	}
	if !sessionID.Valid || msg.ForceFresh {
		// A new chat, or a member who just asked for a fresh context, gets
		// no ambient carry-over; only what they explicitly replied to.
		return body
	}
	rows, err := c.q.ListUnansweredTaskStatusNotifications(ctx, db.ListUnansweredTaskStatusNotificationsParams{
		ChatSessionID: sessionID, MaxItems: maxRecentNotifications,
	})
	if err != nil {
		c.logger.WarnContext(ctx, "telegram: loading unanswered notifications failed", "error", err)
		return body
	}
	if len(rows) == 0 {
		return body
	}
	return prependBlock(renderRecentNotifications(rows), body)
}

func prependBlock(block, body string) string {
	if strings.TrimSpace(body) == "" {
		return block
	}
	return block + "\n\n" + body
}

func renderRepliedNotification(row db.GetTaskStatusNotificationByChannelMessageRow) string {
	return fmt.Sprintf("<replied_to_notification chat_message_id=%q issue_id=%q status=%q sent_at=%q>\n%s\n</replied_to_notification>",
		util.UUIDToString(row.ChatMessageID), util.UUIDToString(row.IssueID), row.IssueStatus,
		formatNotificationTime(row.SentAt), row.Content)
}

func renderRecentNotifications(rows []db.ListUnansweredTaskStatusNotificationsRow) string {
	var b strings.Builder
	b.WriteString("<recent_notifications note=\"You sent these to the member after their previous message; the message below may answer one of them.\">\n")
	// Rows come newest first; render oldest first, as the member read them.
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		fmt.Fprintf(&b, "<notification chat_message_id=%q issue_id=%q status=%q sent_at=%q>\n%s\n</notification>\n",
			util.UUIDToString(row.ChatMessageID), util.UUIDToString(row.IssueID), row.IssueStatus,
			formatNotificationTime(row.SentAt), row.Content)
	}
	b.WriteString("</recent_notifications>")
	return b.String()
}

func formatNotificationTime(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
