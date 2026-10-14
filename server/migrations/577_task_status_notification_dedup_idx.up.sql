CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_dedup_idx ON task_status_notification (subscription_id, dedup_key);
