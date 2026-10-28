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

func TestNotificationTopicCLIRevisionAndConsentBody(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/notification-topics" || r.Method != "PUT" {
			t.Error("incorrect topic endpoint")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["expected_revision"] != float64(7) || body["legacy_stopped"] != true || body["enabled"] != true || body["route_owner"] != "direct" {
			t.Errorf("consent body: %v", body)
		}
		if _, exists := body["user_id"]; exists {
			t.Error("caller selected recipient")
		}
		_, _ = w.Write([]byte(`{"revision":8}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	c := notificationTestCommand()
	c.Flags().String("installation", "installation", "")
	c.Flags().Int64("revision", 0, "")
	c.Flags().String("owner", "direct", "")
	c.Flags().Bool("enabled", true, "")
	c.Flags().Bool("legacy-stopped", true, "")
	c.Flags().String("producer", "subscription-limits", "")
	if err := runNotificationTopicSet(c, nil); err == nil || calls != 0 {
		t.Fatal("configuration did not require explicit revision")
	}
	_ = c.Flags().Set("revision", "7")
	if _, err := captureStdout(t, func() error { return runNotificationTopicSet(c, nil) }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected retries")
	}
}
