# ADR-0011: Artifacts in S3-compatible object storage, metadata in PostgreSQL

- Status: Accepted
- Date: 2026-10-10

## Context

Jobs produce specifications, task lists, diffs, test reports, and
checkpoints. These range from bytes to tens of megabytes. They need integrity
checks, retention, per-tenant isolation, and safe download in the browser.

## Decision

1. **Content goes to S3-compatible object storage** (AWS S3, MinIO, Ceph).
   - Keys: `tenants/<tenant>/jobs/<job>/<artifact>/<name>`, with every
     segment sanitized.
   - Optional server-side encryption: `ASTRION_S3_SSE`.
   - Credentials come from the standard AWS environment and role chain.
2. **Metadata goes to PostgreSQL:** type, name, content type, size, SHA-256,
   classification (`USER` or `INTERNAL`), source step, repository, retention.
3. **Uploads go through the internal execution API**:
   - The declared size is checked and capped (`ASTRION_MAX_ARTIFACT_BYTES`,
     50 MiB by default).
   - The hash is computed while streaming.
   - If storing the metadata fails, the object is deleted.
4. **Downloads** are tenant-checked. `INTERNAL` artifacts (checkpoints) are
   never served to users. Responses carry `Content-Security-Policy: sandbox;
   default-src 'none'`. Inline preview is allowed only for `text/*` and JSON.
5. **Local filesystem store** for development. It is **refused in production**.

## Consequences

- PostgreSQL stays small. Object storage handles volume and lifecycle rules.
- Downloads currently stream through the API. Pre-signed URLs are a Release 3
  optimisation once per-tenant bucket policies are in place.

## Alternatives considered

- **Blobs in PostgreSQL.** Simple, but it bloats backups and replication.
  Rejected.
- **Agents writing to buckets directly.** That would require
  bucket credentials inside untrusted environments. Rejected.
