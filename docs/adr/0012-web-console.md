# ADR-0012: Web console as a React SPA; tokens only in headers; SSE over `fetch`

- Status: Accepted
- Date: 2026-10-10

## Context

The SRS specifies React + TypeScript + Vite + Tailwind for the frontend and
SSE for live updates. The browser's `EventSource` API cannot send an
`Authorization` header. The usual workaround puts the token in the query
string, which leaks it into proxy logs, browser history, and `Referer` headers.

## Decision

1. **SPA** in `web/` (React 19, TypeScript, Vite, Tailwind v4, React Router).
   The browser only calls **relative URLs** (`/api/v1/...`):
   - in development, the Vite dev server proxies them to the API;
   - in single-port mode, the API serves the built SPA (`ASTRION_WEB_DIR`).
2. **Tokens only in the `Authorization` header.** The server ignores tokens in
   URLs everywhere, SSE included. The token is held in `sessionStorage` (per
   tab, cleared on close). In production the token comes from the OIDC login
   flow (Release 2). The dev-persona login page exists only in development.
3. **SSE over `fetch` + `ReadableStream`.** This lets the client:
   - send the `Authorization` and `Last-Event-ID` headers;
   - reconnect with exponential backoff (1 s → 15 s);
   - stop on `401`/`403`/`404` or on the `end` event.
4. **Mutations carry an `Idempotency-Key`** generated per user action
   (`crypto.randomUUID`), so double-clicks and retries are safe.
5. **The UI mirrors, never decides, authorization.** Controls are hidden when
   the server's `actions` map or `/me` permissions say no. The server
   remains authoritative.
6. **Untrusted content** (agent output, artifacts) is rendered as text. Raw
   HTML is never injected. Artifact previews come from the API's sandboxed,
   text-only responses.

## Consequences

- No credentials appear in URLs anywhere in the system.
- A small hand-written, spec-compliant SSE parser to maintain
  (`parseSSE`, covered by `web/src/__tests__/sse.test.ts`: CRLF, chunk
  splits, comments, resume, and that the token never appears in a URL).
- `sessionStorage` is still readable by injected script. The mitigations are
  text-only rendering and a strict Content-Security-Policy on the SPA shell
  (`script-src 'self'`, no `unsafe-inline`, `connect-src 'self'`,
  `object-src 'none'`). The OIDC flow (Release 2) can
  move to HTTP-only cookies with CSRF protection if required.

## Alternatives considered

- **`EventSource` with `?access_token=`.** Leaks tokens. Rejected, and the
  server-side fallback was removed.
- **WebSockets.** Unnecessary bidirectionality. See ADR-0003.
