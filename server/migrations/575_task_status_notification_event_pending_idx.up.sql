CREATE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_event_pending_idx ON task_status_notification_event (available_at) WHERE processed_at IS NULL;
