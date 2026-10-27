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
notifications, carry the exact stored text back to the agent. An ordinary
follow-up stays exactly as the member wrote it: the notification is already
the agent's own previous message in that chat. The feature is
off for everyone until a member enables it for their own account:
`multica user task-notifications enable --agent <name>`, or
`PUT /api/task-status-notifications`.

The flow:

| Step | Where |
| --- | --- |
| Capture: triggers write durable events in the same transaction as the change | `server/migrations/573_task_status_notification.*`; judged by result status since `583_task_status_notification_result_status.*` |
| Indexes, one per migration | `server/migrations/574`–`582` (`581_…session_idx` is no longer read by any query; it stays because deployed databases applied it) |
| Decide: settle window, staleness, dedup, rate limit, route checks | `server/internal/service/task_status_notification.go` |
| Compose: a hidden event input plus a chat turn of the agent | same file, `EnqueueTaskStatusNotificationTurn` |
| Finalize: the agent's reply and the outbox row commit together | `server/internal/service/task.go`, `CompleteTaskWithTransition` |
| Deliver: durable outbox, lease, 429, unknown and refused outcomes, ledger | `server/internal/integrations/telegram/notification_delivery.go` |
| Reply context: canonical text for a reply to a notification | `server/internal/integrations/telegram/notification_context.go`, `resolvers.go` |
| Chat history for a run that cannot resume its session: the claim ships the stored conversation, the daemon renders it only without a resumed session (MUL-14) | `server/internal/handler/chat_prompt_history.go`, `daemon.go`, `agent.go`; `server/internal/daemon/prompt.go`, `types.go` |
| `multica chat history` omits the hidden event input | `server/internal/handler/chat_history.go` |
| Streaming path skips notification turns | `server/internal/integrations/telegram/outbound.go` (`resolveTarget`) |
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

The chat history a fresh run starts from (`chat_history` on the claim) is
likewise rendered only by a daemon built from this fork. A stock daemon
ignores the field: its turns still resume their session as before, but a run
that starts without one sees only the new message.

Tests:

- `server/internal/handler/task_status_notification_test.go`
- `server/internal/handler/chat_prompt_history_test.go`, a row in
  `chat_history_test.go`
- `server/internal/integrations/telegram/notification_context_test.go`
- `server/internal/daemon/prompt_task_status_notification_test.go`
- rows added to `server/internal/daemon/daemon_test.go`, `prompt_test.go`
  and `workspace_delete_manifest_test.go`

## Changes: one Telegram status message per turn (MUL-32)

While an agent run works, its Telegram turn shows one status message instead
of the raw streamed transcript, and the final answer replaces it in the same
message.

- Cause of the old single-letter placeholder: the daemon flushes the first
  visible frame immediately, and Codex streams token deltas, so that frame was
  the first token alone. The rest of the sentence arrived inside the chat's
  2.5s edit cooldown and was dropped with no deferred send, so a run that went
  on into tool calls showed the letter until the final answer.
- The status shows `⏳ Работаю над ответом…` until the agent finishes a
  message, then that whole message (a non-text frame proves it ended; capped
  at 400 characters on a sentence or word boundary). After a minute in one
  stage it adds `Всё ещё выполняется · N мин`, only while `agent_task_queue`
  says the task is `running`.
- A ticker (1s, in-memory unless something is due) sends what met the
  cooldown and the elapsed line. Every status call runs under the turn's
  streaming lease, which the final answer, the failure notice and the close
  outrank, so a late status can never overwrite them.
- A cancelled run or one that completes empty rewrites an existing status to
  `⏹ Остановлено.` / `✅ Готово.`; with no status in the chat nothing is posted.
- Code: `server/internal/integrations/telegram/stream_status.go`, with the
  frame handler and the close path in `outbound.go`. No schema change.
- Tests: `stream_status_test.go`; adjusted streaming rows in `outbound_test.go`,
  `outbound_delivery_boundaries_test.go`, `outbound_stream_race_test.go`.

## Taking upstream updates

1. Fetch upstream and merge it into a new branch. Do not rewrite published
   branches.

   ```
   git remote add upstream https://github.com/multica-ai/multica   # once
   git fetch upstream
   git switch -c sync/<date> <current deploy branch>
   git merge upstream/main
   ```

2. Migrations: this fork's files 573–582 are applied on the production
   database, and 583 is once this build is deployed. Never rename, renumber, delete or edit them, and never reuse
   their names or numbers: `schema_migrations` records the full file name, so
   a renamed file runs again and a deleted one leaves an orphan row. Fix
   schema by adding a new migration after the last one. If upstream adds
   files with the same numbers, keep both: each is recorded by its full name,
   so upstream's apply as new versions. Verify the upgrade on a database that
   already holds this fork's migrations before deploying. Keep each concurrent
   index in its own file with its entry in `concurrentIndexCleanups`
   (`server/cmd/migrate/main.go`).
3. Regenerate queries with `make sqlc` (sqlc v1.31.1), then `go build ./...`.
4. Run the checks against a disposable database
   (`DATABASE_URL=... go run ./cmd/migrate up` first):

   ```
   cd server
   go test ./internal/handler/ -run 'TestTaskStatusNotification|TestWorkspaceDeletion|TestChatPromptHistory|TestGetChatHistory' -count=1
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
   - the chat branch of `buildClaimedTaskResponse` (`handler/daemon.go`) and
     the resume gates before `BuildPrompt` in `daemon/daemon.go`: history is
     rendered exactly when `PriorSessionID` is empty or
     `PriorSessionResumeUnavailable` is set;
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

## Local desktop refresh and MCP guard

- Includes MUL-27 (`36400b5e8`): reject workspace MCP replacements without a command or URL, preserving existing connections.
- Desktop toolbar adds Reload page immediately after the sidebar toggle. It reloads the renderer and clears its in-memory query cache; server-side agent runs continue. Unsaved form edits may be lost, as with a browser reload.
- Based on `5e744988`, retaining notification migrations 573–583 unchanged.

## v0.6.3 migration identity

The upstream uniqueness lint permits only the nine exact historical filename pairs
from the independent fork/upstream 573–582 allocations. All SQL files remain
unchanged; new collisions still fail. Never renumber either side after deployment.

## Telegram notification attachments (MUL-49)

The task-status notification outbox now performs the same object-storage to
Telegram attachment hop as member-triggered replies, after committing the
canonical text delivery. The completion transaction already binds files before
marking the notification pending; no new schema or upload protocol is needed.
Per-file Bot API outcomes are logged separately from the text outbox state.
File sends with uncertain outcomes are not retried, and a process crash during
this final hop can still leave files undelivered.

`scripts/local-tts/` contains the opt-in local Piper Irina +2 reply helper and
its activation/verification contract. The independent MUL-46 `sendVoice` patch is included in this deployment.

## Optional Telegram voice replies (MUL-46)

An agent may upload an OGG/Opus file named `*.voice.ogg` with
`multica attachment upload`. For `audio/ogg` or `application/ogg`, the Telegram
adapter sends it using `sendVoice`. Encode Opus at 48 kHz mono before uploading.
Ordinary OGG/MP3 attachments retain their existing audio-player behavior.
A definite Telegram 400 rejection falls back to a document; ambiguous errors
are not retried. This changes no agent settings and enables no automatic TTS.
Issue attachments do not route to a chat: the upload must run in the chat task
that owns the reply. Activation requires an explicitly approved backend deploy;
there are no migrations or restarts as part of the test.
