package main

import (
	"encoding/json"
	"github.com/spf13/cobra"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func notificationTestCommand() *cobra.Command {
	c := &cobra.Command{Use: "test"}
	for _, flag := range []string{"file", "message", "agent-id", "server-url", "workspace-id", "profile"} {
		c.Flags().String(flag, "", "")
	}
	return c
}
func TestNotificationCLIRequestsAndBounds(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer mat_test-token" {
			t.Error("lost task token")
		}
		if !strings.HasPrefix(r.URL.Path, "/api/notification-events") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method == "POST" && !strings.HasSuffix(r.URL.Path, "/decide") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if strings.HasSuffix(r.URL.Path, "/ack") && body["message_id"] != "member-message" {
				t.Errorf("ack source %v", body)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"shadow","delivered":false}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	c := notificationTestCommand()
	if err := runNotificationPublish(c, nil); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := runNotificationAck(c, []string{"event"}); err == nil {
		t.Fatal("missing source accepted")
	}
	path := filepath.Join(t.TempDir(), "event.json")
	_ = c.Flags().Set("file", path)
	for _, bad := range []string{"null", "[]", "{", strings.Repeat("x", 16385)} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runNotificationPublish(c, nil); err == nil {
			t.Fatalf("accepted %q", bad[:1])
		}
	}
	if requests != 0 {
		t.Fatal("malformed input reached network")
	}
	_ = os.WriteFile(path, []byte(`{"producer":"subscription-limits","event_key":"snapshot","facts":{}}`), 0600)
	_, err := captureStdout(t, func() error { return runNotificationPublish(c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(*cobra.Command, []string) error{runNotificationRead, runNotificationDecision, runNotificationAck} {
		_ = c.Flags().Set("message", "member-message")
		if _, err := captureStdout(t, func() error { return run(c, []string{"event"}) }); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 4 {
		t.Fatalf("requests %d", requests)
	}
}
