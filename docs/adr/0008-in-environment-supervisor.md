# ADR-0008: An in-environment supervisor talking to a narrow, per-execution internal API

- Status: Accepted
- Date: 2026-10-10

## Context

Agent CLIs run arbitrary tools on untrusted repository content. Something
inside the environment must:

- check out repositories and drive the CLI;
- stream logs, upload outputs, and relay questions.

That component must not be able to reach other tenants' data or the database.

## Decision

1. **`astrion-supervisor`** runs as the entrypoint of every execution
   environment. It:
   - fetches its spec;
   - checks out repositories (credentials via `GIT_ASKPASS` from secret files,
     never in URLs or `.git/config`);
   - runs the agent via its adapter in a separate process group;
   - batches and **redacts** logs before they leave the environment;
   - uploads artifacts with size limits and SHA-256;
   - reports status and completion.
2. **Internal execution API** (`/internal/v1/executions/{id}/…`, see
   [`api/execution-api.yaml`](../../api/execution-api.yaml)), authenticated by a
   **per-execution bearer token**:
   - The token is random, stored hashed, and bound to one execution, job,
     and tenant.
   - It expires and is **revoked at cleanup** (AUTH-006).
   - It cannot call the public API.
3. The supervisor **records facts only**. The workflow decides state
   ([ADR-0002](0002-temporal-workflow-per-job.md)). Calls after cleanup get
   `410 Gone`, and the supervisor stops the agent.
4. The agent process gets a **minimal environment**: PATH plus a private
   HOME and TMP (and the OS variables needed to function). The execution token
   is never passed to the agent.

## Consequences

- A compromised agent can at most act on its own execution with its own
  token, until cleanup.
- Redaction happens at the edge, so secrets never reach the database or
  object storage (verified by the `[mock:secret-leak]` end-to-end test).
- The supervisor must be shipped in every runtime image (Release 2:
  signed images).

## Alternatives considered

- **Sidecar with database access.** Too much privilege. Rejected.
- **Streaming raw logs to the API and redacting there.** Secrets would cross
  the network and sit in buffers. Rejected.
