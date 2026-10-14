package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Task-status notifications are a per-user knob: a member opts in, for their
// own account, to hearing from an agent in their private Telegram chat with
// it when an issue reaches blocked / in_review / done. The server refuses
// these calls from an agent's task token.

var userTaskNotificationsCmd = &cobra.Command{
	Use:   "task-notifications",
	Short: "Hear about issue results from an agent in your private Telegram chat",
	Long: "When enabled for an agent, that agent writes you a short message in your " +
		"private Telegram chat with its bot whenever an issue in this workspace " +
		"reaches blocked, in_review or done. Off by default. You must have linked " +
		"your Telegram account with the agent's bot and written to it directly once.",
}

var userTaskNotificationsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show your task-status notification subscriptions",
	Args:  cobra.NoArgs,
	RunE:  runUserTaskNotificationsList,
}

var userTaskNotificationsEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Turn task-status notifications on for one agent",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runUserTaskNotificationsSet(cmd, true) },
}

var userTaskNotificationsDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Turn task-status notifications off for one agent",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runUserTaskNotificationsSet(cmd, false) },
}

func init() {
	userCmd.AddCommand(userTaskNotificationsCmd)
	userTaskNotificationsCmd.AddCommand(userTaskNotificationsListCmd)
	userTaskNotificationsCmd.AddCommand(userTaskNotificationsEnableCmd)
	userTaskNotificationsCmd.AddCommand(userTaskNotificationsDisableCmd)

	userTaskNotificationsListCmd.Flags().String("output", "table", "Output format: table or json")
	for _, c := range []*cobra.Command{userTaskNotificationsEnableCmd, userTaskNotificationsDisableCmd} {
		c.Flags().String("agent", "", "Agent name or ID (required)")
		c.Flags().String("output", "table", "Output format: table or json")
	}
	userTaskNotificationsEnableCmd.Flags().StringSlice("status", nil, "Result statuses to hear about: blocked, in_review, done (default: all three)")
	userTaskNotificationsEnableCmd.Flags().StringSlice("project", nil, "Only issues in these project IDs (default: every project)")
}

func runUserTaskNotificationsList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var resp map[string]any
	if err := client.GetJSON(ctx, "/api/task-status-notifications", &resp); err != nil {
		return fmt.Errorf("list task notifications: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	subs, _ := resp["subscriptions"].([]any)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "AGENT\tENABLED\tSTATUSES\tPROJECTS")
	for _, raw := range subs {
		s, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", strVal(s, "agent_id"), s["enabled"], joinAny(s["statuses"]), joinAny(s["project_ids"]))
	}
	return nil
}

func runUserTaskNotificationsSet(cmd *cobra.Command, enabled bool) error {
	agentRef, _ := cmd.Flags().GetString("agent")
	if strings.TrimSpace(agentRef) == "" {
		return fmt.Errorf("--agent is required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	agentID, err := resolveAgent(ctx, client, agentRef)
	if err != nil {
		return err
	}
	body := map[string]any{"agent_id": agentID, "enabled": enabled}
	if enabled {
		statuses, _ := cmd.Flags().GetStringSlice("status")
		projects, _ := cmd.Flags().GetStringSlice("project")
		if len(statuses) > 0 {
			body["statuses"] = statuses
		}
		if len(projects) > 0 {
			body["project_ids"] = projects
		}
	}
	var resp map[string]any
	if err := client.PutJSON(ctx, "/api/task-status-notifications", body, &resp); err != nil {
		return fmt.Errorf("update task notifications: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, resp)
	}
	sub, _ := resp["subscription"].(map[string]any)
	if sub == nil {
		fmt.Fprintln(os.Stdout, "Task notifications are off for this agent.")
		return nil
	}
	if enabled {
		fmt.Fprintf(os.Stdout, "Task notifications enabled for agent %s (statuses: %s).\n", strVal(sub, "agent_id"), joinAny(sub["statuses"]))
	} else {
		fmt.Fprintf(os.Stdout, "Task notifications disabled for agent %s.\n", strVal(sub, "agent_id"))
	}
	return nil
}

func joinAny(v any) string {
	items, _ := v.([]any)
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, fmt.Sprint(item))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
