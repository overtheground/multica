CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS notification_event_identity ON notification_event(workspace_id,user_id,agent_id,producer,event_key);
