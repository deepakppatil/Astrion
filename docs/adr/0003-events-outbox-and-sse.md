# ADR-0003: Job events through a PostgreSQL outbox, delivered over SSE; NATS JetStream deferred

- Status: Accepted
- Date: 2026-10-10

## Context

Users watch jobs live (state changes, log lines, questions, artifacts).
The SRS names NATS JetStream for events and SSE for live updates. The
hard requirements are:

- An event must never be lost, and never be emitted for a change that was
  rolled back.
- A client that reconnects must be able to resume where it left off.

## Decision

1. **Transactional outbox.** Every state change writes its event row to
   `job_events` (monotonic `id`) **in the same transaction** as the change.
   There are no dual writes.
2. **`pg_notify` is only a wake-up hint.** After commit, a notification wakes
   live subscribers (`internal/events`). Subscribers always re-read
   `job_events WHERE id > lastSeen`. A missed notification costs latency
   (a 2-second fallback poll), never data.
3. **SSE for the browser.** `GET /api/v1/jobs/{id}/events` streams
   `id:`/`event:`/`data:` frames. Clients resume with `Last-Event-ID`. The
   stream ends with an `end` event once the job is terminal and drained.
4. **NATS JetStream is deferred.** It is introduced when there are
   cross-service consumers that need fan-out beyond one database: the
   integrations service, notification service, and analytics. It will be
   fed by an outbox relay, so the outbox stays the source of truth.

Event types: `job.status_changed`, `job.step_changed`, `job.log_batch`,
`interaction.input_required`, `interaction.approval_required`,
`interaction.resolved`, `artifact.available`, `job.failed`, `cleanup.failed`,
`job.completed`, and `end` (stream only).

## Consequences

- One less piece of infrastructure in Release 1. Exactly-once *visible*
  semantics come from the outbox ID cursor.
- PostgreSQL carries event write volume. Log lines are batched (≤200 lines
  or 60 KiB per batch) to keep it modest. Retention pruning of old events
  (per tenant `retentionDays`) is planned for Release 3.
- Adding JetStream later does not change the API or UI contract.

## Alternatives considered

- **Publish to NATS directly from handlers.** A dual write: an event could be
  published for a rolled-back transaction, or lost after commit. Rejected.
- **WebSockets.** Bidirectional channels are not needed. SSE works through
  standard proxies and has built-in resume semantics. Rejected for now.
