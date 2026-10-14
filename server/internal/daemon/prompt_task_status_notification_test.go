package daemon

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// A task-status notification turn (MUL-11) reaches the runtime as an ordinary
// private Telegram chat turn whose input is the server-authored event. The
// prompt does not depend on the provider, so the agent sees the same thing on
// Codex as on Claude Code: the event verbatim, the stored transcript to read
// back, and the rule that the reply is the delivered message.
func TestTaskStatusNotificationTurnPromptIsProviderIndependent(t *testing.T) {
	event := "[Multica task status event — sent by Multica, not written by the member]\n\n" +
		"Issue: MUL-11 — Telegram notifications\nStatus now: in_review\n" +
		"Write one short message to Max about this result."
	task := Task{
		ChatSessionID:   "sess-1",
		ChatChannelType: execenv.ChannelTypeTelegram,
		ChatType:        execenv.ChatTypeP2P,
		ChatMessage:     event,
	}
	codex := BuildPrompt(task, "codex")
	claude := BuildPrompt(task, "claude")
	if codex != claude {
		t.Fatalf("notification turn prompt differs by provider:\n--- codex ---\n%s\n--- claude ---\n%s", codex, claude)
	}
	for _, want := range []string{
		event,
		"Telegram conversation",
		"read it back with `multica chat history`",
		"Reply to Telegram with the final outcome only",
	} {
		if !strings.Contains(codex, want) {
			t.Fatalf("prompt missing %q:\n%s", want, codex)
		}
	}
	if strings.Contains(codex, "no history reader") {
		t.Fatalf("prompt still claims Telegram has no readable history:\n%s", codex)
	}
}
