CREATE INDEX CONCURRENTLY IF NOT EXISTS task_status_notification_session_idx ON task_status_notification (chat_session_id, sent_at) WHERE state = 'sent';
