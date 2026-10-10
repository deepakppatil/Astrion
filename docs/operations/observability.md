# Observability

What Astrion emits today (Release 1 vertical slice), and what is planned.

## Health endpoints (API)

| Endpoint | Meaning | Use for |
|---|---|---|
| `GET /healthz` | Process is up | Liveness probe |
| `GET /readyz` | Database reachable within 2 s (`503` otherwise) | Readiness probe |
| `GET /metrics` | Prometheus text format | Scraping. Restrict to the cluster network (see the [threat model](../security/threat-model.md)) |

## Metrics

Exposed by `astrion-api` (`internal/api/metrics.go`, a small dependency-free
exporter):

| Metric | Type | Labels |
|---|---|---|
| `astrion_http_requests_total` | counter | `method`, `route` (pattern, e.g. `/api/v1/jobs/{id}`), `code` |
| `astrion_http_request_duration_seconds_sum` / `_count` | summary | `method`, `route` |
| `astrion_jobs_submitted_total` | counter | — |
| `astrion_interactions_resolved_total` | counter | — |
| `astrion_artifacts_uploaded_total` | counter | — |
| `astrion_sse_streams_active` | gauge | — |

Route labels are patterns, never raw paths, so cardinality stays bounded and
no IDs or tenant data leak.

Useful queries:

```promql
# Error ratio by route
sum by (route) (rate(astrion_http_requests_total{code=~"5.."}[5m]))
  / sum by (route) (rate(astrion_http_requests_total[5m]))

# Mean latency by route
sum by (route) (rate(astrion_http_request_duration_seconds_sum[5m]))
  / sum by (route) (rate(astrion_http_request_duration_seconds_count[5m]))
```

Business-level signals are also available from the API itself:

- `GET /api/v1/dashboard` returns active jobs, jobs awaiting input,
  succeeded/failed in 24 h, average duration, counts by state, and cleanup
  failures.
- The Temporal Web UI (`http://localhost:8233` locally) shows every job
  workflow (`job-<jobID>`) with its full history.

## Logs

All services log **structured JSON** to stderr (`log/slog`). The level comes
from `ASTRION_LOG_LEVEL` (`debug`, `info`, `warn`, `error`). Every record
carries `service` (`astrion-api`, `astrion-worker`, `astrion-supervisor`).
Workflow and activity logs include Temporal's workflow and run IDs.

Never logged: tokens, secret values, request bodies. Agent output reaches
logs only after in-environment redaction.

Job logs (agent output) are a separate, user-facing stream:

- stored per execution;
- readable via `GET /api/v1/jobs/{id}/logs` (JSON pages, or
  `?format=text`);
- streamed live as `job.log_batch` SSE events.

## Alerts worth having from day one

| Alert | Signal |
|---|---|
| Cleanup failures | `dashboard.cleanupFailures > 0`, or `cleanup.failed` events |
| Jobs stuck waiting for capacity | Jobs in `QUEUED` older than the queue timeout |
| Jobs needing intervention | Count of `NEEDS_INTERVENTION` > 0 |
| API 5xx ratio | Query above > 1% for 10 min |
| Readiness flapping | `/readyz` non-200 |

## Planned (see the [implementation plan](../plan/implementation-plan.md))

- OpenTelemetry tracing across API → Temporal → activities → supervisor
  (trace context propagated in the execution spec).
- Switch the exporter to `client_golang`/OTel metrics (names stay the same),
  add per-pool provisioning latency and queue-time histograms.
- Grafana dashboards and alert rules shipped with the Helm chart.
