package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeNotificationContextQueries struct {
	byMessage   map[string]db.GetTaskStatusNotificationByChannelMessageRow
	lookupErr   error
	unanswered  []db.ListUnansweredTaskStatusNotificationsRow
	listCalls   int
	lookupCalls int
}

func (f *fakeNotificationContextQueries) GetTaskStatusNotificationByChannelMessage(_ context.Context, arg db.GetTaskStatusNotificationByChannelMessageParams) (db.GetTaskStatusNotificationByChannelMessageRow, error) {
	f.lookupCalls++
	if f.lookupErr != nil {
		return db.GetTaskStatusNotificationByChannelMessageRow{}, f.lookupErr
	}
	row, ok := f.byMessage[arg.ChannelMessageID]
	if !ok || arg.InstallationID != telegramTestUUID(1) {
		return db.GetTaskStatusNotificationByChannelMessageRow{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeNotificationContextQueries) ListUnansweredTaskStatusNotifications(context.Context, db.ListUnansweredTaskStatusNotificationsParams) ([]db.ListUnansweredTaskStatusNotificationsRow, error) {
	f.listCalls++
	return f.unanswered, nil
}

func privateReply(text, replyTo string) channel.InboundMessage {
	msg := channel.InboundMessage{
		Text:   text,
		Source: channel.Source{ChannelType: TypeTelegram, ChatID: "42", ChatType: channel.ChatTypeP2P, SenderID: "42"},
		Raw:    []byte(`{"bot_id":"1","event_type":"message"}`),
	}
	if replyTo != "" {
		msg.ReplyTo = &channel.ReplyCtx{MessageID: replyTo}
	}
	return msg
}

func TestNotificationContextResolvesRepliesToTheCanonicalText(t *testing.T) {
	member := telegramTestUUID(9)
	sent := pgtype.Timestamptz{Time: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC), Valid: true}
	// A long notification went out in two chunks; Telegram quotes only the
	// part the member replied to. The canonical text is the whole message.
	canonical := "MUL-11 is in review.\n" + strings.Repeat("details ", 50)
	row := db.GetTaskStatusNotificationByChannelMessageRow{
		ChatMessageID: telegramTestUUID(5), IssueID: telegramTestUUID(6), IssueStatus: "in_review",
		SentAt: sent, UserID: member, Content: canonical,
	}
	q := &fakeNotificationContextQueries{byMessage: map[string]db.GetTaskStatusNotificationByChannelMessageRow{
		"42:501": row, "42:502": row,
	}}
	c := &notificationContext{q: q}

	for _, chunk := range []string{"42:501", "42:502"} {
		got := c.enrich(context.Background(), telegramTestUUID(1), telegramTestUUID(3), member, privateReply("what about tests?", chunk))
		want := "<replied_to_notification chat_message_id=\"" + uuidText(telegramTestUUID(5)) + "\" issue_id=\"" + uuidText(telegramTestUUID(6)) + "\" status=\"in_review\" sent_at=\"2026-10-07T09:00:00Z\">\n" +
			canonical + "\n</replied_to_notification>\n\nwhat about tests?"
		if got != want {
			t.Fatalf("reply to %s:\n%s\nwant:\n%s", chunk, got, want)
		}
	}
	if q.listCalls != 0 {
		t.Fatal("a resolved reply also attached the unanswered list")
	}
}

func TestNotificationContextNeverLeaksAnotherMembersNotification(t *testing.T) {
	q := &fakeNotificationContextQueries{byMessage: map[string]db.GetTaskStatusNotificationByChannelMessageRow{
		"42:501": {UserID: telegramTestUUID(8), Content: "someone else's status"},
	}}
	c := &notificationContext{q: q}
	got := c.enrich(context.Background(), telegramTestUUID(1), telegramTestUUID(3), telegramTestUUID(9), privateReply("hi", "42:501"))
	if got != "hi" {
		t.Fatalf("body = %q, want the member's own text only", got)
	}
}

func TestNotificationContextLeavesOtherMessagesAlone(t *testing.T) {
	member := telegramTestUUID(9)
	row := db.GetTaskStatusNotificationByChannelMessageRow{UserID: member, Content: "status"}
	cases := []struct {
		name string
		msg  channel.InboundMessage
		inst pgtype.UUID
		q    *fakeNotificationContextQueries
	}{
		{"another bot's message id", privateReply("hi", "42:501"), telegramTestUUID(2),
			&fakeNotificationContextQueries{byMessage: map[string]db.GetTaskStatusNotificationByChannelMessageRow{"42:501": row}}},
		{"a group message", func() channel.InboundMessage {
			m := privateReply("hi", "42:501")
			m.Source.ChatType = channel.ChatTypeGroup
			return m
		}(), telegramTestUUID(1), &fakeNotificationContextQueries{byMessage: map[string]db.GetTaskStatusNotificationByChannelMessageRow{"42:501": row}}},
		{"a message carrying a file", func() channel.InboundMessage {
			m := privateReply("[Image]", "42:501")
			m.Raw = []byte(`{"bot_id":"1","media":[{"kind":"image","file_id":"f","placeholder":"[Image]"}]}`)
			return m
		}(), telegramTestUUID(1), &fakeNotificationContextQueries{byMessage: map[string]db.GetTaskStatusNotificationByChannelMessageRow{"42:501": row}}},
		{"a lookup that failed", privateReply("hi", "42:501"), telegramTestUUID(1),
			&fakeNotificationContextQueries{lookupErr: errors.New("db down")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &notificationContext{q: tc.q, logger: testLogger()}
			if got := c.enrich(context.Background(), tc.inst, telegramTestUUID(3), member, tc.msg); got != tc.msg.Text {
				t.Fatalf("body = %q, want %q", got, tc.msg.Text)
			}
		})
	}
	var nilContext *notificationContext
	if got := nilContext.enrich(context.Background(), telegramTestUUID(1), telegramTestUUID(3), member, privateReply("hi", "")); got != "hi" {
		t.Fatalf("nil context changed the body: %q", got)
	}
}

func TestNotificationContextCarriesUnansweredNotificationsOldestFirst(t *testing.T) {
	q := &fakeNotificationContextQueries{unanswered: []db.ListUnansweredTaskStatusNotificationsRow{
		{ChatMessageID: telegramTestUUID(5), IssueStatus: "done", Content: "second"},
		{ChatMessageID: telegramTestUUID(4), IssueStatus: "blocked", Content: "first"},
	}}
	c := &notificationContext{q: q}
	got := c.enrich(context.Background(), telegramTestUUID(1), telegramTestUUID(3), telegramTestUUID(9), privateReply("ok", ""))
	if !strings.HasPrefix(got, "<recent_notifications") || !strings.HasSuffix(got, "</recent_notifications>\n\nok") {
		t.Fatalf("body:\n%s", got)
	}
	if strings.Index(got, "first") > strings.Index(got, "second") {
		t.Fatalf("notifications not in the order the member read them:\n%s", got)
	}
	// Asking for a fresh context drops the ambient carry-over.
	q.listCalls = 0
	fresh := privateReply("ok", "")
	fresh.ForceFresh = true
	if got := c.enrich(context.Background(), telegramTestUUID(1), telegramTestUUID(3), telegramTestUUID(9), fresh); got != "ok" || q.listCalls != 0 {
		t.Fatalf("fresh body = %q (list calls %d)", got, q.listCalls)
	}
	// A message that opens a new chat has no unanswered notifications.
	q.listCalls = 0
	if got := c.enrich(context.Background(), telegramTestUUID(1), pgtype.UUID{}, telegramTestUUID(9), privateReply("ok", "")); got != "ok" || q.listCalls != 0 {
		t.Fatalf("new chat body = %q (list calls %d)", got, q.listCalls)
	}
}

type fakeNotificationTurns struct {
	isNotification bool
	err            error
}

func (f fakeNotificationTurns) IsTaskStatusNotificationComposeTask(context.Context, pgtype.UUID) (bool, error) {
	return f.isNotification, f.err
}

// The streaming reply path must not post a notification turn's reply: the
// outbox delivers it, and two paths would send it twice.
func TestResolveTargetSkipsNotificationTurns(t *testing.T) {
	q := newTelegramOutboundQueries(t)
	q.channelOrigin = true

	o := NewOutbound(q, nil, "", nil, nil)
	o.SkipNotificationTurns(fakeNotificationTurns{isNotification: true})
	target, err := o.resolveTarget(context.Background(), telegramTestEvent(), false)
	if err != nil || target != nil {
		t.Fatalf("notification turn target = %+v (err=%v), want none", target, err)
	}

	o = NewOutbound(q, nil, "", nil, nil)
	o.SkipNotificationTurns(fakeNotificationTurns{err: errors.New("db down")})
	if _, err := o.resolveTarget(context.Background(), telegramTestEvent(), false); err == nil {
		t.Fatal("a failed notification-turn lookup must not fall through to delivery")
	}

	o = NewOutbound(q, nil, "", nil, nil)
	o.SkipNotificationTurns(fakeNotificationTurns{})
	if target, err := o.resolveTarget(context.Background(), telegramTestEvent(), false); err != nil || target == nil {
		t.Fatalf("ordinary channel turn target = %+v (err=%v), want delivered", target, err)
	}
}
