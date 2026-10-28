package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestNotificationEventRoutesAdmitTaskTokenWithBoundScope(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	runtime := fx.Runtime(t, "Notification route runtime")
	agent := fx.Agent(t, "Notification route agent", runtime, testutil.Cols{"visibility": "workspace"})
	session := fx.ChatSession(t, agent)
	task := fx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": session, "originator_user_id": testUserID, "accountable_user_id": testUserID})
	token := mintAgentTaskToken(t, agent, task, testUserID)
	do := func(method, path string, body any, want int) map[string]json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, testServer.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", "00000000-0000-0000-0000-000000000099")
		req.Header.Set("X-Agent-ID", "00000000-0000-0000-0000-000000000099")
		req.Header.Set("X-Actor-Source", "member")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, data)
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal(data, &out)
		return out
	}
	now := time.Now().UTC()
	body := service.NotificationEventInput{Producer: "subscription-limits", EventKey: "router", Facts: service.NotificationFacts{Subject: "claude/account/five_hour", Period: "next-reset", Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour), FreshUntil: now.Add(time.Minute), Urgency: "routine"}}
	out := do("POST", "/api/notification-events", body, 200)
	var event struct {
		ID      string `json:"id"`
		UserID  string `json:"user_id"`
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal(out["event"], &event)
	if event.ID == "" || event.UserID != testUserID || event.AgentID != agent {
		t.Fatalf("scope: %+v", event)
	}
	for _, table := range []string{"notification_event", "notification_awareness", "notification_decision_audit"} {
		fx.Cleanup(t, "DELETE FROM "+table+" WHERE workspace_id=$1 AND "+map[string]string{"notification_event": "agent_id=$2", "notification_awareness": "agent_id=$2", "notification_decision_audit": "event_id=$2"}[table], testWorkspaceID, map[string]string{"notification_event": agent, "notification_awareness": agent, "notification_decision_audit": event.ID}[table])
	}
	do("GET", "/api/notification-events/"+event.ID, nil, 200)
	do("POST", "/api/notification-events/"+event.ID+"/decide", map[string]any{}, 200)
	source := fx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": session, "role": "user", "content": "ack " + event.ID, "message_kind": "message"})
	do("POST", "/api/notification-events/"+event.ID+"/ack", map[string]string{"message_id": source}, 200)
	do("POST", "/api/notification-events/"+event.ID+"/decide", map[string]any{}, 200)
	do("GET", "/api/notification-events/"+event.ID+"?agent_id=00000000-0000-0000-0000-000000000099", nil, 403)
	var issues int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM issue WHERE workspace_id=$1 AND title='router'", testWorkspaceID).Scan(&issues); err != nil || issues != 0 {
		t.Fatal("event created issue", err)
	}
}
