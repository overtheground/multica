-- Independent of issue lifecycle. New producers start in shadow mode. There
-- is deliberately no trigger that publishes to Telegram from these tables.
CREATE TABLE IF NOT EXISTS notification_event (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    user_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    producer TEXT NOT NULL,
    event_key TEXT NOT NULL,
    payload JSONB NOT NULL,
    subject TEXT NOT NULL,
    period TEXT NOT NULL,
    action TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    reset_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    urgency TEXT NOT NULL CHECK (urgency IN ('routine','urgent','unknown')),
    route_owner TEXT NOT NULL DEFAULT 'shadow' CHECK (route_owner IN ('legacy','shadow','direct')),
    state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','deferred','suppressed','shadow_send','reserved','sent','failed','unknown','legacy_owned')),
    decision_reason TEXT,
    ack_message_id UUID,
    retry_at TIMESTAMPTZ,
    first_ack_failure_at TIMESTAMPTZ,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS notification_awareness (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    user_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    subject TEXT NOT NULL,
    period TEXT NOT NULL,
    action TEXT NOT NULL,
    source_message_id UUID NOT NULL,
    source_session_id UUID NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    CHECK (expires_at > occurred_at)
);

CREATE TABLE IF NOT EXISTS notification_decision_audit (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    event_id UUID NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('send','defer','suppress')),
    reason TEXT NOT NULL,
    ack_message_id UUID,
    event_revision BIGINT NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('capture','authorize')),
    policy_version TEXT NOT NULL DEFAULT 'v1',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
