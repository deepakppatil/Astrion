# ADR-0005: Pluggable execution providers, policy-based pool selection, Kubernetes Jobs

- Status: Accepted
- Date: 2026-10-10

## Context

Agents must run in isolated, ephemeral environments: a private Rancher
cluster for sensitive work and AWS EKS for public workloads, with other
providers possible later. Data classification and network reachability
constrain where a job may run.

## Decision

1. **Provider interface** (`internal/provider`): `Validate`, `CheckCapacity`,
   `Allocate`, `Status`, `Terminate`, `ListOwned`. Every environment carries
   **ownership labels** (tenant, job, execution, attempt) so it can always be
   traced and reconciled (LIFE-008).
2. **Pools** wrap providers with policy attributes: `network`
   (`PRIVATE`/`PUBLIC`), `maxClassification`, region, tools, cost weight, and
   enabled state. Pools come from configuration (`ASTRION_POOLS_FILE`).
3. **Selection fails safe** (`provider.Select`). A pool is a candidate only if
   it satisfies every constraint: tenant and workspace allow-lists, data
   classification ≤ pool maximum, private network if the repository requires
   it, region, and tools. Among candidates prefer **private over public**,
   then lower cost. **There is no fallback** to a less restrictive pool when
   compliant pools are busy. The job queues instead.
4. **Kubernetes provider.** Each attempt is one `batch/v1` Job:
   - **Hardened pod spec:** `runAsNonRoot`, `readOnlyRootFilesystem`,
     `allowPrivilegeEscalation: false`, drop `ALL` capabilities, seccomp
     `RuntimeDefault`, `automountServiceAccountToken: false`, resource limits.
   - **Job limits:** `backoffLimit: 0` (retries are a workflow decision),
     `activeDeadlineSeconds`.
   - **Credential handoff:** the Job is created **suspended**. A Secret with
     an `ownerReference` to the Job is created next, and only then is the Job
     unsuspended. Deleting the Job therefore garbage-collects its secrets.
   - **TTL:** `ttlSecondsAfterFinished` is only a backstop. Astrion deletes
     Jobs explicitly (LIFE-005).
5. **Local provider** (`internal/provider/local`) runs the supervisor as a
   local process group in a private directory. It offers **no isolation**,
   exists for development and CI, and is **refused in production mode**.

## Consequences

- Rancher and EKS are configuration (two Kubernetes pools), not code.
- Classification policy is enforced in one tested function.
- Busy private pools increase queue time rather than leaking data. This is
  a deliberate trade-off that is visible to users as `QUEUED`.

## Alternatives considered

- **Long-lived agent pods with job multiplexing.** Cheaper, but there is
  cross-job residue risk. Rejected.
- **Pods instead of Jobs.** Jobs give deadlines, suspend/unsuspend, and
  owner-based GC. Chosen.
- **Automatic public fallback when private is full.** Violates data policy.
  Rejected outright.
