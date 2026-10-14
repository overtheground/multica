CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_compose_task_idx ON task_status_notification (compose_task_id) WHERE compose_task_id IS NOT NULL;
