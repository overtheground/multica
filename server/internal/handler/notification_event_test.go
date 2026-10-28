package handler

import (
	"context"
	"encoding/json"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestNotificationEventHTTPShadowAndCanonicalAck(t *testing.T) {
	if testHandler == nil {
		t.Skip("database unavailable")
	}
	runtime := handlerTestRuntimeID(t)
	agent := dbfx.Agent(t, "Notification HTTP", runtime, testutil.Cols{"visibility": "workspace"})
	session := dbfx.ChatSession(t, agent)
	task := dbfx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": session, "originator_user_id": testUserID, "accountable_user_id": testUserID, "status": "running"})
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(testUserID)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	body := map[string]any{"producer": "subscription-limits", "event_key": "http-shadow", "facts": service.NotificationFacts{Subject: "claude/account/five_hour", Period: "next-reset", Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), FreshUntil: now.Add(time.Minute), Urgency: "routine"}}
	request := func(method, path string, payload any) *http.Request {
		r := newRequest(method, path, payload)
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Task-ID", task)
		return r.WithContext(middleware.SetMemberContext(r.Context(), testWorkspaceID, member))
	}
	var captured struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
		Delivered bool   `json:"delivered"`
		Mode      string `json:"mode"`
	}
	testutil.Call(t, testHandler.CaptureNotificationEvent, request("POST", "/api/notification-events", body)).Want(200).JSON(&captured)
	if captured.Event.ID == "" || captured.Delivered || captured.Mode != "shadow" {
		t.Fatalf("capture: %+v", captured)
	}
	event := captured.Event.ID
	dbfx.Cleanup(t, "DELETE FROM notification_event WHERE id=$1", event)
	dbfx.Cleanup(t, "DELETE FROM notification_awareness WHERE workspace_id=$1 AND agent_id=$2", testWorkspaceID, agent)
	dbfx.Cleanup(t, "DELETE FROM notification_decision_audit WHERE event_id=$1", event)
	route := func(path string, payload any) *http.Request {
		return withURLParam(request("POST", path, payload), "id", event)
	}
	var decision struct {
		Decision  service.NotificationDecision `json:"decision"`
		Delivered bool                         `json:"delivered"`
	}
	testutil.Call(t, testHandler.DecideNotificationEvent, route("/decide", nil)).Want(200).JSON(&decision)
	if decision.Decision.Verdict != service.NotificationSend || decision.Delivered {
		t.Fatalf("first shadow: %+v", decision)
	}
	message := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": session, "role": "user", "content": "ack " + event, "message_kind": "message"})
	testutil.Call(t, testHandler.AcknowledgeNotificationEvent, route("/ack", map[string]string{"message_id": message})).Want(200)
	testutil.Call(t, testHandler.DecideNotificationEvent, route("/decide", nil)).Want(200).JSON(&decision)
	if decision.Decision.Verdict != service.NotificationSuppress || decision.Decision.AckMessageID != message {
		t.Fatalf("ack ignored: %+v", decision)
	}
	var journal map[string]json.RawMessage
	testutil.Call(t, testHandler.GetNotificationEvent, route("/get", nil)).Want(200).JSON(&journal)
	t.Logf("shadow journal: %s", journal["audit"])
	if len(journal["audit"]) == 0 {
		t.Fatal("missing audit")
	}
	// An issue task cannot borrow a source message from a chat it is not running.
	issueTask := dbfx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "originator_user_id": testUserID, "accountable_user_id": testUserID, "status": "running"})
	r := route("/ack", map[string]string{"message_id": message})
	r.Header.Set("X-Task-ID", issueTask)
	testutil.Call(t, testHandler.AcknowledgeNotificationEvent, r).Want(400)
	// Payload-supplied author/session/recipient is never an authorization input.
	testutil.Call(t, testHandler.AcknowledgeNotificationEvent, route("/ack", map[string]string{"message_id": message, "author": testUserID})).Want(400)
	body["user_id"] = testUserID
	testutil.Call(t, testHandler.CaptureNotificationEvent, request("POST", "/api/notification-events", body)).Want(400)
}
