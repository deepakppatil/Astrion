# ADR-0010: Idempotency keys, idempotent cleanup, and an orphan reconciler

- Status: Accepted
- Date: 2026-10-10

## Context

Networks retry, users double-click, workers crash between "create" and
"record". Two failures must never happen:

- **Duplicate jobs:** two jobs for one request.
- **Orphaned environments:** running agents with live credentials that no
  job owns.

The SRS lifecycle requirements LIFE-001..008 demand that cleanup is always
attempted, idempotent, and backed by reconciliation.

## Decision

1. **Idempotency keys** on mutating user calls (`POST /jobs`,
   `POST /interactions/{id}/responses|approve|reject`, `POST /jobs/{id}/retry`):
   - Header `Idempotency-Key`: 8–128 characters of `[A-Za-z0-9._:-]`.
   - The key is stored with a SHA-256 hash of the request body, scoped to
     tenant and user.
   - The same key with the same body returns the original result. The same
     key with a different body returns `409 Conflict`.
2. **Idempotent internals:**
   - `CreateExecution` is unique per `(job, attempt)`.
   - `Allocate` is idempotent per execution ID.
   - `RecordInteraction` is unique per agent request ID.
   - `TransitionJob` re-applying the current state is a no-op.
   - `ResolveInteraction` is idempotent per key.
3. **Cleanup** (LIFE-001..003):
   - Always attempted, through `CLEANING_UP`.
   - Each step is idempotent and safe to repeat: terminate the environment,
     delete its secrets, revoke the execution token, release capacity.
   - Cleanup failure never changes the job outcome. It sets `cleanupStatus`,
     emits `cleanup.failed`, and leaves the rest to the reconciler.
4. **Orphan reconciler** (LIFE-004..008, `internal/reconciler`) runs every
   minute in the worker. It terminates environments that are:
   - labelled as Astrion-owned but with no execution row, after a 10-minute
     grace period for in-flight creates;
   - owned by an execution that is terminal, cleaned up, or expired.

## Consequences

- Clients can safely retry any mutating call.
- The worst case after a crash is a delay until the next reconciler pass,
  never a leak.
- Ownership labels must be present on every environment. Providers enforce
  this.

## Alternatives considered

- **Exactly-once delivery guarantees.** Not achievable end to end. Rejected
  in favour of idempotency everywhere.
- **Kubernetes TTL alone.** It does not revoke tokens or delete
  non-Kubernetes resources. Kept only as a backstop.
