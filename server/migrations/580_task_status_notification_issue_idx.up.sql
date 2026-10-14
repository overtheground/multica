CREATE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_issue_idx ON task_status_notification (subscription_id, issue_id, created_at);
