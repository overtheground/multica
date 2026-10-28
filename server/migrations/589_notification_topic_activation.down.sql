DROP TABLE IF EXISTS notification_topic_route_audit;
ALTER TABLE notification_topic_route DROP COLUMN IF EXISTS eligible_after;
ALTER TABLE notification_topic_route DROP COLUMN IF EXISTS updated_at;
