CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS notification_awareness_identity ON notification_awareness(workspace_id,user_id,agent_id,subject,period);
