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
	topic := &cobra.Command{Use: "topic", Short: "Inspect or configure your own structured-notification consent"}
	topicGet := &cobra.Command{Use: "get", Args: cobra.NoArgs, RunE: runNotificationTopicGet}
	topicSet := &cobra.Command{Use: "set", Short: "Human only: configure topic ownership with a revision check", Args: cobra.NoArgs, RunE: runNotificationTopicSet}
	for _, c := range []*cobra.Command{topicGet, topicSet} {
		c.Flags().String("agent-id", "", "Agent UUID")
		c.Flags().String("producer", "subscription-limits", "Structured producer")
		topic.AddCommand(c)
	}
	topicSet.Flags().String("installation", "", "Private Telegram installation UUID (required)")
	topicSet.Flags().Int64("revision", 0, "Expected revision from topic get; 0 creates a route (required)")
	topicSet.Flags().String("owner", "shadow", "Route owner: shadow, legacy, direct")
	topicSet.Flags().Bool("enabled", false, "Explicit topic consent")
	topicSet.Flags().Bool("legacy-stopped", false, "Confirm the old producer is stopped; required for direct activation")
	notificationCmd.AddCommand(topic)
	rollback := &cobra.Command{Use: "rollback <id>", Args: cobra.ExactArgs(1), Short: "Human only: return an unsent event to shadow", RunE: func(cmd *cobra.Command, args []string) error {
		return notificationRequest(cmd, notificationPath(cmd, args[0], "/rollback"), map[string]any{}, false)
	}}
	rollback.Flags().String("agent-id", "", "Agent UUID")
	notificationCmd.AddCommand(rollback)
	rootCmd.AddCommand(notificationCmd)
}

func notificationRequest(cmd *cobra.Command, path string, body any, get bool) error {
	method := "POST"
	if get {
		method = "GET"
	}
	return notificationMethodRequest(cmd, path, body, method)
}
func notificationMethodRequest(cmd *cobra.Command, path string, body any, method string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if method == "GET" {
		err = client.GetJSON(ctx, path, &out)
	} else if method == "PUT" {
		err = client.PutJSON(ctx, path, body, &out)
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

func runNotificationTopicGet(cmd *cobra.Command, _ []string) error {
	agent, _ := cmd.Flags().GetString("agent-id")
	producer, _ := cmd.Flags().GetString("producer")
	q := url.Values{"agent_id": {agent}, "producer": {producer}}
	return notificationMethodRequest(cmd, "/api/notification-topics?"+q.Encode(), nil, "GET")
}
func runNotificationTopicSet(cmd *cobra.Command, _ []string) error {
	installation, _ := cmd.Flags().GetString("installation")
	if installation == "" || !cmd.Flags().Changed("revision") {
		return errors.New("--installation and --revision are required")
	}
	agent, _ := cmd.Flags().GetString("agent-id")
	producer, _ := cmd.Flags().GetString("producer")
	revision, _ := cmd.Flags().GetInt64("revision")
	owner, _ := cmd.Flags().GetString("owner")
	enabled, _ := cmd.Flags().GetBool("enabled")
	stopped, _ := cmd.Flags().GetBool("legacy-stopped")
	return notificationMethodRequest(cmd, "/api/notification-topics", map[string]any{"agent_id": agent, "producer": producer, "installation_id": installation, "expected_revision": revision, "route_owner": owner, "enabled": enabled, "legacy_stopped": stopped}, "PUT")
}
