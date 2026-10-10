# ADR-0006: Durable human-in-the-loop — checkpoint/resume by default, live sessions when supported

- Status: Accepted
- Date: 2026-10-10

## Context

Agents ask clarifying questions and request approvals. Humans answer in
minutes or days. Keeping a container alive for days is costly, and the
container may be lost anyway. The SRS requires that the question and the
waiting state are persisted before notifying anyone, and that responses are
validated and audited before resuming. It also forbids claiming to restore
a process that was destroyed.

## Decision

1. **Two modes, chosen from adapter capabilities** during validation and then
   pinned for the job:
   - `CHECKPOINT_RESUME` (default): the agent writes a checkpoint (uploaded
     as an internal artifact) and exits with "awaiting input". The workflow
     **releases the environment and capacity**, waits durably, then
     provisions a **new attempt** that resumes from the checkpoint with the
     answer.
   - `LIVE_SESSION`: the process stays alive and the answer is written to its
     stdin. Strict timeouts apply. If the environment is lost while waiting,
     the job goes to `NEEDS_INTERVENTION`. It is never "restored".
2. **Ordering.** The supervisor records the question (`RECORDED`, idempotent
   per agent request ID). The workflow moves the job to `WAITING_FOR_INPUT`
   or `WAITING_FOR_APPROVAL` and *then* opens the interaction (`OPEN`) and
   emits the event that notifies users.
3. **Responses** are validated against kind, options, and schema, then
   authorized and stored with an `Idempotency-Key`. They are audited, and only
   then is the workflow nudged. Responses are data and are never interpreted
   as commands.
4. **Who may answer:** the requested user, a holder of the requested role,
   the initiator (when no target is set), or `interaction:respond:any`.
   Production approvals also require `approve:production`.
5. **Timeouts** follow the command's `hitl.onTimeout` policy: `FAIL`,
   `NEEDS_INTERVENTION` (operator may `reopen`), or `PAUSE`.
6. **Platform approval gates** (`approval.beforeExecution`) reuse the same
   interaction machinery before any environment is provisioned.

## Consequences

- Waiting costs nothing in compute in the default mode.
- Resume correctness depends on adapters producing faithful checkpoints.
  The mock agent tests this path end to end.
- The UI must say plainly which mode a job uses. For example, "the session
  will resume from a checkpoint" versus "the agent is waiting live".

## Alternatives considered

- **Always keep the session alive.** Expensive, and fragile across node
  drains. It remains available when an adapter cannot checkpoint.
- **Notify first, persist later.** A crash in between loses the question.
  Rejected.
