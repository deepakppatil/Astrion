# ADR-0002: One durable Temporal workflow per job; the workflow is the only writer of job state

- Status: Accepted
- Date: 2026-10-10

## Context

A job may run for minutes, wait days for a human, lose its environment
mid-run, or be cancelled at any moment. Workers and API replicas restart.
We need orchestration that survives process loss, has durable timers, and
never runs two conflicting state changes for one job.

## Decision

1. **One Temporal workflow per job.** Workflow ID `job-<jobID>`, task queue
   `astrion-jobs`, ID reuse policy `REJECT_DUPLICATE`, so a job maps to
   exactly one run and starting is idempotent.
   - The API commits the job first, then tries to start the workflow
     (best effort, 5 s timeout).
   - A **start dispatcher** in the worker starts any `SUBMITTED` job whose
     start was not confirmed (`workflow_started = false`).
   - A crash between commit and start therefore delays the job. It never
     loses it.
2. **The workflow is the only writer of job state.** The API and the
   supervisor only record *facts* in PostgreSQL (status reports, interaction
   requests, responses, cancel requests). Then they send a **nudge** signal.
3. **Signals are hints, the database is the truth.** On every wake-up
   (signal or timer) the workflow re-observes the database through an
   activity and decides. A lost or duplicated signal therefore affects
   latency, never correctness. Operator decisions (`retry`, `reopen`,
   `cancel`) are delivered as an `operator-action` signal *and* audited by the
   API first.
4. **Small phases, not one giant function.** The workflow is composed of:
   validate → optional approval gate → attempts loop (admit → provision →
   monitor → suspend/resume) → finalize (begin cleanup → cleanup →
   complete). Every side effect is an idempotent activity with an explicit
   retry policy.
5. **Bounded loops.** At most 25 execution attempts per job. Waits use
   durable timers with policy-defined deadlines.

## Consequences

- Correctness does not depend on exactly-once messaging anywhere.
- All transitions go through `Store.TransitionJob`, which enforces the
  [state machine](../architecture/state-machine.md) and is idempotent.
- Workflow code must stay deterministic (no wall clock, I/O, or randomness
  outside activities). Code review enforces this today. Temporal replay tests
  against recorded histories are planned for CI (see the
  [implementation plan](../plan/implementation-plan.md)).
- Temporal is a hard dependency. Local development uses
  `temporal server start-dev`.

## Alternatives considered

- **Database-polling state machine (cron + row locks).** Feasible, but it
  re-implements durable timers, retries, and history. Rejected.
- **API writes state directly; workflow only executes.** Two writers race:
  cancel versus completion, answer versus timeout. Rejected.
- **One long-lived workflow per workspace or tenant.** Unbounded history
  and a shared failure domain. Rejected.
