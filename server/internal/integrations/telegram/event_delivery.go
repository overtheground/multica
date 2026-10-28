package telegram

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/multica-ai/multica/server/internal/service"
)

// EventTransport reuses the task-status sender's chunking, markup fallback and
// error classification. It is not started at boot; producer ingress stays shadow.
type EventTransport struct {
	Decrypt Decrypter
	APIBase string
	Client  *http.Client
}

func (t *EventTransport) Prepare(_ context.Context, route service.TaskStatusNotificationRoute) (service.NotificationSender, error) {
	chatID, err := strconv.ParseInt(route.UserBinding.ChannelUserID, 10, 64)
	if err != nil || chatID <= 0 {
		return nil, errors.New("event delivery requires private Telegram chat")
	}
	creds, err := decodeCredentials(route.Installation.Config, t.Decrypt)
	if err != nil {
		return nil, err
	}
	return &eventSender{api: newBotAPI(t.APIBase, creds.BotToken, t.Client), chatID: chatID}, nil
}

type eventSender struct {
	api    *botAPI
	chatID int64
}

func (s *eventSender) Chunks(text string) []string { return chunkMessage(text, maxMessageUnits) }
func (s *eventSender) Send(ctx context.Context, text string) service.NotificationSendResult {
	msg, err := sendNotificationChunk(ctx, s.api, s.chatID, text)
	if err == nil && msg.MessageID <= 0 {
		return service.NotificationSendResult{Err: errors.New("Telegram returned no message identity; outcome unknown")}
	}
	if err == nil {
		return service.NotificationSendResult{MessageID: messageKey(s.chatID, msg.MessageID)}
	}
	if wait, ok := retryAfter(err); ok {
		return service.NotificationSendResult{Rejected: true, RetryAfter: wait, Err: err}
	}
	return service.NotificationSendResult{Rejected: isDefiniteRejection(err), Err: err}
}
