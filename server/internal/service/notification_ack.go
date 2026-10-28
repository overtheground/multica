package service

import (
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Exact full-message grammar prevents quoted/relayed words from becoming ack.
// No semantic claim is made for arbitrary language or an unscoped “I know”.
func matchesNotificationAck(text string, e db.NotificationEvent) bool {
	if text == "ack "+util.UUIDToString(e.ID) || text == "в курсе "+util.UUIDToString(e.ID) {
		return true
	}
	if e.Action != "blocked_until_reset" {
		return false
	}
	parts := strings.Split(e.Subject, "/")
	if len(parts) != 3 || parts[0] != "claude" || parts[1] == "" {
		return false
	}
	window := map[string]string{"five_hour": "5 часов", "seven_day": "неделю"}[parts[2]]
	if window == "" {
		return false
	}
	expected := "я в курсе, лимиты Claude " + parts[1] + " на " + window + " исчерпаны до " + e.ResetAt.Time.UTC().Format(time.RFC3339)
	return text == expected
}
