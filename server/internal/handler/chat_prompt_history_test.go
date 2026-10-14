package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestChatPromptHistoryFromRows(t *testing.T) {
	at := func(min int) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: time.Date(2026, 10, 14, 20, min, 0, 0, time.UTC), Valid: true}
	}
	// Newest first, as the transcript queries return them.
	rows := []db.ChatMessage{
		{Role: "assistant", Content: "MUL-11 на ревью.", MessageKind: protocol.ChatMessageKindMessage, CreatedAt: at(5)},
		{Role: "user", Content: "[Multica task status event]", MessageKind: protocol.ChatMessageKindTaskStatusEvent, CreatedAt: at(4)},
		{Role: "assistant", Content: "agent crashed", FailureReason: pgtype.Text{String: "agent_error", Valid: true}, CreatedAt: at(3)},
		{Role: "user", Content: "  ", CreatedAt: at(2)},
		{Role: "user", Content: "Привет", MessageKind: protocol.ChatMessageKindMessage, CreatedAt: at(1)},
		{Role: "user", Content: "introduce yourself", MessageKind: protocol.ChatMessageKindOnboardingKickoff, CreatedAt: at(0)},
	}
	got := chatPromptHistoryFromRows(rows)
	if len(got) != 2 || got[0].Content != "Привет" || got[1].Content != "MUL-11 на ревью." ||
		got[0].Role != "user" || got[1].Role != "assistant" || got[1].CreatedAt != "2026-10-14T20:05:00Z" {
		t.Fatalf("history = %+v, want the visible conversation oldest first", got)
	}

	// One long message is clipped; the block keeps the newest messages that
	// fit its budget.
	long := strings.Repeat("я", chatPromptHistoryMessageRunes+10)
	var many []db.ChatMessage
	for i := 0; i < 10; i++ {
		many = append(many, db.ChatMessage{Role: "user", Content: long, CreatedAt: at(10 - i)})
	}
	got = chatPromptHistoryFromRows(many)
	if want := chatPromptHistoryRunes / (chatPromptHistoryMessageRunes + 1); len(got) != want {
		t.Fatalf("kept %d messages, want %d within the budget", len(got), want)
	}
	if n := len([]rune(got[0].Content)); n != chatPromptHistoryMessageRunes+1 || !strings.HasSuffix(got[0].Content, "…") {
		t.Fatalf("long message not clipped: %d runes", n)
	}
	if got[len(got)-1].CreatedAt != "2026-10-14T20:10:00Z" {
		t.Fatalf("budget dropped the newest message: %+v", got[len(got)-1].CreatedAt)
	}
}
