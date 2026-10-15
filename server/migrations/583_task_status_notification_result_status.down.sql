-- Restores migration 573's category-based capture (the MUL-19 bug).
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

DROP FUNCTION IF EXISTS task_status_notification_result_status(UUID, TEXT);
