package service

import (
	"context"
	"errors"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"testing"
	"time"
)

func TestNotificationScopedNaturalAckProvenance(t *testing.T) {
	f := newNotificationFixture(t)
	e := f.capture(t)
	scoped := "я в курсе, лимиты Claude a на 5 часов исчерпаны до " + e.ResetAt.Time.UTC().Format(time.RFC3339)
	for _, text := range []string{"я в курсе", "> " + scoped, "Макс сказал: " + scoped, scoped + "?", "\"" + scoped + "\""} {
		m := f.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": util.UUIDToString(f.session), "role": "user", "message_kind": "message", "content": text, "created_at": f.now.Add(-time.Second)})
		if err := f.s.Acknowledge(context.Background(), f.w, f.u, f.a, e.ID, f.session, util.MustParseUUID(m)); !errors.Is(err, ErrNotificationAckSource) {
			t.Fatalf("unscoped/quoted accepted: %s: %v", text, err)
		}
	}
	m := f.fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": util.UUIDToString(f.session), "role": "user", "message_kind": "message", "content": scoped, "created_at": f.now.Add(-time.Second)})
	if err := f.s.Acknowledge(context.Background(), f.w, f.u, f.a, e.ID, f.session, util.MustParseUUID(m)); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.AuthorizeShadow(context.Background(), f.w, f.u, f.a, e.ID)
	if err != nil || d.Verdict != NotificationSuppress {
		t.Fatalf("scope not bound: %+v %v", d, err)
	}
}
