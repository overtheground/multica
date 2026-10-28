CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS notification_topic_route_identity ON notification_topic_route(workspace_id,user_id,agent_id,producer);
