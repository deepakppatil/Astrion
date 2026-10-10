# Phased implementation plan

The SRS §28 directive is followed in order:

1. Design artifacts first: ADRs, component boundaries, domain model, state
   machine, OpenAPI, threat model, this plan.
2. Then a **minimal vertical slice** that exercises the whole lifecycle with a
   mock agent and a local provider.
3. Only then real integrations.

The guiding priorities are correctness, recoverability, security, and
simplicity.

> The release contents below map the SRS roadmap (§26, Releases 1–4) onto
> what is built. Where the SRS assigns an item to a different release,
> the SRS wins. Update this file in the same pull request that changes scope.

## Status at a glance (2026-10-10)

| Phase | Scope | Status |
|---|---|---|
| 0. Foundations | ADRs, architecture, domain model, state machine, OpenAPI, threat model, plan | ✅ Done (`docs/`, `api/`) |
| 1. Vertical slice | End-to-end lifecycle with mock agent + local provider + web console | ✅ Done |
| 1H. Release 1 hardening | Containers, Helm, CI, NetworkPolicies, OIDC login in the console | ⏭️ Next |
| 2. Real agents and repository integrations | Claude Code, Copilot CLI, GitHub/GitLab change requests, Jira | 📋 Planned |
| 3. Enterprise controls | Tenant policies, retention, sandboxing, audit integrity, presigned downloads | 📋 Planned |
| 4. Scale and breadth | Test/Deploy/Maintain depth, more providers, analytics, NATS fan-out | 📋 Planned |

## Phase 1 — vertical slice (done)

What exists and how it is verified:

| Capability | Where | Evidence |
|---|---|---|
| State machine with enforced, idempotent transitions | `internal/domain/jobstate.go` | Unit tests + doc/code drift test |
| Durable job acceptance with idempotency keys | `POST /api/v1/jobs` | API tests (replay, conflicting body) |
| One Temporal workflow per job; workflow is sole state writer; start dispatcher | `internal/workflow`, `internal/orchestrator` | Workflow tests + live e2e |
| Metadata-driven registry, exact-casing commands (`/Plan-migrate`, `/Create-Jira`, …) | `internal/registry` | Registry + API tests (visibility, casing hints) |
| Personas and backend-enforced RBAC; separation of duties | `internal/authz` | Authz + API tests |
| Tenant isolation (404 across tenants) | store + API | `TestTenantIsolation` |
| Policy-based pool selection, no private→public fallback | `internal/provider` | Selection tests |
| Kubernetes provider (hardened Jobs, suspended create + owned Secret) | `internal/provider/kubernetes` | Manifest and lifecycle tests (fake API server) |
| Local provider (dev only), cross-platform process-tree control | `internal/provider/local`, `internal/procgroup` | Tests; Windows/macOS cross-compile |
| Supervisor: checkout, adapter run, redacted logs, artifacts, HITL | `internal/supervisor` | Supervisor tests + e2e |
| Durable HITL: checkpoint/resume, approvals, timeouts, operator actions | workflow + API | e2e (answer, approve/reject, cancel, fail) |
| Idempotent cleanup + orphan reconciler | workflow, `internal/reconciler` | Reconciler tests; cancel e2e (no surviving processes) |
| Outbox events + SSE with `Last-Event-ID` resume | `internal/events`, `internal/api/stream.go` | API SSE replay test; web SSE tests |
| Secret redaction at the edge | `internal/redact`, supervisor | Unit tests + `[mock:secret-leak]` e2e |
| Web console (dashboard, run command, live job view, inbox, workspaces, registry, audit) | `web/` | Vitest; headless-browser walkthrough |

## Phase 1H — Release 1 hardening (next)

Ordered by risk reduction:

1. **CI pipeline** (GitHub Actions):
   - `go vet`, `go test` with a PostgreSQL service;
   - `GOOS=windows`/`darwin` cross-compile;
   - web `tsc` + `vitest` + `vite build`;
   - OpenAPI lint and the route/spec drift test;
   - `govulncheck` and `npm audit`.
2. **Containers**: distroless images for API/worker; a runtime image with
   supervisor + agent CLIs (non-root, read-only FS compatible).
3. **Helm chart** (`deploy/helm/astrion`):
   - API and worker Deployments, probes, PodDisruptionBudgets;
   - provider ServiceAccount/Role (namespace-scoped);
   - **default-deny NetworkPolicies** with egress allow-lists by network
     profile;
   - internal execution API on a separate listener and port;
   - `/metrics` not exposed via ingress.
4. **OIDC login in the console**: authorization-code + PKCE flow. Replaces the
   dev-persona page outside development.
5. **Temporal replay tests** in CI against recorded workflow histories, to
   catch non-deterministic workflow changes.
6. **Operations**: Grafana dashboards and alert rules from
   [observability](../operations/observability.md); runbooks for
   `NEEDS_INTERVENTION`, cleanup failures, and stuck queues.

Exit criteria:

- A clean install on a Rancher cluster and an EKS cluster runs the mock
  agent end to end.
- The threat-model items tagged "R1 hardening" are closed.

## Phase 2 — real agents and repository integrations

- **Claude Code adapter** and **GitHub Copilot CLI adapter**:
  - structured output decoding;
  - checkpoint/resume where the CLI supports it, live session otherwise;
  - output-contract tests pinned per CLI version.
- **Credentials**: Vault / cloud secret-manager integration for
  `credentialRef`; short-lived GitHub App and GitLab tokens minted per
  execution.
- **Repository side effects**, permission-gated and audited:
  - push the working branch;
  - open a PR or MR with mandatory human review;
  - record `finalCommit` and `changeRequestUrl`.
- **Jira** connector for `/Create-Jira` and `/implement-jira`
  (approval-gated, `integration:jira:write`).
- **Supply chain**: signed runtime images (cosign) verified at admission.
- **Console**: notifications (email or chat) for open interactions; per-user
  SSE stream caps.

## Phase 3 — enterprise controls

- Tenant-customizable role and permission policies, and approval matrices.
- Retention pruning (events, logs, artifacts) by tenant `retentionDays`.
- Per-tenant buckets and KMS keys; presigned artifact downloads.
- DB-enforced append-only audit with a hash chain; audit export.
- Sandboxed runtimes (gVisor/Kata) per pool; private package mirrors.
- Full OpenTelemetry tracing across API → workflow → supervisor.

## Phase 4 — scale and breadth

- Deeper Test, Deploy, and Maintain command packs (deploy approvals with
  `approve:production` are already modelled).
- Additional execution providers (for example serverless containers) behind
  the same interface.
- NATS JetStream fan-out fed by the outbox, for integrations and analytics
  consumers ([ADR-0003](../adr/0003-events-outbox-and-sse.md)).
- Usage analytics and cost reporting per tenant and pool.

## Known gaps carried forward

- Acceptance criteria AC-001..AC-015 (SRS §25) should be traced
  one-by-one to tests in this file. Today AC-007 (no provider assumptions in
  the core) and AC-008 (new commands without backend changes) are referenced
  in code and covered by the adapter and registry design.
- Local development is exercised end to end on Linux. macOS and Windows
  builds are cross-compiled and vetted (`GOOS=darwin|windows go vet ./...`),
  and the only platform-specific code (process-tree control) is isolated in
  `internal/procgroup`. Neither has yet run on real macOS or Windows
  hardware. Reports from those platforms are welcome, and CI matrix runners
  are part of Phase 1H.
