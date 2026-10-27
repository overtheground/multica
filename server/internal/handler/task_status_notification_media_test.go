package handler

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/telegram"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type notificationTestObjects struct{}

func (notificationTestObjects) Upload(context.Context, string, []byte, string, string) (string, error) {
	return "", fmt.Errorf("unexpected upload")
}
func (notificationTestObjects) ObjectURL(key string) string { return "https://cdn.example/" + key }
func (notificationTestObjects) KeyFromURL(url string) string {
	return strings.TrimPrefix(url, "https://cdn.example/")
}
func (notificationTestObjects) GetReader(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("stored bytes")), nil
}

// Both triggers upload against the current task, bind in CompleteTask, then
// deliver through their distinct outbound paths. No member resend is involved.
func TestTelegramReplyAttachmentsByTrigger(t *testing.T) {
	for _, trigger := range []string{"member", "task_status_event"} {
		t.Run(trigger, func(t *testing.T) {
			e := newTSNEnv(t)
			e.openChat()
			e.mustSubscribe()
			objects := notificationTestObjects{}
			e.delivery.EnableFileDelivery(objects)
			outbound := telegram.NewOutbound(e.q, nil, e.bot.server.URL, e.bot.server.Client(), nil)
			outbound.EnableFileDelivery(objects)
			outbound.SkipNotificationTurns(e.q)
			ctx, cancel := context.WithCancel(e.ctx)
			bus := events.New()
			previousBus := testHandler.TaskService.Bus
			testHandler.TaskService.Bus = bus
			defer func() { testHandler.TaskService.Bus = previousBus }()
			outbound.Register(bus)
			outbound.Start(ctx)
			defer func() {
				cancel()
				if !outbound.WaitWithTimeout(3 * time.Second) {
					t.Error("outbound did not stop")
				}
			}()
			var issueID string
			if trigger == "member" {
				e.inbound("Скинь файлы", 0)
			} else {
				issueID = e.fx.Issue(t, "result with files", testutil.Cols{"status": "in_progress"})
				e.finishIssueRun(issueID, "in_review", "result")
				e.tick()
			}
			var attachmentIDs []string
			e.beforeComplete = func(task tsnClaim) {
				for _, name := range []string{"report.pdf", "irina-plus2.voice.ogg"} {
					contentType := "application/pdf"
					if strings.HasSuffix(name, "ogg") {
						contentType = "audio/ogg"
					}
					var id string
					e.fx.QueryRow(t, `INSERT INTO attachment (workspace_id,task_id,chat_session_id,uploader_type,uploader_id,filename,url,content_type,size_bytes)
      VALUES ($1,$2,$3,'agent',$4,$5,$6,$7,12) RETURNING id`, e.workspaceID, task.ID, e.sessionID(), e.mikaID, name, objects.ObjectURL(name), contentType).Scan(&id)
					attachmentIDs = append(attachmentIDs, id)
					t.Cleanup(func() { e.fx.Exec(t, `DELETE FROM attachment WHERE id=$1`, id) })
				}
			}
			claim := e.claimAndComplete("Готово, файлы приложены.")
			for _, id := range attachmentIDs {
				var taskID, messageID string
				e.fx.QueryRow(t, `SELECT a.task_id::text,a.chat_message_id::text FROM attachment a JOIN chat_message m ON m.id=a.chat_message_id WHERE a.id=$1 AND m.content=$2`, id, "Готово, файлы приложены.").Scan(&taskID, &messageID)
				if taskID != claim.ID || messageID == "" {
					t.Fatalf("attachment binding: task=%s message=%s", taskID, messageID)
				}
			}
			if trigger == "task_status_event" {
				if n := e.notifications(issueID)[0]; n.State != service.TaskStatusNotificationPending {
					t.Fatalf("before delivery: %+v", n)
				}
				e.deliver()
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				e.bot.mu.Lock()
				count := len(e.bot.uploads)
				e.bot.mu.Unlock()
				if count == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("Bot API uploads=%d, want 2; storage/binding alone is not delivery", count)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if trigger == "task_status_event" {
				e.deliver()
			}
			e.bot.mu.Lock()
			defer e.bot.mu.Unlock()
			if len(e.bot.uploads) != 2 {
				t.Fatalf("duplicate files: %v", e.bot.uploads)
			}
			if len(e.bot.sent) != 1 {
				t.Fatalf("text sends=%d, want 1", len(e.bot.sent))
			}
		})
	}
}

func TestNotificationTextFailureDoesNotSendOrDeleteAttachments(t *testing.T) {
	e := newTSNEnv(t)
	e.openChat()
	e.mustSubscribe()
	e.delivery.EnableFileDelivery(notificationTestObjects{})
	var attachmentID string
	e.beforeComplete = func(task tsnClaim) {
		e.fx.QueryRow(t, `INSERT INTO attachment (workspace_id,task_id,chat_session_id,uploader_type,uploader_id,filename,url,content_type,size_bytes)
   VALUES ($1,$2,$3,'agent',$4,'report.pdf','https://cdn.example/report.pdf','application/pdf',12) RETURNING id`, e.workspaceID, task.ID, e.sessionID(), e.mikaID).Scan(&attachmentID)
		t.Cleanup(func() { e.fx.Exec(t, `DELETE FROM attachment WHERE id=$1`, attachmentID) })
	}
	issueID, _ := e.composeOne("refused text with file", "Файл приложен.")
	e.bot.script(fakeBotReply{status: 403, body: `{"ok":false,"error_code":403,"description":"Forbidden"}`})
	e.deliver()
	if n := e.notifications(issueID)[0]; n.State != service.TaskStatusNotificationFailed {
		t.Fatalf("text state: %+v", n)
	}
	e.bot.mu.Lock()
	uploads := len(e.bot.uploads)
	e.bot.mu.Unlock()
	if uploads != 0 {
		t.Fatalf("files sent after refused text: %d", uploads)
	}
	var count int
	e.fx.QueryRow(t, `SELECT count(*) FROM attachment WHERE id=$1 AND chat_message_id IS NOT NULL`, attachmentID).Scan(&count)
	if count != 1 {
		t.Fatal("bound file lost after transport rejection")
	}
}
