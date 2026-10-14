package handler

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/slack"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chat_prompt_history.go — the stored conversation a chat run starts from
// when it cannot continue its provider session.
//
// A chat turn's input is only the messages this turn answers; everything
// earlier lives in the agent's provider session, which the next turn resumes.
// When that session cannot be resumed — none recorded on this runtime, a Codex
// rollout that went missing, a resume the provider rejects — the run used to
// start with nothing earlier than its own input. The claim therefore ships the
// most recent stored messages of the same conversation, and the daemon renders
// them only when the run really starts without the session
// (daemon/prompt.go), so a resumed turn never sees them twice. A message the
// agent sent first — a task-status notification (MUL-11) — reaches the
// member's next turn this way as the agent's own previous message, not as
// text copied into the member's.

const (
	// chatPromptHistoryMessages bounds how many stored messages one claim
	// carries; older context stays readable through `multica chat history`.
	chatPromptHistoryMessages = 20
	// chatPromptHistoryMessageRunes clips one long message.
	chatPromptHistoryMessageRunes = 4000
	// chatPromptHistoryRunes bounds the whole block, newest messages first.
	chatPromptHistoryRunes = 16000
)

// ChatHistoryEntry is one stored message of the conversation before this
// turn's input, oldest first.
type ChatHistoryEntry struct {
	ID        string `json:"id"`
	Role      string `json:"role"` // "user" or "assistant"
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// chatPromptHistoryAllowed reports whether a chat task may start from the
// stored conversation. A member who asked for a fresh session gets none; an
// automatic fresh retry continues the same conversation, only in a new
// provider session. Slack reads its live channel instead (buildChatPrompt).
func chatPromptHistoryAllowed(task db.AgentTaskQueue, resp AgentTaskResponse) bool {
	if resp.ChatIntro || resp.ChatChannelType == string(slack.TypeSlack) {
		return false
	}
	return !task.ForceFreshSession || task.RetryOfTaskID.Valid
}

// loadChatPromptHistory returns the messages stored before this turn's input.
// A channel task reads only its own context generation, the same projection
// `multica chat history` serves it.
func (h *Handler) loadChatPromptHistory(ctx context.Context, sessionID pgtype.UUID, contextRevision pgtype.Int8, input []db.ChatMessage) ([]ChatHistoryEntry, error) {
	if len(input) == 0 {
		return nil, nil
	}
	first := input[0]
	for _, m := range input[1:] {
		if m.CreatedAt.Time.Before(first.CreatedAt.Time) ||
			(m.CreatedAt.Time.Equal(first.CreatedAt.Time) && uuidToString(m.ID) < uuidToString(first.ID)) {
			first = m
		}
	}
	before := pgtype.Timestamptz{Time: first.CreatedAt.Time, Valid: true}
	var rows []db.ChatMessage
	var err error
	if contextRevision.Valid {
		rows, err = h.Queries.ListChatMessagesPageForChannelContext(ctx, db.ListChatMessagesPageForChannelContextParams{
			ChatSessionID: sessionID, ChannelContextRevision: contextRevision,
			PageLimit: chatPromptHistoryMessages, BeforeCreatedAt: before, BeforeID: first.ID,
		})
	} else {
		rows, err = h.Queries.ListChatMessagesPage(ctx, db.ListChatMessagesPageParams{
			ChatSessionID: sessionID, Limit: chatPromptHistoryMessages,
			BeforeCreatedAt: before, BeforeID: first.ID,
		})
	}
	if err != nil {
		return nil, err
	}
	return chatPromptHistoryFromRows(rows), nil
}

// chatPromptHistoryFromRows turns newest-first rows into the oldest-first
// entries a prompt shows. Server-authored turn inputs (the onboarding kickoff,
// a notification's event facts) were never part of the conversation, and a
// failed turn's error placeholder is not something the agent said.
func chatPromptHistoryFromRows(rows []db.ChatMessage) []ChatHistoryEntry {
	out := make([]ChatHistoryEntry, 0, len(rows))
	budget := chatPromptHistoryRunes
	for _, m := range rows {
		if isHiddenChatMessageKind(m.MessageKind) || m.FailureReason.Valid || strings.TrimSpace(m.Content) == "" {
			continue
		}
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		content := clipChatHistoryRunes(m.Content, chatPromptHistoryMessageRunes)
		n := len([]rune(content))
		if n > budget {
			break
		}
		budget -= n
		out = append(out, ChatHistoryEntry{
			ID:        uuidToString(m.ID),
			Role:      m.Role,
			Content:   content,
			CreatedAt: m.CreatedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func clipChatHistoryRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
