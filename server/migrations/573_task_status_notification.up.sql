-- Task-status notifications: a member opts in to hearing, from an agent's
-- private Telegram chat, that an issue reached a result status. Four tables,
-- no foreign keys (see AGENTS.md); every reader re-validates the rows it joins.
--
-- subscription: one row per (workspace, member, agent). Disabled by default;
-- only the member can enable it, for their own account. installation_id pins
-- the agent's Telegram installation the member's private chat belongs to.
CREATE TABLE IF NOT EXISTS task_status_notification_subscription (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    workspace_id    UUID NOT NULL,
    user_id         UUID NOT NULL,
    agent_id        UUID NOT NULL,
    installation_id UUID NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    statuses        TEXT[] NOT NULL DEFAULT ARRAY['blocked','in_review','done']::TEXT[],
    project_ids     UUID[] NOT NULL DEFAULT '{}'::UUID[],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- event: a durable capture of "an issue may have reached a result", written by
-- the triggers below in the same transaction as the write that caused it, so a
-- crash between commit and an in-process bus publish cannot lose it. Rows are
-- only written while the workspace has an enabled subscription. The processor
-- decides later (after available_at) whether the capture is still current.
CREATE TABLE IF NOT EXISTS task_status_notification_event (
    id             UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    workspace_id   UUID NOT NULL,
    issue_id       UUID NOT NULL,
    source         TEXT NOT NULL CHECK (source IN ('task', 'issue')),
    task_id        UUID,
    task_status    TEXT,
    issue_status   TEXT NOT NULL,
    issue_category TEXT NOT NULL,
    issue_revision BIGINT NOT NULL,
    actor_type     TEXT,
    actor_id       UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    available_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    claimed_at     TIMESTAMPTZ,
    processed_at   TIMESTAMPTZ,
    outcome        TEXT
);

-- notification: one per (subscription, dedup_key). It is the durable outbox
-- row for one canonical chat message: 'composing' while the agent writes the
-- message in its chat; the completion transaction that stores the agent's
-- reply moves it to 'pending' (or 'no_reply') atomically; the delivery worker
-- then owns it. 'sending' is published before any Telegram call; a send whose
-- result was lost ends in 'unknown' and is never retried automatically.
CREATE TABLE IF NOT EXISTS task_status_notification (
    id                 UUID NOT NULL PRIMARY KEY,
    workspace_id       UUID NOT NULL,
    subscription_id    UUID NOT NULL,
    user_id            UUID NOT NULL,
    agent_id           UUID NOT NULL,
    installation_id    UUID NOT NULL,
    event_id           UUID NOT NULL,
    issue_id           UUID NOT NULL,
    issue_status       TEXT NOT NULL,
    dedup_key          TEXT NOT NULL,
    chat_session_id    UUID,
    binding_id         UUID,
    route_revision     BIGINT,
    channel_chat_id    TEXT,
    input_message_id   UUID,
    compose_task_id    UUID,
    chat_message_id    UUID,
    state              TEXT NOT NULL CHECK (state IN (
        'composing', 'pending', 'sending', 'sent', 'unknown', 'failed',
        'no_reply', 'compose_failed', 'rejected'
    )),
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    chunks_sent        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    TIMESTAMPTZ,
    lease_token        UUID,
    lease_expires_at   TIMESTAMPTZ,
    last_error         TEXT,
    sent_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- message: the ledger of Telegram messages a notification produced, one row
-- per chunk. A reply to any chunk resolves to the one canonical chat message.
CREATE TABLE IF NOT EXISTS task_status_notification_message (
    installation_id    UUID NOT NULL,
    channel_chat_id    TEXT NOT NULL,
    channel_message_id TEXT NOT NULL,
    notification_id    UUID NOT NULL,
    chat_session_id    UUID NOT NULL,
    chat_message_id    UUID NOT NULL,
    chunk_index        INTEGER NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Effective category of a status key: built-in keys are their own category,
-- custom keys inherit theirs from the workspace catalog.
CREATE OR REPLACE FUNCTION task_status_notification_category(ws UUID, status_key TEXT) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(
        (SELECT s.category FROM issue_status s WHERE s.workspace_id = ws AND s.key = status_key LIMIT 1),
        status_key
    )
$$;

-- An agent run on an issue ended. The issue's status and the run's final
-- comment were written by the agent before the run reported its end, so they
-- are already visible here.
CREATE OR REPLACE FUNCTION capture_task_status_notification_from_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE iss RECORD; cat TEXT;
BEGIN
    SELECT i.id, i.workspace_id, i.status, i.revision INTO iss FROM issue i WHERE i.id = NEW.issue_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM task_status_notification_subscription sub
        WHERE sub.workspace_id = iss.workspace_id AND sub.enabled
    ) THEN
        RETURN NULL;
    END IF;
    cat := task_status_notification_category(iss.workspace_id, iss.status);
    INSERT INTO task_status_notification_event (
        workspace_id, issue_id, source, task_id, task_status,
        issue_status, issue_category, issue_revision, actor_type, actor_id
    ) VALUES (
        iss.workspace_id, iss.id, 'task', NEW.id, NEW.status,
        iss.status, cat, iss.revision, 'agent', NEW.agent_id
    );
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS task_status_notification_task ON agent_task_queue;
CREATE TRIGGER task_status_notification_task AFTER UPDATE OF status ON agent_task_queue FOR EACH ROW
    WHEN (NEW.issue_id IS NOT NULL
        AND OLD.status IS DISTINCT FROM NEW.status
        AND NEW.status IN ('completed', 'failed', 'cancelled'))
    EXECUTE FUNCTION capture_task_status_notification_from_task();

-- An issue reached a done-category status without an agent making the change
-- (a member, PR automation). Agent-made changes are captured when their run
-- ends instead, so the notification describes the run's final state.
CREATE OR REPLACE FUNCTION capture_task_status_notification_from_issue() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE actor_kind TEXT; actor UUID; cat TEXT;
BEGIN
    actor_kind := NULLIF(current_setting('multica.actor_type', true), '');
    IF actor_kind = 'agent' THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM task_status_notification_subscription sub
        WHERE sub.workspace_id = NEW.workspace_id AND sub.enabled
    ) THEN
        RETURN NULL;
    END IF;
    cat := task_status_notification_category(NEW.workspace_id, NEW.status);
    IF cat <> 'done' THEN
        RETURN NULL;
    END IF;
    BEGIN
        actor := NULLIF(current_setting('multica.actor_id', true), '')::UUID;
    EXCEPTION WHEN invalid_text_representation THEN
        actor := NULL;
    END;
    INSERT INTO task_status_notification_event (
        workspace_id, issue_id, source, issue_status, issue_category,
        issue_revision, actor_type, actor_id
    ) VALUES (
        NEW.workspace_id, NEW.id, 'issue', NEW.status, cat,
        NEW.revision, COALESCE(actor_kind, 'system'), actor
    );
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS task_status_notification_issue ON issue;
CREATE TRIGGER task_status_notification_issue AFTER UPDATE OF status ON issue FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION capture_task_status_notification_from_issue();
