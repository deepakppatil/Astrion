# ADR-0001: Modular control plane with explicit component boundaries

- Status: Accepted
- Date: 2026-10-10

## Context

Astrion runs AI coding agents (Claude Code, Copilot CLI, and future CLIs)
against customer repositories, across six SDLC phases, on several kinds of
infrastructure. Its requirements pull in different directions:

- API latency and availability (users, the UI, integrations).
- Long-running, durable orchestration (jobs that wait hours for a human).
- Untrusted, resource-heavy execution (agent CLIs running arbitrary tools).

The SRS forbids a monolith and "one enormous workflow", and asks for
correctness, recoverability, security, and simplicity.

## Decision

Split the system into processes with **narrow, explicit contracts**. Each
process can be deployed, scaled, and secured independently.

| Component | Binary / package | Owns | Must not |
|---|---|---|---|
| **Control-plane API** | `cmd/astrion-api`, `internal/api` | Public REST API, authN/Z, validation, durable job acceptance, SSE, internal execution API | Run agents; change job state after acceptance |
| **Workflow worker** | `cmd/astrion-worker`, `internal/workflow` | Job orchestration (one workflow per job), all job state transitions, provisioning via providers, cleanup | Serve users; hold user credentials |
| **Supervisor** | `cmd/astrion-supervisor`, `internal/supervisor` | Inside one execution environment: checkout, run the agent CLI via its adapter, stream redacted logs, upload artifacts, raise questions | Talk to the database; see other jobs |
| **Agent adapters** | `internal/agent/*` | Translating one CLI's flags and output into the neutral event contract | Leak CLI specifics into the core |
| **Execution providers** | `internal/provider/*` | Allocate, inspect, terminate, list environments in one infrastructure type | Make policy decisions |
| **Orphan reconciler** | `internal/reconciler` (runs in the worker, every minute) | Finding and terminating environments no live execution owns | Change job outcomes |
| **Start dispatcher** | `internal/orchestrator` (runs in the worker, every 5 s) | Starting workflows for accepted jobs whose start the API could not confirm | Create jobs |
| **Registry** | `internal/registry` | Framework/command metadata and visibility rules | Execute anything |
| **Domain** | `internal/domain` | Entities, state machine, roles/permissions, errors | Depend on any infrastructure |

Dependency direction: `domain` ← everything; nothing imports `api`. `api` and
`workflow` depend on `store`, `registry`, `authz`. **Only the workflow
activities and the reconciler allocate or terminate environments.** The API
uses the pool registry read-only: to check that an explicitly requested pool
exists and to list pools. Policy-based pool selection runs in the workflow's
validation step. The supervisor is the only component that *runs* agents through adapters.
The API uses the adapter registry only to validate agent profiles and show
capabilities. The workflow uses it to resolve the interaction mode.

State lives in **PostgreSQL** (application state, outbox, audit), **Temporal**
(execution history), and **object storage** (artifacts).

## Consequences

- Each boundary is a security boundary. The supervisor runs in an
  untrusted environment with a token scoped to one execution
  ([ADR-0008](0008-in-environment-supervisor.md)). The API never executes
  agent code.
- More moving parts locally (API + worker + Temporal + Postgres). The
  local-development guides and the `local` provider keep this manageable.
- Adding an agent CLI or an infrastructure type is a new adapter or provider
  package. The core does not change (AC-007, AC-008).

## Alternatives considered

- **Single binary with goroutines for everything.** Simpler to start, but it
  mixes untrusted execution with the API's credentials and couples scaling.
  Rejected.
- **Microservice per SDLC phase.** The phases differ only in metadata
  (commands, personas, policies), not in behaviour, so separate services
  would duplicate the lifecycle. Rejected in favour of the metadata-driven
  registry ([ADR-0007](0007-metadata-driven-command-registry.md)).
