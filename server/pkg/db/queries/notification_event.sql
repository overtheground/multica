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
  AND notification_event.route_owner=EXCLUDED.route_owner
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
