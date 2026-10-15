-- Task-status notifications: an agent tells a member, in their private
-- Telegram chat with that agent, that an issue reached a result status.
-- See migrations/573_task_status_notification.up.sql for the table contracts.

-- =====================
-- subscription
-- =====================

-- name: UpsertTaskStatusNotificationSubscription :one
INSERT INTO task_status_notification_subscription (
    workspace_id, user_id, agent_id, installation_id, enabled, statuses, project_ids
) VALUES (
    @workspace_id, @user_id, @agent_id, @installation_id, @enabled,
    @statuses::text[], @project_ids::uuid[]
)
ON CONFLICT (workspace_id, user_id, agent_id) DO UPDATE SET
    installation_id = EXCLUDED.installation_id,
    enabled = EXCLUDED.enabled,
    statuses = EXCLUDED.statuses,
    project_ids = EXCLUDED.project_ids,
    updated_at = now()
RETURNING *;

-- name: DisableTaskStatusNotificationSubscription :one
UPDATE task_status_notification_subscription
SET enabled = FALSE, updated_at = now()
WHERE workspace_id = @workspace_id AND user_id = @user_id AND agent_id = @agent_id
RETURNING *;

-- name: ListTaskStatusNotificationSubscriptionsForUser :many
SELECT * FROM task_status_notification_subscription
WHERE workspace_id = @workspace_id AND user_id = @user_id
ORDER BY created_at;

-- name: ListEnabledTaskStatusNotificationSubscriptions :many
SELECT * FROM task_status_notification_subscription
WHERE workspace_id = @workspace_id AND enabled
ORDER BY created_at;

-- name: GetTaskStatusNotificationSubscription :one
SELECT * FROM task_status_notification_subscription
WHERE id = @id;

-- =====================
-- event
-- =====================

-- name: ClaimTaskStatusNotificationEvents :many
-- Claims captured events whose settle window has passed. A claim older than
-- five minutes is treated as abandoned by a process that died mid-way.
UPDATE task_status_notification_event AS e
SET claimed_at = now()
WHERE e.id IN (
    SELECT pending.id FROM task_status_notification_event AS pending
    WHERE pending.processed_at IS NULL
      AND pending.available_at <= @ready_before::timestamptz
      AND (pending.claimed_at IS NULL OR pending.claimed_at < now() - interval '5 minutes')
    ORDER BY pending.created_at
    FOR UPDATE SKIP LOCKED
    LIMIT @batch_size
)
RETURNING e.*;

-- name: MarkTaskStatusNotificationEventProcessed :exec
UPDATE task_status_notification_event
SET processed_at = now(), outcome = @outcome
WHERE id = @id;

-- name: HasNewerTaskStatusNotificationEvent :one
-- Events of one issue can be processed out of order; a newer capture that
-- itself carries a result describes the issue's current result. A newer run
-- that was cancelled or left no result comment carries none, so it does not
-- hide the older one.
SELECT EXISTS (
    SELECT 1 FROM task_status_notification_event AS newer
    WHERE newer.issue_id = @issue_id
      AND newer.id <> @id
      AND (newer.created_at, newer.id) > (@created_at::timestamptz, @id::uuid)
      AND (
          newer.source = 'issue'
          OR (
              newer.task_status IS DISTINCT FROM 'cancelled'
              AND EXISTS (
                  SELECT 1 FROM comment AS c
                  WHERE c.issue_id = newer.issue_id
                    AND c.source_task_id = newer.task_id
                    AND c.author_type = 'agent'
                    AND c.type = 'comment'
                    AND c.deleted_at IS NULL
              )
          )
      )
)::boolean AS has_newer;

-- name: CountActiveIssueAgentTasks :one
-- A run still queued or executing on the issue means the status seen now may
-- be an intermediate one. A deferred run counts only when it is due soon (an
-- automatic retry); a wakeup scheduled for days ahead does not hold the
-- result back.
SELECT count(*) FROM agent_task_queue
WHERE issue_id = @issue_id
  AND (
      status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
      OR (status = 'deferred' AND (fire_at IS NULL OR fire_at < now() + interval '10 minutes'))
  );

-- name: DeferTaskStatusNotificationEvent :exec
-- Judge the event again later: a run on the issue is still active, or the
-- route is briefly unavailable.
UPDATE task_status_notification_event
SET available_at = @available_at, claimed_at = NULL
WHERE id = @id;

-- name: PruneTaskStatusNotificationEvents :execrows
-- Judged events are only an audit trail; keep a week of it.
DELETE FROM task_status_notification_event
WHERE id IN (
    SELECT old.id FROM task_status_notification_event AS old
    WHERE old.processed_at < now() - interval '7 days'
    LIMIT 500
);

-- name: GetLatestAgentCommentForTask :one
SELECT * FROM comment
WHERE issue_id = @issue_id
  AND source_task_id = @task_id
  AND author_type = 'agent'
  AND type = 'comment'
  AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- =====================
-- notification
-- =====================

-- name: InsertTaskStatusNotification :one
-- A repeated or concurrent processing of the same result returns no row,
-- which is how the caller learns it is a duplicate.
INSERT INTO task_status_notification (
    id, workspace_id, subscription_id, user_id, agent_id, installation_id,
    event_id, issue_id, issue_status, dedup_key, state, last_error
) VALUES (
    @id, @workspace_id, @subscription_id, @user_id, @agent_id, @installation_id,
    @event_id, @issue_id, @issue_status, @dedup_key, @state, sqlc.narg('last_error')
)
ON CONFLICT (subscription_id, dedup_key) DO UPDATE SET
    event_id = EXCLUDED.event_id,
    state = EXCLUDED.state,
    last_error = EXCLUDED.last_error,
    updated_at = now()
-- A refusal does not use up the result: once the route is fixed, a replayed
-- event may compose it. Anything else already holds this result.
WHERE task_status_notification.state = 'rejected'
RETURNING *;

-- name: SetTaskStatusNotificationComposing :one
UPDATE task_status_notification
SET chat_session_id = @chat_session_id,
    binding_id = @binding_id,
    route_revision = @route_revision,
    channel_chat_id = @channel_chat_id,
    input_message_id = @input_message_id,
    compose_task_id = @compose_task_id,
    updated_at = now()
WHERE id = @id AND state = 'composing'
RETURNING *;

-- name: GetLatestTaskStatusNotificationForIssue :one
-- The newest notification this subscription produced for an issue that
-- actually reached (or is about to reach) the member.
SELECT * FROM task_status_notification
WHERE subscription_id = @subscription_id
  AND issue_id = @issue_id
  AND state IN ('composing', 'pending', 'sending', 'sent', 'unknown')
ORDER BY created_at DESC
LIMIT 1;

-- name: CountRecentTaskStatusNotifications :one
-- Notifications handed to the agent for one subscription since a moment;
-- the rate limit. Refusals never reached the agent and do not count.
SELECT count(*) FROM task_status_notification
WHERE subscription_id = @subscription_id
  AND created_at >= @since::timestamptz
  AND state <> 'rejected';

-- name: GetTaskStatusNotificationForComposeTask :one
-- Resolves the notification an agent turn is composing. An automatic retry
-- runs under a new task id but keeps the original input owner, so the lookup
-- goes through chat_input_task_id.
SELECT n.* FROM task_status_notification AS n
WHERE n.compose_task_id = @input_owner_task_id
FOR UPDATE;

-- name: IsTaskStatusNotificationComposeTask :one
SELECT EXISTS (
    SELECT 1 FROM task_status_notification AS n
    WHERE n.compose_task_id = COALESCE(
        (SELECT t.chat_input_task_id FROM agent_task_queue AS t WHERE t.id = @task_id),
        @task_id::uuid
    )
)::boolean AS is_notification;

-- name: AttachTaskStatusNotificationReply :one
UPDATE task_status_notification
SET chat_message_id = sqlc.narg('chat_message_id'),
    state = @state,
    last_error = sqlc.narg('last_error'),
    next_attempt_at = CASE WHEN @state::text = 'pending' THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = @id AND state = 'composing'
RETURNING *;

-- name: FailStaleTaskStatusNotificationCompositions :many
-- A composing notification whose agent turn ended without a stored reply (the
-- run failed, was cancelled, or its retries were exhausted) is recorded as
-- compose_failed. Completion moves a notification out of 'composing' in the
-- same transaction that stores the reply, so a composing row with no live
-- task in its retry chain can never still receive one.
UPDATE task_status_notification AS n
SET state = 'compose_failed',
    last_error = 'the agent turn ended without a reply',
    updated_at = now()
WHERE n.state = 'composing'
  AND n.compose_task_id IS NOT NULL
  AND n.updated_at < now() - interval '1 minute'
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_queue AS t
      WHERE (t.id = n.compose_task_id OR t.chat_input_task_id = n.compose_task_id)
        AND t.status NOT IN ('completed', 'failed', 'cancelled')
  )
RETURNING n.*;

-- name: ExpireTaskStatusNotificationSends :many
-- A send whose owner stopped mid-call: nobody knows whether Telegram posted
-- it, and Telegram offers no idempotency key, so it is never re-sent.
UPDATE task_status_notification
SET state = 'unknown',
    lease_token = NULL,
    lease_expires_at = NULL,
    last_error = COALESCE(last_error, 'delivery owner stopped while a send was in flight'),
    updated_at = now()
WHERE state = 'sending' AND lease_expires_at < now()
RETURNING *;

-- name: ClaimTaskStatusNotificationDelivery :one
UPDATE task_status_notification AS n
SET state = 'sending',
    lease_token = gen_random_uuid(),
    lease_expires_at = now() + make_interval(secs => @lease_seconds::float8),
    attempt_count = n.attempt_count + 1,
    updated_at = now()
WHERE n.id = (
    SELECT due.id FROM task_status_notification AS due
    WHERE due.state = 'pending'
      AND (due.next_attempt_at IS NULL OR due.next_attempt_at <= now())
    ORDER BY due.created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING n.*;

-- name: RenewTaskStatusNotificationLease :execrows
UPDATE task_status_notification
SET lease_expires_at = now() + make_interval(secs => @lease_seconds::float8),
    updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND state = 'sending';

-- name: RecordTaskStatusNotificationChunk :execrows
UPDATE task_status_notification
SET chunks_sent = @chunks_sent, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND state = 'sending';

-- name: InsertTaskStatusNotificationMessage :exec
INSERT INTO task_status_notification_message (
    installation_id, channel_chat_id, channel_message_id, notification_id,
    chat_session_id, chat_message_id, chunk_index
) VALUES (
    @installation_id, @channel_chat_id, @channel_message_id, @notification_id,
    @chat_session_id, @chat_message_id, @chunk_index
)
ON CONFLICT (installation_id, channel_message_id) DO NOTHING;

-- name: CompleteTaskStatusNotificationSend :execrows
UPDATE task_status_notification
SET state = 'sent', sent_at = now(), lease_token = NULL, lease_expires_at = NULL,
    last_error = NULL, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND state = 'sending';

-- name: DeferTaskStatusNotificationSend :execrows
UPDATE task_status_notification
SET state = 'pending', next_attempt_at = @next_attempt_at, lease_token = NULL,
    lease_expires_at = NULL, last_error = @last_error, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND state = 'sending';

-- name: FailTaskStatusNotificationSend :execrows
UPDATE task_status_notification
SET state = 'failed', lease_token = NULL, lease_expires_at = NULL,
    last_error = @last_error, updated_at = now()
WHERE id = @id AND lease_token = @lease_token AND state = 'sending';

-- name: MarkTaskStatusNotificationSendUnknown :execrows
-- Deliberately not owner-checked: the outcome has to land even when the lease
-- expired while the request hung, or a successor could re-send.
UPDATE task_status_notification
SET state = 'unknown', lease_token = NULL, lease_expires_at = NULL,
    last_error = @last_error, updated_at = now()
WHERE id = @id AND state = 'sending';

-- name: SetChatMessageChannelOutbound :exec
UPDATE chat_message
SET channel_outbound_type = @channel_outbound_type,
    channel_outbound_installation_id = @channel_outbound_installation_id,
    channel_outbound_chat_id = @channel_outbound_chat_id,
    channel_outbound_message_ids = @channel_outbound_message_ids::text[]
WHERE id = @id;

-- name: ListTaskStatusNotificationMessageIDs :many
SELECT channel_message_id FROM task_status_notification_message
WHERE notification_id = @notification_id
ORDER BY chunk_index;

-- name: GetTaskStatusNotification :one
SELECT * FROM task_status_notification WHERE id = @id;

-- name: GetTaskStatusNotificationResultStatus :one
-- The status key an issue's status counts as for notifications: built-ins are
-- themselves, custom done/closed statuses are done/cancelled, other custom
-- statuses stay their own key (migration 582).
SELECT task_status_notification_result_status(@workspace_id::uuid, @status_key::text)::text AS result_status;

-- name: CreateChannelTaskDeliveryForNotification :one
-- A notification turn speaks first: it answers no inbound message, so unlike
-- CreateChannelTaskDeliveryFromSession it carries no trigger (no quote, no
-- thread, no sender). The row tells the runtime which surface the turn is
-- written for; the reply itself is delivered by the notification outbox.
INSERT INTO channel_task_delivery (
    task_id, binding_id, installation_id, channel_type, channel_chat_id, chat_type,
    route_revision, config
)
SELECT @task_id, binding.id, binding.installation_id, binding.channel_type,
       binding.channel_chat_id, binding.chat_type, binding.route_revision, binding.config
FROM channel_chat_session_binding AS binding
WHERE binding.id = @binding_id
RETURNING *;

-- =====================
-- delivery route
-- =====================

-- name: GetChannelUserBindingForMemberOnInstallation :one
SELECT * FROM channel_user_binding
WHERE installation_id = @installation_id
  AND multica_user_id = @multica_user_id
  AND channel_type = @channel_type
ORDER BY bound_at DESC
LIMIT 1;

-- =====================
-- inbound reply context
-- =====================

-- name: GetTaskStatusNotificationByChannelMessage :one
-- A Telegram reply names the bot message it answers; this resolves it to the
-- canonical chat message and the notification it delivered.
SELECT
    m.notification_id, m.chat_session_id, m.chat_message_id, m.chunk_index,
    n.issue_id, n.issue_status, n.sent_at, n.user_id, n.workspace_id,
    cm.content, cm.created_at AS message_created_at
FROM task_status_notification_message AS m
JOIN task_status_notification AS n ON n.id = m.notification_id
JOIN chat_message AS cm ON cm.id = m.chat_message_id
WHERE m.installation_id = @installation_id
  AND m.channel_message_id = @channel_message_id;
