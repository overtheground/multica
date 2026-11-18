package agent

import (
	"encoding/json"
	"testing"
)

func assistantWithUsage(id, parent string, in, cr, cw int64) claudeSDKMessage {
	body, _ := json.Marshal(claudeMessageContent{
		ID: id, Role: "assistant", Model: "m",
		Usage: &claudeUsage{InputTokens: in, CacheReadInputTokens: cr, CacheCreationInputTokens: cw},
	})
	return claudeSDKMessage{Type: "assistant", Message: body, ParentToolUseID: parent}
}

func TestRecordClaudeContext(t *testing.T) {
	stats := &ContextStats{}
	seen := map[string]struct{}{}
	recordClaudeContext(assistantWithUsage("a", "", 10, 1000, 90), stats, seen)
	recordClaudeContext(assistantWithUsage("a", "", 10, 1000, 90), stats, seen)    // duplicate block
	recordClaudeContext(assistantWithUsage("b", "tool1", 5, 9999, 0), stats, seen) // subagent
	recordClaudeContext(assistantWithUsage("c", "", 20, 3000, 0), stats, seen)

	if stats.Calls != 2 || stats.PeakTokens != 3020 || stats.SumTokens != 4120 ||
		stats.LastTokens != 3020 || stats.LastCacheReadTokens != 3000 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if contextStatsOrNil(&ContextStats{}) != nil {
		t.Fatal("empty stats must collapse to nil")
	}
}
