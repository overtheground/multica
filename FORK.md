# About this fork

This repository is a fork of [multica-ai/multica](https://github.com/multica-ai/multica)
for one self-hosted instance. It is distributed under the unchanged Multica
License (`LICENSE`, `NOTICE`); the product is built on Multica. The files
listed below were changed or added by this fork. Everything else is upstream.

## Changes: task-status notifications in the agent's Telegram chat (MUL-11)

When an agent run on an issue ends with the issue in `blocked`, `in_review` or
`done`, or a non-agent actor moves an issue to a `done` status, the agent the
member subscribed to writes them a short message. The message goes into the
member's canonical private chat with that agent and is delivered to the
private Telegram chat with the same bot. Replies, including replies to old
notifications, carry the exact stored text back to the agent. The feature is
off for everyone until a member enables it for their own account:
`multica user task-notifications enable --agent <name>`, or
`PUT /api/task-status-notifications`.

The flow:

| Step | Where |
| --- | --- |
| Capture: triggers write durable events in the same transaction as the change | `server/migrations/573_task_status_notification.*` |
| Indexes, one per migration | `server/migrations/574`–`582` |
| Decide: settle window, staleness, dedup, rate limit, route checks | `server/internal/service/task_status_notification.go` |
| Compose: a hidden event input plus a chat turn of the agent | same file, `EnqueueTaskStatusNotificationTurn` |
| Finalize: the agent's reply and the outbox row commit together | `server/internal/service/task.go`, `CompleteTaskWithTransition` |
| Deliver: durable outbox, lease, 429, unknown and refused outcomes, ledger | `server/internal/integrations/telegram/notification_delivery.go` |
| Reply context: canonical text for replies and unanswered notifications | `server/internal/integrations/telegram/notification_context.go`, `resolvers.go` |
| Streaming path skips notification turns | `server/internal/integrations/telegram/outbound.go` |
| Hidden input kind | `server/pkg/protocol/messages.go`, `server/internal/handler/chat.go`, `server/pkg/db/queries/chat.sql` |
| Subscription API | `server/internal/handler/task_status_notification.go`, `server/cmd/server/router.go` |
| Worker wiring | `server/cmd/server/main.go`, `server/internal/handler/handler.go` |
| CLI | `server/cmd/multica/cmd_user_notifications.go` |
| Queries | `server/pkg/db/queries/task_status_notification.sql`, `workspace_delete.sql`, generated code in `server/pkg/db/generated/` |
| Index cleanup registry | `server/cmd/migrate/main.go` |

Telegram transcripts are now readable by agents: `SurfacePersistsTranscript`
includes Telegram (`server/internal/daemon/execenv/channel_type.go`). This part
is daemon-side. It takes effect only on daemons built from this fork; a stock
daemon still tells the agent that Telegram history is unreadable.

Tests:

- `server/internal/handler/task_status_notification_test.go`
- `server/internal/integrations/telegram/notification_context_test.go`
- `server/internal/daemon/prompt_task_status_notification_test.go`
- rows added to `server/internal/daemon/daemon_test.go`, `prompt_test.go`
  and `workspace_delete_manifest_test.go`

## Taking upstream updates

1. Fetch upstream and merge it into a new branch. Do not rewrite published
   branches.

   ```
   git remote add upstream https://github.com/multica-ai/multica   # once
   git fetch upstream
   git switch -c sync/<date> <current deploy branch>
   git merge upstream/main
   ```

2. Migration numbers: upstream keeps adding `server/migrations/NNN_*`. If
   upstream reuses numbers 573–582, rename this fork's files to numbers after
   upstream's last migration. Keep each concurrent index in its own file, and
   update the keys in `concurrentIndexCleanups` in `server/cmd/migrate/main.go`.
   Never renumber migrations a deployed database has already applied: check
   `schema_migrations` on the target first. If it already holds this fork's
   numbers, move upstream's conflicting files after them instead.
3. Regenerate queries with `make sqlc` (sqlc v1.31.1), then `go build ./...`.
4. Run the checks against a disposable database
   (`DATABASE_URL=... go run ./cmd/migrate up` first):

   ```
   cd server
   go test ./internal/handler/ -run 'TestTaskStatusNotification|TestWorkspaceDeletion' -count=1
   go test ./internal/integrations/telegram/ ./internal/service/ ./internal/daemon/... ./cmd/migrate/ -count=1
   go test ./... -count=1        # full backend suite before deploying
   ```

   `cmd/multica` tests must run outside a Multica agent task directory: the
   task marker switches the CLI into agent mode.
5. Hot spots to re-read after a merge:
   - `CompleteTaskWithTransition` and `writeChatCompletionOutcome` in
     `service/task.go`;
   - `enqueueChatTaskTx`: keep the lock order session → agent → binding →
     generation in `EnqueueTaskStatusNotificationTurn`;
   - Telegram `resolveTarget` and `sessionBinder`;
   - the workspace deletion manifest.

## Deploying a specific version

1. Build the backend image from a commit: `docker build -t multica-backend:<sha> .`
2. Back up the database before the first start.
3. Point only the backend service at that image. The entrypoint applies
   migrations on start. The web image can stay on its current version.
4. Roll back by switching the image back. The migrations here only add
   tables, functions and triggers, and have `down` files. To stop the
   feature without a deploy, disable every subscription: the triggers write
   nothing while none is enabled.
