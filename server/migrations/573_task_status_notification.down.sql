DROP TRIGGER IF EXISTS task_status_notification_issue ON issue;
DROP TRIGGER IF EXISTS task_status_notification_task ON agent_task_queue;
DROP FUNCTION IF EXISTS capture_task_status_notification_from_issue();
DROP FUNCTION IF EXISTS capture_task_status_notification_from_task();
DROP FUNCTION IF EXISTS task_status_notification_category(UUID, TEXT);
DROP TABLE IF EXISTS task_status_notification_message;
DROP TABLE IF EXISTS task_status_notification;
DROP TABLE IF EXISTS task_status_notification_event;
DROP TABLE IF EXISTS task_status_notification_subscription;
