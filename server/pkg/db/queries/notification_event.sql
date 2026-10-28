-- name: CaptureNotificationEvent :one
INSERT INTO notification_event (
 id,workspace_id,user_id,agent_id,producer,event_key,payload,subject,period,action,
 observed_at,reset_at,fresh_until,urgency,route_owner
) VALUES (
 @id, @workspace_id, @user_id, @agent_id, @producer, @event_key, @payload, @subject, @period, @action,
 @observed_at, @reset_at, @fresh_until, @urgency, @route_owner
)
ON CONFLICT (workspace_id,user_id,agent_id,producer,event_key)
DO UPDATE SET event_key=EXCLUDED.event_key
WHERE notification_event.payload=EXCLUDED.payload
RETURNING *;

-- name: GetNotificationEvent :one
SELECT * FROM notification_event WHERE workspace_id= @workspace_id AND id= @id;

-- name: LockNotificationEvent :one
SELECT * FROM notification_event WHERE workspace_id= @workspace_id AND id= @id FOR UPDATE;

-- name: LockNotificationScope :exec
SELECT pg_advisory_xact_lock(hashtextextended( @scope_key::text,0));

-- name: GetNotificationAwareness :one
SELECT * FROM notification_awareness WHERE workspace_id= @workspace_id
 AND user_id= @user_id AND agent_id= @agent_id AND subject= @subject AND period= @period;

-- name: UpsertNotificationAwareness :one
INSERT INTO notification_awareness (
 id,workspace_id,user_id,agent_id,subject,period,action,source_message_id,
 source_session_id,occurred_at,expires_at
) VALUES (
 @id, @workspace_id, @user_id, @agent_id, @subject, @period, @action, @source_message_id,
 @source_session_id, @occurred_at, @expires_at
)
ON CONFLICT (workspace_id,user_id,agent_id,subject,period)
DO UPDATE SET action=EXCLUDED.action, source_message_id=EXCLUDED.source_message_id,
 source_session_id=EXCLUDED.source_session_id, occurred_at=EXCLUDED.occurred_at,
 expires_at=EXCLUDED.expires_at, revision=notification_awareness.revision+1
WHERE notification_awareness.occurred_at<EXCLUDED.occurred_at
RETURNING *;

-- name: RecordNotificationDecision :one
UPDATE notification_event SET state= @state,decision_reason= @decision_reason,
 ack_message_id=sqlc.narg('ack_message_id'),retry_at=sqlc.narg('retry_at'),
 first_ack_failure_at=COALESCE(first_ack_failure_at,sqlc.narg('first_ack_failure_at')),
 revision=revision+1,updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND id= @id AND revision= @expected_revision
RETURNING *;

-- name: AppendNotificationDecisionAudit :exec
INSERT INTO notification_decision_audit(
 id,workspace_id,event_id,verdict,reason,ack_message_id,event_revision,phase
) VALUES ( @id, @workspace_id, @event_id, @verdict, @reason,sqlc.narg('ack_message_id'), @event_revision, @phase);

-- name: ListNotificationDecisionAudit :many
SELECT * FROM notification_decision_audit WHERE workspace_id= @workspace_id AND event_id= @event_id
ORDER BY created_at,id LIMIT 100;

-- name: GetNotificationAckSource :one
SELECT m.id,m.chat_session_id,m.content,m.role,m.message_kind,m.created_at
FROM chat_message m JOIN chat_session s ON s.id=m.chat_session_id
WHERE m.id= @message_id AND s.id= @session_id AND s.workspace_id= @workspace_id
 AND s.creator_id= @user_id AND s.agent_id= @agent_id AND s.status='active'
 AND m.role='user' AND m.message_kind='message';

-- name: GetNotificationTopicRoute :one
SELECT * FROM notification_topic_route WHERE workspace_id= @workspace_id AND user_id= @user_id
 AND agent_id= @agent_id AND producer= @producer FOR SHARE;

-- name: InsertNotificationOutbox :one
INSERT INTO notification_outbox(event_id,workspace_id,route_id,route_revision,installation_id,
 chat_session_id,chat_message_id,channel_chat_id)
VALUES ( @event_id, @workspace_id, @route_id, @route_revision, @installation_id,
 @chat_session_id, @chat_message_id, @channel_chat_id) RETURNING *;

-- name: GetNotificationOutbox :one
SELECT * FROM notification_outbox WHERE workspace_id= @workspace_id AND event_id= @event_id;

-- name: ReserveNotificationOutbox :one
UPDATE notification_outbox SET state='sending',lease_token= @lease_token,
 lease_until=clock_timestamp()+interval '60 seconds',updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND event_id= @event_id AND state='pending'
 AND next_attempt_at<=clock_timestamp() RETURNING *;

-- name: ExpireNotificationOutbox :exec
WITH expired AS (
 UPDATE notification_outbox SET state='unknown',last_error='abandoned send; outcome unknown',updated_at=clock_timestamp()
 WHERE state='sending' AND lease_until<clock_timestamp() RETURNING workspace_id,event_id
), updated AS (
 UPDATE notification_event e SET state='unknown',decision_reason='abandoned_send_unknown',revision=e.revision+1,updated_at=clock_timestamp()
 FROM expired x WHERE e.id=x.event_id AND e.workspace_id=x.workspace_id RETURNING e.*
)
INSERT INTO notification_decision_audit(id,workspace_id,event_id,verdict,reason,event_revision,phase)
SELECT gen_random_uuid(),workspace_id,id,'send','abandoned_send_unknown',revision,'authorize' FROM updated;

-- name: SetNotificationOutboxOutcome :execrows
UPDATE notification_outbox SET state= @state,last_error= @last_error,
 next_attempt_at= @next_attempt_at,message_ids= @message_ids,updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND event_id= @event_id AND state='sending' AND lease_token= @lease_token;

-- name: RecordNotificationOutboxChunk :execrows
UPDATE notification_outbox SET message_ids=array_append(message_ids, @message_id::text),updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND event_id= @event_id AND state='sending' AND lease_token= @lease_token
 AND cardinality(message_ids)= @chunk_index::integer;

-- name: SetNotificationRouteOwner :execrows
UPDATE notification_event SET route_owner= @route_owner,revision=revision+1,updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND id= @id AND revision= @expected_revision
 AND route_owner= @expected_owner AND state IN ('queued','shadow_send','deferred');

-- name: GetNotificationTopicRouteForUpdate :one
SELECT * FROM notification_topic_route WHERE workspace_id= @workspace_id AND user_id= @user_id
 AND agent_id= @agent_id AND producer= @producer FOR UPDATE;

-- name: CreateNotificationTopicRoute :one
INSERT INTO notification_topic_route(id,workspace_id,user_id,agent_id,producer,installation_id)
VALUES ( @id, @workspace_id, @user_id, @agent_id, @producer, @installation_id)
ON CONFLICT (workspace_id,user_id,agent_id,producer) DO NOTHING RETURNING *;

-- name: UpdateNotificationTopicRoute :one
UPDATE notification_topic_route SET installation_id= @installation_id,enabled= @enabled,
 route_owner= @route_owner,eligible_after= @eligible_after,revision=revision+1,updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND id= @id AND revision= @expected_revision RETURNING *;

-- name: AppendNotificationTopicRouteAudit :exec
INSERT INTO notification_topic_route_audit(id,workspace_id,route_id,actor_user_id,revision,previous_owner,route_owner,enabled,reason)
VALUES ( @id, @workspace_id, @route_id, @actor_user_id, @revision, @previous_owner, @route_owner, @enabled, @reason);

-- name: ListNotificationTopicRouteAudit :many
SELECT * FROM notification_topic_route_audit WHERE workspace_id= @workspace_id AND route_id= @route_id
ORDER BY created_at,id LIMIT 100;

-- name: CountNotificationLegacyInFlight :one
SELECT count(*) FROM task_status_notification WHERE workspace_id= @workspace_id AND user_id= @user_id AND agent_id= @agent_id
 AND state IN ('composing','pending','sending','unknown');

-- name: ListNotificationShadowCandidates :many
SELECT e.* FROM notification_event e JOIN notification_topic_route r
ON r.workspace_id=e.workspace_id AND r.user_id=e.user_id AND r.agent_id=e.agent_id AND r.producer=e.producer
WHERE r.enabled AND r.route_owner='direct' AND e.route_owner='shadow'
 AND e.state IN ('queued','shadow_send','deferred') AND (e.retry_at IS NULL OR e.retry_at<=clock_timestamp())
 AND e.created_at>=r.eligible_after ORDER BY e.created_at,e.id LIMIT @batch_size;

-- name: ListNotificationOutboxDue :many
SELECT o.workspace_id,o.event_id FROM notification_outbox o JOIN notification_topic_route r
ON r.id=o.route_id AND r.workspace_id=o.workspace_id AND r.revision=o.route_revision
WHERE o.state='pending' AND o.next_attempt_at<=clock_timestamp() AND r.enabled AND r.route_owner='direct'
ORDER BY o.next_attempt_at,o.event_id LIMIT @batch_size;

-- name: CountNotificationDirectUnsettled :one
SELECT count(*) FROM notification_outbox o JOIN notification_event e ON e.id=o.event_id AND e.workspace_id=o.workspace_id
WHERE e.workspace_id= @workspace_id AND e.user_id= @user_id AND e.agent_id= @agent_id AND e.producer= @producer
 AND (o.state IN ('pending','sending','unknown') OR (cardinality(o.message_ids)>0 AND o.state<>'sent'));
