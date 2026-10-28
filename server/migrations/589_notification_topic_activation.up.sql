ALTER TABLE notification_topic_route ADD COLUMN IF NOT EXISTS eligible_after TIMESTAMPTZ NOT NULL DEFAULT '-infinity';
ALTER TABLE notification_topic_route ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp();
CREATE TABLE IF NOT EXISTS notification_topic_route_audit (
 id UUID PRIMARY KEY,
 workspace_id UUID NOT NULL,
 route_id UUID NOT NULL,
 actor_user_id UUID NOT NULL,
 revision BIGINT NOT NULL,
 previous_owner TEXT NOT NULL,
 route_owner TEXT NOT NULL,
 enabled BOOLEAN NOT NULL,
 reason TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
