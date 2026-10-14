CREATE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_event_issue_idx ON task_status_notification_event (issue_id, created_at);
