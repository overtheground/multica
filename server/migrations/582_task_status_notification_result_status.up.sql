-- Task-status notifications judge an issue by its RESULT STATUS, not by its
-- catalog category (MUL-19). Since migrations 469/492 issue_status.category is
-- a lifecycle category (unstarted, started, done, closed): in_progress,
-- in_review and blocked all sit in 'started', so the category cannot tell a
-- result from work in progress. task_status_notification_category returned that
-- category and every built-in in_review/blocked result was suppressed as
-- not_result_status.
--
-- The result status is issue_effective_status (migration 494): a built-in key
-- is itself; a custom status in 'done' is done and in 'closed' is cancelled;
-- any other custom status stays its own key. Custom statuses inherit only
-- lifecycle, never built-in review or blocked behavior, so a custom 'started'
-- status is never a result.
--
-- task_status_notification_event.issue_category keeps its name and now holds
-- this result status.
CREATE OR REPLACE FUNCTION task_status_notification_result_status(ws UUID, status_key TEXT) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT issue_effective_status(ws, status_key)
$$;

CREATE OR REPLACE FUNCTION capture_task_status_notification_from_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE iss RECORD; result_status TEXT;
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
    result_status := task_status_notification_result_status(iss.workspace_id, iss.status);
    INSERT INTO task_status_notification_event (
        workspace_id, issue_id, source, task_id, task_status,
        issue_status, issue_category, issue_revision, actor_type, actor_id
    ) VALUES (
        iss.workspace_id, iss.id, 'task', NEW.id, NEW.status,
        iss.status, result_status, iss.revision, 'agent', NEW.agent_id
    );
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION capture_task_status_notification_from_issue() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE actor_kind TEXT; actor UUID; result_status TEXT;
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
    result_status := task_status_notification_result_status(NEW.workspace_id, NEW.status);
    IF result_status <> 'done' THEN
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
        NEW.workspace_id, NEW.id, 'issue', NEW.status, result_status,
        NEW.revision, COALESCE(actor_kind, 'system'), actor
    );
    RETURN NULL;
END $$;

-- task_status_notification_category stays in place, unused by this build: a
-- backend rolled back to the previous image still calls it, and keeping it
-- makes that rollback an image swap with no schema step.
