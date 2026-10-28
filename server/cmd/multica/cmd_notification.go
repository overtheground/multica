package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

var notificationCmd = &cobra.Command{Use: "notification", Short: "Capture and inspect structured notifications (shadow only)"}

func init() {
	publish := &cobra.Command{Use: "publish", Short: "Durably capture a shadow event; never sends a message", Args: cobra.NoArgs, RunE: runNotificationPublish}
	publish.Flags().String("file", "", "Structured UTF-8 JSON event file (required)")
	get := &cobra.Command{Use: "get <id>", Short: "Read an event and its decision audit", Args: cobra.ExactArgs(1), RunE: runNotificationRead}
	decide := &cobra.Command{Use: "decide <id>", Short: "Re-evaluate shadow delivery against current awareness", Args: cobra.ExactArgs(1), RunE: runNotificationDecision}
	ack := &cobra.Command{Use: "ack <id>", Short: "Verify a scoped member acknowledgement in this task's chat", Args: cobra.ExactArgs(1), RunE: runNotificationAck}
	ack.Flags().String("message", "", "Canonical member message ID (required); explicit event ID or full scoped quota acknowledgement")
	for _, c := range []*cobra.Command{publish, get, decide, ack} {
		c.Flags().String("agent-id", "", "Agent UUID (human CLI only; task callers use their own agent)")
		notificationCmd.AddCommand(c)
	}
	rootCmd.AddCommand(notificationCmd)
}

func notificationRequest(cmd *cobra.Command, path string, body any, get bool) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if get {
		err = client.GetJSON(ctx, path, &out)
	} else {
		err = client.PostJSON(ctx, path, body, &out)
	}
	if err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

func runNotificationPublish(cmd *cobra.Command, _ []string) error {
	file, _ := cmd.Flags().GetString("file")
	if file == "" {
		return errors.New("--file is required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if len(raw) > 16384 {
		return errors.New("event exceeds 16 KiB")
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if body == nil {
		return errors.New("event must be an object")
	}
	agent, _ := cmd.Flags().GetString("agent-id")
	if agent != "" {
		body["agent_id"], _ = json.Marshal(agent)
	}
	return notificationRequest(cmd, "/api/notification-events", body, false)
}

func notificationPath(cmd *cobra.Command, id, suffix string) string {
	agent, _ := cmd.Flags().GetString("agent-id")
	q := url.Values{}
	if agent != "" {
		q.Set("agent_id", agent)
	}
	path := "/api/notification-events/" + url.PathEscape(id) + suffix
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
}
func runNotificationRead(cmd *cobra.Command, args []string) error {
	return notificationRequest(cmd, notificationPath(cmd, args[0], ""), nil, true)
}
func runNotificationDecision(cmd *cobra.Command, args []string) error {
	return notificationRequest(cmd, notificationPath(cmd, args[0], "/decide"), map[string]any{}, false)
}
func runNotificationAck(cmd *cobra.Command, args []string) error {
	message, _ := cmd.Flags().GetString("message")
	if message == "" {
		return errors.New("--message is required")
	}
	return notificationRequest(cmd, notificationPath(cmd, args[0], "/ack"), map[string]string{"message_id": message}, false)
}
