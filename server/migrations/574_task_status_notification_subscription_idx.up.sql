CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_subscription_member_agent_idx ON task_status_notification_subscription (workspace_id, user_id, agent_id);
