CREATE INDEX CONCURRENTLY IF NOT EXISTS notification_outbox_due ON notification_outbox(next_attempt_at,event_id) WHERE state='pending';
