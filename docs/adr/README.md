# Architecture Decision Records

An ADR records one significant decision: the context, what we decided,
and what it costs. ADRs are immutable once accepted. To change a decision,
add a new ADR that supersedes the old one and update the old one's status.

Format: [Michael Nygard's template](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
(Context, Decision, Consequences, Alternatives considered).

| # | Title | Status |
|---|---|---|
| [0001](0001-modular-control-plane.md) | Modular control plane with explicit component boundaries | Accepted |
| [0002](0002-temporal-workflow-per-job.md) | One durable Temporal workflow per job; the workflow is the only writer of job state | Accepted |
| [0003](0003-events-outbox-and-sse.md) | Job events through a PostgreSQL outbox, delivered over SSE; NATS JetStream deferred | Accepted |
| [0004](0004-capability-based-agent-adapters.md) | Capability-based agent adapter contract | Accepted |
| [0005](0005-execution-providers-and-pools.md) | Pluggable execution providers, policy-based pool selection, Kubernetes Jobs | Accepted |
| [0006](0006-durable-human-in-the-loop.md) | Durable human-in-the-loop: checkpoint/resume by default, live sessions when supported | Accepted |
| [0007](0007-metadata-driven-command-registry.md) | Metadata-driven command registry with case-preserved canonical names | Accepted |
| [0008](0008-in-environment-supervisor.md) | An in-environment supervisor talking to a narrow, per-execution internal API | Accepted |
| [0009](0009-identity-and-authorization.md) | OIDC identity and backend-enforced role-based authorization | Accepted |
| [0010](0010-idempotency-and-recoverable-cleanup.md) | Idempotency keys, idempotent cleanup, and an orphan reconciler | Accepted |
| [0011](0011-artifact-storage.md) | Artifacts in S3-compatible object storage, metadata in PostgreSQL | Accepted |
| [0012](0012-web-console.md) | Web console as a React SPA; tokens only in headers; SSE over `fetch` | Accepted |
