# Astrion

Astrion is a cloud-native platform for running **AI coding agents** across the
software delivery lifecycle: Plan, Design, Implement, Test, Deploy, Maintain.
Teams run framework commands such as `/write-spec` or `/implement-task`
against their repositories. Each run is:

- a durable, observable **job** with an enforced state machine;
- executed in an **isolated environment**;
- able to pause for **human input or approval** without losing work;
- cleaned up afterwards, every time.

The core assumes nothing about specific agents or infrastructure.
Agents (Claude Code, GitHub Copilot CLI, …) plug in through **adapters**.
Execution targets (Kubernetes on Rancher or EKS, a local process for
development) plug in through **providers**.

> **Status: Release 1 vertical slice.** The full job lifecycle works end to end:
>
> - acceptance → validation → queueing → provisioning → workspace →
>   agent → human-in-the-loop → artifacts → cleanup;
> - with a mock agent and the local and Kubernetes providers;
> - with a web console.
>
> Real agent adapters, pull-request creation, Jira, and the deployment
> artifacts (images, Helm chart, CI) are next. See the
> [implementation plan](docs/plan/implementation-plan.md) for exact status.

## Highlights

- **Durable jobs.** `POST /api/v1/jobs` returns only after the job is
  committed. One Temporal workflow per job is the single writer of its state.
  Idempotency keys make retries safe.
- **Human-in-the-loop that survives restarts.** The question and the waiting
  state are persisted *before* anyone is notified. Responses are validated,
  authorized, and audited *before* the job resumes. Jobs resume from a
  checkpoint, or continue a live session where the agent supports it. A
  destroyed process is never claimed to be restored.
- **Metadata-driven commands.** Commands, personas, inputs, approvals, and
  timeouts come from framework metadata, not code. Names keep their exact
  casing (`/Plan-migrate`, `/Create-Jira`).
- **Backend-enforced personas.** Roles cover business analyst/PM, architect,
  developer, QA, release engineer, and admin, with separation of duties. The
  UI only reflects what the server allows.
- **Secure by default.**
  - Hardened, ephemeral execution pods.
  - Per-execution tokens that are revoked at cleanup.
  - Credentials delivered as files, never as environment variables or URLs.
  - Secret redaction inside the environment.
  - Tenant isolation everywhere.
  - Production mode refuses every development shortcut.
- **Recoverable cleanup.** Cleanup is idempotent on every path. An orphan
  reconciler sweeps anything left behind.
- **Live updates.** A transactional outbox feeds Server-Sent Events, with
  `Last-Event-ID` resume.

## Commands in the default framework

| Phase | Commands |
|---|---|
| Plan | `/shape-spec`, `/write-spec`, `/create-task`, `/Plan-migrate`, `/Create-Jira`* |
| Design | `/build-context`, `/build-architecture` |
| Implement | `/implement-task`, `/orchestrate-task`, `/implement-jira` |
| Test | `/run-tests` |
| Deploy | `/prepare-deployment`, `/deploy`* |
| Maintain | `/diagnose`, `/update-dependencies` |

\* Requires an approval before execution.

Defined in
[`internal/registry/defaults/sdd-default.json`](internal/registry/defaults/sdd-default.json).
New commands need no backend changes.

## Architecture

```
Browser ──► astrion-api ──► PostgreSQL (state, outbox, audit)
              │  ▲              ▲
              │  │ SSE          │
              ▼  │              │
           Temporal ◄── astrion-worker ──► execution provider (Kubernetes / local)
                                                │
                                                ▼
                                   astrion-supervisor + agent CLI
                                   (reports back via the internal execution API)
```

| Component | Stack |
|---|---|
| Web console | React 19, TypeScript, Vite, Tailwind |
| Control-plane API | Go, chi, pgx |
| Workflows | Temporal (Go SDK) |
| State | PostgreSQL |
| Artifacts | S3-compatible object storage (filesystem in development) |
| Execution | Kubernetes Jobs (private Rancher, public EKS); local processes for development |

More detail:

- [Architecture overview](docs/architecture/overview.md);
- [domain model](docs/architecture/domain-model.md);
- [job state machine](docs/architecture/state-machine.md);
- [architecture decision records](docs/adr/README.md).

## Quick start (local)

Everything runs on one machine with the mock agent; no Docker or Kubernetes
is needed.

- **macOS:** [docs/getting-started/local-macos.md](docs/getting-started/local-macos.md)
- **Windows:** [docs/getting-started/local-windows.md](docs/getting-started/local-windows.md) (preview)
- **Linux:** below.

### Linux

You need:

- Go 1.26+, Node.js 22.12+, Git;
- PostgreSQL 15+ (your distribution's packages are fine);
- the [Temporal CLI](https://docs.temporal.io/cli/setup-cli).

The flow matches the macOS guide.

```bash
go build -o bin/ ./cmd/...
(cd web && npm ci && npm run build)
createdb astrion                                  # as a user allowed to create databases
temporal server start-dev --db-filename temporal.db   # terminal 1

# terminal 2 (API)
export ASTRION_DATABASE_URL="postgres://$USER@localhost:5432/astrion?sslmode=disable"
ASTRION_HTTP_ADDR=127.0.0.1:8080 ASTRION_WEB_DIR=web/dist ./bin/astrion-api

# terminal 3 (worker)
export ASTRION_DATABASE_URL="postgres://$USER@localhost:5432/astrion?sslmode=disable"
PATH="$PWD/bin:$PATH" ./bin/astrion-worker
```

Open <http://127.0.0.1:8080> and choose a development persona.

## Configuration

All settings are environment variables. The most important ones:

| Variable | Default | Purpose |
|---|---|---|
| `ASTRION_ENV` | `development` | `production` enables strict checks: OIDC only; no local provider, filesystem store, demo seeding, or `file://` repositories |
| `ASTRION_DATABASE_URL` | — (required) | PostgreSQL connection string |
| `ASTRION_HTTP_ADDR` | `:8080` | API listen address |
| `ASTRION_TEMPORAL_ADDRESS` / `_NAMESPACE` | `127.0.0.1:7233` / `default` | Temporal frontend |
| `ASTRION_AUTH_MODE` | `dev` (`oidc` in production) | `oidc` uses `ASTRION_OIDC_*` settings |
| `ASTRION_OBJECT_STORE` | `fs` (`s3` in production) | Artifact store; `s3` uses `ASTRION_S3_BUCKET`, `_ENDPOINT`, `_REGION`, `_SSE` |
| `ASTRION_POOLS_FILE` | — (a local pool in development) | Execution pool definitions (Kubernetes clusters, classification limits) |
| `ASTRION_EXEC_API_URL` | `http://127.0.0.1:8080` | Where execution environments reach the internal API |
| `ASTRION_DATA_DIR` | `.astrion` | Local artifact store and local-provider work directories |
| `ASTRION_WEB_DIR` | — | Serve the built console from this directory |
| `ASTRION_DEMO_REPO_URL` | — | Repository attached to the seeded demo workspace |
| `ASTRION_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

The complete list lives in [`internal/config/config.go`](internal/config/config.go).

## API

- Public REST API: [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1).
- Internal supervisor API: [`api/execution-api.yaml`](api/execution-api.yaml).

Both specs are checked against the server by tests (`go test ./internal/api`).
Every documented route must exist and vice versa. Schemas must match the Go
types and enums must match the domain constants.

## Development

```bash
# Go: unit tests run everywhere; database-backed tests run when a database is given
export ASTRION_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable"
go vet ./... && go test ./...
GOOS=windows go vet ./... && GOOS=darwin go vet ./...

# Web
cd web && npm ci && npx tsc -b && npm test && npm run build
```

Repository layout:

| Path | Contents |
|---|---|
| `cmd/` | `astrion-api`, `astrion-worker`, `astrion-supervisor`, `mock-agent` |
| `internal/` | Domain, API, workflow, store, registry, agent adapters, providers, supervisor, security |
| `web/` | Web console |
| `api/` | OpenAPI specifications |
| `docs/` | ADRs, architecture, [threat model](docs/security/threat-model.md), [observability](docs/operations/observability.md), [plan](docs/plan/implementation-plan.md), guides |

## Known limitations

- Only the **mock agent** adapter exists. Claude Code and Copilot CLI
  adapters are planned for Release 2.
- Repository changes stay on a working branch inside the environment.
  Pushing and opening PRs/MRs is planned for Release 2. Jira is not
  connected yet.
- No container images, Helm chart, or CI workflow yet; they are part of
  Release 1 hardening. The Kubernetes provider is unit-tested against a fake
  API server, not yet against a live cluster.
- The console signs in with development personas. The OIDC browser login
  flow is planned (the API already verifies OIDC tokens).
- Windows local development is a preview. See the
  [Windows guide](docs/getting-started/local-windows.md).
