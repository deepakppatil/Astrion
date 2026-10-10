# ADR-0007: Metadata-driven command registry with case-preserved canonical names

- Status: Accepted
- Date: 2026-10-10

## Context

SDLC frameworks expose slash commands, for example:

- Plan: `/shape-spec`, `/write-spec`, `/create-task`, `/Plan-migrate`, `/Create-Jira`
- Design: `/build-context`, `/build-architecture`
- Implement: `/implement-task`, `/orchestrate-task`, `/implement-jira`

New commands and frameworks must not require backend code changes (AC-008).
Names are case-sensitive as published (`/Plan-migrate` ≠ `/plan-migrate`).

## Decision

1. Each command is **metadata** (`internal/registry/command.go`):
   - Identity: canonical name (exact casing), version, phase.
   - Who and what: personas, required permissions, compatible agents.
   - Inputs: typed input fields.
   - Effects: repository access (`NONE`/`READ`/`WRITE`), expected artifacts,
     side effects (for example `EXTERNAL_TICKET`).
   - Policies: timeout, approval policy, retry policy, HITL policy, output
     validation rules.
2. **Exact-casing lookup.** Submissions must use the canonical name exactly.
   Lookups never case-fold. A near-miss (`/plan-migrate`) returns a `422`
   that suggests the canonical spelling (`/Plan-migrate`). The suggestion only
   names commands the caller may already see, so it cannot reveal hidden
   commands.
3. **Validation at submit and again in the workflow:**
   - Inputs are checked strictly against the declared fields (unknown fields
     rejected, enums, lengths).
   - The command version is pinned on the job.
   - The framework revision is recorded for reproducibility.
4. **Visibility** (`registry.Visible`): users see only commands for their
   permissions and the selected agent. Unapproved commands are hidden unless
   explicitly requested by an administrator.
5. **Discovery:** commands come from the framework's manifest
   (`internal/registry/defaults/sdd-default.json` for the default SDD
   framework). Discovered commands start unapproved.

## Consequences

- Adding `/build-architecture`-style commands is a manifest change.
- Policy (approvals, side effects) is reviewable data. Side effects such as
  Jira writes are separately grantable (`integration:jira:write`).
- Old jobs stay interpretable because the command version and framework
  revision are stored on each job.

## Alternatives considered

- **Hard-coded command handlers per phase.** Violates AC-008. Rejected.
- **Case-insensitive matching.** Risks executing a different command than
  the framework defines. Rejected.
