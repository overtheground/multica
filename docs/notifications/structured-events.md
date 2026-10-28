Structured notification events (MUL-58)
======================================

Status: implementation in progress. Producer ingress remains shadow-only and no
new worker is registered at boot. This is not an activation instruction.

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
with `remaining_percent` (nullable) and `reset_at`. Direct queuing requires it.
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
atomically. An issue-results subscription does not authorize structured events.

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

Work still required before activation review
-------------------------------------------

- Human-owned, authenticated topic configuration/consent API and CLI, with CAS
  updates; no subscriptions have been created by this implementation run.
- Bounded worker registration behind an explicit disabled-by-default flag,
  eligibility cutoff to avoid historical backlog, and pending-event scans.
- A prepared monitor adapter with stable identities for independent windows;
  no installation into the working monitor. Coordinate old/new producer owner
  transfer so the old issue path cannot send a second copy. Current event-level
  CAS rejects legacy ownership but does not stop an independently running old
  monitor from creating an unrelated issue.
- Idempotent publish retry after route transfer: current shadow-only capture
  intentionally reports a conflict if the identity is now direct-owned.
- Full final suite including race checks, migration/cutover rehearsal, and PR.

Activation must be a separately approved release: review consent and a fresh
shadow journal, stop/drain the old producer under one owner, enable only the
approved topic after a cutoff, then canary with known IDs. Roll back new claiming
first; preserve unknown/partial rows for reconciliation, return only unsent rows
to shadow, and restore the old producer only after proving no in-flight direct
attempt. Do not blindly replay a Telegram timeout. Explicit reminder schedules
remain enabled throughout.
