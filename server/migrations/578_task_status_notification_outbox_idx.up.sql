CREATE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_outbox_idx ON task_status_notification (next_attempt_at) WHERE state IN ('pending', 'sending');
