# ADR-0009: OIDC identity and backend-enforced role-based authorization

- Status: Accepted
- Date: 2026-10-10

## Context

Personas (BA/PM, Architect, Developer, QA, Release/Platform Engineer,
Admin) have different rights per SDLC phase, plus separately grantable
external side effects (repository push, change requests, Jira writes,
production deploys and approvals). Tenants must be isolated. The UI must
not be the enforcement point.

## Decision

1. **Authentication** (`internal/auth`):
   - **Production:** OIDC bearer tokens verified against the issuer's JWKS
     (issuer, audience, expiry with leeway). The tenant comes from a claim or
     the configured default. Roles come from IdP groups via
     `ASTRION_OIDC_GROUP_ROLES` and are synchronized on each request.
   - **Development only:** static dev tokens (`ASTRION_AUTH_MODE=dev`). They
     are **refused when `ASTRION_ENV=production`** (AUTH-001).
2. **Authorization** (`internal/authz`): roles map to permissions in one
   reviewed table. Every handler checks permissions server-side.
   `GET /jobs/{id}` returns an `actions` map so the UI can *hide* controls,
   but the server re-checks on every call.
3. **Separation of duties:**
   - `platform_admin` administers (agents, registry, pools, audit, cancel any
     job, answer any interaction) but has **no** phase-execution, deploy, or
     Jira-write permissions.
   - Delivery roles cannot administer.
4. **Tenant isolation:** every query is tenant-scoped. Cross-tenant IDs
   return **404, not 403**, so existence is not revealed.
5. **Audit:** security-relevant actions are written to an append-only
   `audit_events` table with actor, action, target, and non-secret details.

| Role | Highlights |
|---|---|
| `business_analyst` | Execute Plan; Jira write |
| `architect` | Execute Plan and Design; manage workspaces |
| `developer` | Execute Design, Implement, Test, Maintain; push; change requests |
| `qa_engineer` | Execute Test |
| `release_engineer` | Execute Test, Deploy, Maintain; deploy (non-prod and prod); approve production; select pools |
| `platform_admin` | Administration only (see above); audit |
| `viewer` | Read-only |

## Consequences

- The UI is a convenience. API clients get identical enforcement.
- Static role mapping in code is simple and reviewable. Tenant-custom
  policies are planned (Release 3).

## Alternatives considered

- **Authorization in the UI or API gateway only.** Bypassable. Rejected.
- **Admin as superuser.** Concentrates risk and blurs accountability.
  Rejected.
