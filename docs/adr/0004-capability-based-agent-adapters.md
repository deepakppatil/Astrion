# ADR-0004: Capability-based agent adapter contract

- Status: Accepted
- Date: 2026-10-10

## Context

Astrion must support Claude Code and GitHub Copilot CLI now and other agent
CLIs later. These CLIs differ in flags, auth, output formats, and whether they
can pause for input. The SRS requires **no provider assumptions in the core
state machine** (AC-007).

## Decision

Define a neutral `Adapter` interface (`internal/agent/agent.go`). Each
adapter implements:

| Method | Purpose |
|---|---|
| `Capabilities()` | `interactionModes` (`CHECKPOINT_RESUME`, `LIVE_SESSION`), `structuredEvents`, `toolPermissionControl`, `nonInteractive`, `gracefulCancellation`, `supportedAuthMethods` |
| `ValidateProfile` / `ValidateAuth` | Reject agent profiles or credentials the CLI cannot use, before any job runs |
| `Invocation(req)` | Executable path, args, env and files for a command, inputs, mode, and optional resume answer. Secrets are referenced by file path, never placed in argv |
| `NewDecoder()` | Turns CLI output lines into neutral events (`log`, `progress`, `question`, `artifact`, `result`) |
| `EncodeLiveResponse` | Formats a human answer for a live session's stdin |
| `Disposition(exit, result, awaiting)` | Maps the exit to completed, awaiting input (checkpoint written), resume error, or failed |

The core (workflow, API, UI) consults **only capabilities**, never the
adapter's name. During the workflow's `VALIDATING` step,
`agent.ResolveMode` fixes the job's interaction mode:

- Use the profile's configured mode if the adapter supports it.
- With no configured mode, use `CHECKPOINT_RESUME` if supported, otherwise
  `LIVE_SESSION`.
- An unsupported mode is a validation error. The mode is never silently
  switched.

The **mock adapter** (`internal/agent/mock`, `cmd/mock-agent`) implements the
full contract. Tags in any string input simulate behaviours (`[mock:no-hitl]`,
`[mock:approval]`, `[mock:fail]`, `[mock:skip-artifact]`, `[mock:slow]`,
`[mock:secret-leak]`). CI and local development use it.

## Consequences

- A new CLI is one package plus an agent profile. No schema or workflow
  change.
- Behaviour differences surface as capability differences, which the UI can
  explain ("this agent cannot pause; answers must arrive within N minutes").
- Real adapters (Claude Code, Copilot CLI) are Release 2. Their output
  formats must be pinned by CLI version in the agent profile.

## Alternatives considered

- **Branching on `provider == "claude"` in the workflow.** Violates AC-007
  and spreads vendor logic everywhere. Rejected.
- **Wrapping every CLI in a common PTY and screen-scraping.** Fragile and
  untestable. Structured output modes are preferred and required where
  available.
