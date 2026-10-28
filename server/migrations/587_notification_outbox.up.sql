-- Dormant generic transport. No subscriptions are inferred from issue delivery.
CREATE TABLE IF NOT EXISTS notification_topic_route (
 id UUID PRIMARY KEY,
 workspace_id UUID NOT NULL,
 user_id UUID NOT NULL,
 agent_id UUID NOT NULL,
 producer TEXT NOT NULL,
 installation_id UUID NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 route_owner TEXT NOT NULL DEFAULT 'shadow' CHECK (route_owner IN ('legacy','shadow','direct')),
 revision BIGINT NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS notification_outbox (
 event_id UUID PRIMARY KEY,
 workspace_id UUID NOT NULL,
 route_id UUID NOT NULL,
 route_revision BIGINT NOT NULL,
 installation_id UUID NOT NULL,
 chat_session_id UUID NOT NULL,
 chat_message_id UUID NOT NULL,
 channel_chat_id TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sending','sent','suppressed','failed','unknown')),
 lease_token UUID,
 lease_until TIMESTAMPTZ,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 message_ids TEXT[] NOT NULL DEFAULT '{}',
 last_error TEXT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
