Structured notification events (MUL-58)
======================================

Status: review candidate. Producer ingress always captures shadow events. The
bounded worker is registered only when `MULTICA_STRUCTURED_NOTIFICATIONS_ENABLED`
is exactly `true` and Telegram is configured. The flag defaults off; a separate
human-owned topic opt-in is also required. This change does not activate delivery.

`multica notification publish --file event.json` captures an idempotent event for
the authenticated originator/agent. `get`, `decide`, and `ack --message UUID`
inspect and exercise policy. Ingress cannot select a recipient or installation,
claim a trusted reminder, opt into a topic, or cause an external send.

An event has an account/window subject, reset period, action, observation time,
reset time and freshness deadline. Producers must emit an independent event for
an independently exhausted window, delayed reset or changed required action.
A known five-hour block does not acknowledge the weekly window. Event keys must
be stable across retries; different payloads at the same key are conflicts.
The optional `claude` snapshot contains both `five_hour` and `seven_day`, each
with `remaining_percent` (nullable) and `reset_at`. A missing snapshot renders both
windows as unknown; it does not invent current quota values.
Rendered text always attributes values to the snapshot and names both resets;
it never presents an expired observation as a current measurement.

Acknowledgement
---------------

The server reads the actual stored user/message row in the current member-owned
active chat. Caller-supplied text, role, author, session and relayed issue words
cannot supply consent. Supported full-message forms are:

- `ack <event UUID>`
- `в курсе <event UUID>`
- `я в курсе, лимиты Claude <account> на 5 часов исчерпаны до <UTC RFC3339 reset>`
- `я в курсе, лимиты Claude <account> на неделю исчерпаны до <UTC RFC3339 reset>`

The last two apply only to `blocked_until_reset` with the matching account,
window and exact reset. This is a deterministic narrow grammar, not arbitrary
natural-language understanding. Bare “я в курсе”, quotes, questions, relays and
assistant messages are refused. Source timestamps must follow observation and
precede reset; awareness expires at reset. Changed period/action/risk is not muted.
Generic capture refuses `explicit_reminder`; existing trusted schedules remain
on their existing path and are not cancelled by topic awareness.

Dormant outbox
--------------

`NotificationEventService.QueueDirect` is an internal preparation primitive,
not a publicly available activation endpoint. It requires a separately enabled
`notification_topic_route`, verifies the existing private Telegram route, checks
expected event revision and shadow ownership, and runs policy before creating
anything visible. Message, single owner transition, audit and outbox commit
atomically. An issue-results subscription does not authorize structured events. Replay of an
identical publish returns the existing identity, state and owner after transfer;
a different payload at that key still conflicts.

`DeliverDirect` durably reserves a pending row, then rechecks topic revision,
member/agent/private route, canonical message and awareness under the same
PostgreSQL scope lock used by ack writes. The lock remains held during a bounded
20-second transport interval. An ack committed before this authorization wins;
an ack arriving after authorization waits until the already-started attempt is
recorded. External sends cannot be retracted. Eight concurrent workers still
produce one attempt. The scope transaction consumes one database connection;
chunk recording consumes another. Pool sizing and bounded worker concurrency
must be reviewed before activation.

The Telegram adapter reuses existing chunking, HTML fallback and API error
classification. A proved 429 returns to pending at retry_after; acknowledged
chunks survive and are not resent. Proved rejection is failed. Timeout, 5xx,
missing message identity, recording failure or an abandoned reservation becomes
unknown and is not automatically retried. A delivered chunk is recorded durably
before the next chunk. Final outcome, canonical outbound metadata, event state
and audit commit together. A lost commit can leave sending until expiry; it does
not authorize a second send. `notification get` includes the outbox and audit.

Rollback returns only pending rows with zero delivered chunks to shadow and
removes their queued canonical message in the same transaction. Sent, unknown,
sending and partially delivered rows cannot be transferred to another route.
Existing PR #3 issue-result and attachment paths are unchanged.

Topic consent and bounded worker
-------------------------------

`notification topic get --agent-id UUID` returns the human's own route, revision
and audit (absent revision is 0). `notification topic set --agent-id UUID
--installation UUID --revision N --owner shadow|legacy|direct` performs a CAS
update; add `--enabled --legacy-stopped` only for an approved direct activation.
The installation must already be an accessible private Telegram route. Task and
cloud credentials cannot configure consent, even with a spoofed user header.
The authenticated human is recorded as actor; no user ID is accepted in input.
Configuration neither creates subscriptions nor adds permissions.

Activation requires an operator attestation that the legacy producer stopped and
a server check that existing task-status composing/pending/sending/unknown rows
are drained. The backend cannot prove that an unmodified external monitor has
stopped: stop and disable its old producer before attesting. Topic CAS prevents
multiple owners in this implementation; it cannot fence an unrelated old script.
A new direct interval records a server cutoff so historical shadow events are
not promoted. Installation changes require a fresh cutoff and attestation.

Each worker tick expires abandoned leases, scans at most 20 eligible shadow
events and at most 20 pending outbox rows, then works sequentially. Multiple
processes use durable reservations, event revisions and the shared ack scope
lock. Revoked and old-revision pending rows stay pending for explicit rollback;
they are not silently retried under a new topic revision. Unknown/partial rows
require reconciliation. The worker cannot create topic consent.

Prepared monitor adapter
------------------------

`scripts/notifications/claude_events.py --snapshot normalized.json --ledger
ledger.json` prepares events locally. `--publish` calls the authenticated Multica
CLI; it never sends Telegram or creates issues. The adapter is not installed in
the working monitor. Task credentials derive the agent; a human CLI invocation
must provide `--agent-id UUID`. The backend always scopes the recipient itself.
Its normalized input contains account, observed_at,
fresh_until and both five_hour/seven_day windows with remaining_percent/reset_at.
Use a separate durable ledger per producer identity. The ledger freezes the first
payload for each account/window/reset/action key, with an exclusive sidecar lock,
atomic replacement and fsync. Retries after lost CLI responses keep the same
payload even if a later observation changes benign numbers. New resets, windows
or required actions get separate keys. Unknown and unconfirmed reset conditions
remain separate risks. An expired frozen observation remains stale by policy;
it is not relabelled as a fresh measurement.

Release and rollback (separate approval required)
------------------------------------------------

1. Review this diff stacked on PR #3, migrate using the repository tooling and
   rehearse with a synthetic private route. Keep the server flag off and topics
   disabled. Inspect `notification get` decisions/audit and both template windows.
2. Obtain separate release approval. Stop/drain the old issue producer under one
   operator owner; account for pending and unknown old task-status deliveries.
   Keep explicit reminder schedules enabled. Do not run old and new producers
   concurrently and assume event identity alone prevents duplicate old issues.
3. After an approved release, enable the server flag and configure only the
   approved human topic using its current revision plus the legacy-stop
   attestation. Start the prepared CLI producer with its preserved ledger. Check
   the cutoff, canonical message, outbox identity and one canary delivery.
4. To roll back, disable the direct topic with CAS first (this waits behind a
   currently authorized bounded send). Inspect all outboxes. Use `notification
   rollback EVENT --agent-id UUID` only for pending rows with zero delivered
   chunks; this is human-only and removes the queued canonical message atomically.
   Sent, sending, unknown and partial outcomes cannot be replayed or transferred.
5. Reconcile unknown/partial attempts against Telegram before restoring the old
   route. A legacy owner update is refused while direct rows are unsettled.
   Disable the new worker flag in the approved rollback release, then restore the
   old producer only after proving no direct attempt is in flight. Never blindly
   replay a timeout. Preserve event, decision and topic audit history.

No real sends, topic subscriptions, production monitor changes or deployments
were performed to prepare this candidate. The scoped acknowledgement grammar
above remains deliberately narrow; arbitrary natural-language understanding and
Slack transport are not implemented.
