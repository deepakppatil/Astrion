-- Astrion application schema, version 1.
-- The workflow engine (Temporal) owns its own persistence. This schema holds
-- the platform's business representation: entities, job status, interaction
-- records, artifact metadata, the ordered job event log (also used as the
-- outbox for live updates), and the audit trail.

CREATE TABLE tenants (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL DEFAULT 'ACTIVE',
    quotas              JSONB NOT NULL DEFAULT '{}'::jsonb,
    retention_days      INT  NOT NULL DEFAULT 90,
    permitted_providers TEXT[] NOT NULL DEFAULT '{}',
    security_policy     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    subject      TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, subject)
);

CREATE TABLE role_assignments (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role)
);

CREATE TABLE workspaces (
    id                       TEXT PRIMARY KEY,
    tenant_id                TEXT NOT NULL REFERENCES tenants(id),
    name                     TEXT NOT NULL,
    description              TEXT NOT NULL DEFAULT '',
    type                     TEXT NOT NULL,
    lifecycle_mode           TEXT NOT NULL,
    data_classification      TEXT NOT NULL DEFAULT 'INTERNAL',
    default_agent_profile_id TEXT NOT NULL DEFAULT '',
    default_framework_id     TEXT NOT NULL DEFAULT '',
    default_provider         TEXT NOT NULL DEFAULT '',
    permitted_agent_profiles TEXT[] NOT NULL DEFAULT '{}',
    permitted_providers      TEXT[] NOT NULL DEFAULT '{}',
    created_by               TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE repositories (
    id                   TEXT PRIMARY KEY,
    tenant_id            TEXT NOT NULL REFERENCES tenants(id),
    workspace_id         TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    provider             TEXT NOT NULL,
    url                  TEXT NOT NULL,
    namespace            TEXT NOT NULL DEFAULT '',
    default_branch       TEXT NOT NULL DEFAULT 'main',
    credential_ref       TEXT NOT NULL DEFAULT '',
    checkout_policy      TEXT NOT NULL DEFAULT 'SHALLOW',
    permitted_operations TEXT[] NOT NULL DEFAULT '{read}',
    pinned_revision      TEXT NOT NULL DEFAULT '',
    last_validated_at    TIMESTAMPTZ,
    validation_error     TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, url)
);
CREATE INDEX repositories_tenant_idx ON repositories (tenant_id);

CREATE TABLE integrations (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    type           TEXT NOT NULL, -- GITHUB | GITLAB | JIRA
    name           TEXT NOT NULL,
    config         JSONB NOT NULL DEFAULT '{}'::jsonb,
    credential_ref TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'UNVALIDATED',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, type, name)
);

-- Agent profiles are versioned; every change inserts a new version.
CREATE TABLE agent_profiles (
    id               TEXT NOT NULL,
    version          INT  NOT NULL,
    tenant_id        TEXT NOT NULL REFERENCES tenants(id),
    name             TEXT NOT NULL,
    provider         TEXT NOT NULL,
    cli_name         TEXT NOT NULL DEFAULT '',
    cli_version      TEXT NOT NULL DEFAULT '',
    runtime_image    TEXT NOT NULL DEFAULT '',
    auth_method      TEXT NOT NULL DEFAULT 'none',
    interaction_mode TEXT NOT NULL DEFAULT '',
    environment      JSONB NOT NULL DEFAULT '{}'::jsonb,
    default_args     TEXT[] NOT NULL DEFAULT '{}',
    resources        JSONB NOT NULL DEFAULT '{}'::jsonb,
    execution_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, version)
);
CREATE INDEX agent_profiles_tenant_idx ON agent_profiles (tenant_id, id, version DESC);

-- Framework profiles pin an immutable revision. tenant_id NULL = platform-wide.
CREATE TABLE framework_profiles (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT REFERENCES tenants(id),
    name               TEXT NOT NULL,
    version            TEXT NOT NULL,
    source             TEXT NOT NULL,
    revision           TEXT NOT NULL,
    discovery_strategy TEXT NOT NULL DEFAULT 'DECLARED',
    compatible_agents  TEXT[] NOT NULL DEFAULT '{}',
    required_tools     TEXT[] NOT NULL DEFAULT '{}',
    config             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Versioned command registry. The full definition is stored as JSON so new
-- metadata fields do not require schema migrations; hot fields are columns.
CREATE TABLE command_definitions (
    id           TEXT NOT NULL,
    version      INT  NOT NULL,
    framework_id TEXT NOT NULL REFERENCES framework_profiles(id),
    canonical    TEXT NOT NULL,
    phase        TEXT NOT NULL,
    status       TEXT NOT NULL,
    source       TEXT NOT NULL,
    definition   JSONB NOT NULL,
    created_by   TEXT NOT NULL DEFAULT 'system',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, version),
    UNIQUE (framework_id, canonical, version)
);

CREATE TABLE jobs (
    id                    TEXT PRIMARY KEY,
    tenant_id             TEXT NOT NULL REFERENCES tenants(id),
    workspace_id          TEXT NOT NULL REFERENCES workspaces(id),
    initiated_by          TEXT NOT NULL REFERENCES users(id),
    phase                 TEXT NOT NULL,
    command               TEXT NOT NULL,
    command_id            TEXT NOT NULL,
    command_version       INT  NOT NULL,
    agent_profile_id      TEXT NOT NULL,
    agent_profile_version INT  NOT NULL,
    framework_id          TEXT NOT NULL,
    framework_revision    TEXT NOT NULL,
    requested_provider    TEXT NOT NULL DEFAULT 'AUTO',
    selected_provider     TEXT NOT NULL DEFAULT '',
    inputs                JSONB NOT NULL DEFAULT '{}'::jsonb,
    workflow_id           TEXT NOT NULL UNIQUE,
    workflow_started      BOOLEAN NOT NULL DEFAULT FALSE,
    state                 TEXT NOT NULL,
    current_step          TEXT NOT NULL DEFAULT '',
    pending_outcome       TEXT NOT NULL DEFAULT '',
    failure_class         TEXT NOT NULL DEFAULT '',
    failure_message       TEXT NOT NULL DEFAULT '',
    cleanup_status        TEXT NOT NULL DEFAULT 'NOT_STARTED',
    capacity_held         BOOLEAN NOT NULL DEFAULT FALSE,
    attempt               INT  NOT NULL DEFAULT 0,
    retry_of              TEXT NOT NULL DEFAULT '',
    parent_job_id         TEXT NOT NULL DEFAULT '',
    links                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key       TEXT,
    request_hash          TEXT NOT NULL DEFAULT '',
    cancel_requested_at   TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at            TIMESTAMPTZ,
    finished_at           TIMESTAMPTZ,
    UNIQUE (tenant_id, initiated_by, idempotency_key)
);
CREATE INDEX jobs_tenant_created_idx ON jobs (tenant_id, created_at DESC);
CREATE INDEX jobs_tenant_state_idx ON jobs (tenant_id, state);
CREATE INDEX jobs_unstarted_idx ON jobs (created_at) WHERE workflow_started = FALSE;
CREATE INDEX jobs_user_created_idx ON jobs (initiated_by, created_at DESC);

CREATE TABLE job_repositories (
    job_id             TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    repository_id      TEXT NOT NULL,
    url                TEXT NOT NULL,
    requested_revision TEXT NOT NULL,
    resolved_commit    TEXT NOT NULL DEFAULT '',
    checkout_path      TEXT NOT NULL DEFAULT '',
    working_branch     TEXT NOT NULL DEFAULT '',
    final_commit       TEXT NOT NULL DEFAULT '',
    change_request_url TEXT NOT NULL DEFAULT '',
    position           INT  NOT NULL DEFAULT 0,
    PRIMARY KEY (job_id, repository_id)
);

CREATE TABLE execution_environments (
    id                     TEXT PRIMARY KEY,
    tenant_id              TEXT NOT NULL REFERENCES tenants(id),
    job_id                 TEXT NOT NULL REFERENCES jobs(id),
    attempt                INT  NOT NULL,
    provider               TEXT NOT NULL,
    provider_ref           TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL,
    interaction_mode       TEXT NOT NULL,
    token_hash             TEXT NOT NULL,
    token_expires_at       TIMESTAMPTZ NOT NULL,
    token_revoked_at       TIMESTAMPTZ,
    exit_code              INT,
    result_status          TEXT NOT NULL DEFAULT '',
    summary                TEXT NOT NULL DEFAULT '',
    error_message          TEXT NOT NULL DEFAULT '',
    failure_class          TEXT NOT NULL DEFAULT '',
    pending_interaction_id TEXT NOT NULL DEFAULT '',
    checkpoint_artifact_id TEXT NOT NULL DEFAULT '',
    resume_interaction_id  TEXT NOT NULL DEFAULT '',
    cleanup_status         TEXT NOT NULL DEFAULT 'NOT_STARTED',
    cleanup_error          TEXT NOT NULL DEFAULT '',
    cleanup_attempts       INT  NOT NULL DEFAULT 0,
    expires_at             TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at            TIMESTAMPTZ,
    UNIQUE (job_id, attempt)
);
CREATE INDEX executions_cleanup_idx ON execution_environments (cleanup_status) WHERE cleanup_status <> 'COMPLETED';

CREATE TABLE interactions (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL REFERENCES tenants(id),
    job_id           TEXT NOT NULL REFERENCES jobs(id),
    execution_id     TEXT NOT NULL DEFAULT '',
    step_id          TEXT NOT NULL DEFAULT '',
    agent_request_id TEXT NOT NULL,
    kind             TEXT NOT NULL,
    prompt           TEXT NOT NULL,
    options          JSONB NOT NULL DEFAULT '[]'::jsonb,
    schema           JSONB NOT NULL DEFAULT '{}'::jsonb,
    requested_user   TEXT NOT NULL DEFAULT '',
    requested_role   TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    opened_at        TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    resolved_at      TIMESTAMPTZ,
    UNIQUE (job_id, execution_id, agent_request_id)
);
CREATE INDEX interactions_job_idx ON interactions (job_id, created_at);
CREATE INDEX interactions_open_idx ON interactions (tenant_id, status) WHERE status = 'OPEN';

CREATE TABLE interaction_responses (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    interaction_id  TEXT NOT NULL UNIQUE REFERENCES interactions(id),
    decision        TEXT NOT NULL,
    response_values TEXT[] NOT NULL DEFAULT '{}',
    text            TEXT NOT NULL DEFAULT '',
    comment         TEXT NOT NULL DEFAULT '',
    responded_by    TEXT NOT NULL REFERENCES users(id),
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE artifacts (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    job_id         TEXT NOT NULL REFERENCES jobs(id),
    execution_id   TEXT NOT NULL DEFAULT '',
    type           TEXT NOT NULL,
    name           TEXT NOT NULL,
    object_key     TEXT NOT NULL UNIQUE,
    content_type   TEXT NOT NULL,
    size_bytes     BIGINT NOT NULL,
    sha256         TEXT NOT NULL,
    classification TEXT NOT NULL DEFAULT 'USER',
    source_step    TEXT NOT NULL DEFAULT '',
    repository_id  TEXT NOT NULL DEFAULT '',
    revision       TEXT NOT NULL DEFAULT '',
    retain_until   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX artifacts_job_idx ON artifacts (job_id, created_at);

-- Ordered, durable job event log. Serves SSE replay (Last-Event-ID) and acts
-- as the transactional outbox: rows are written in the same transaction as
-- the state change and pg_notify wakes subscribers after commit.
CREATE TABLE job_events (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    job_id     TEXT NOT NULL,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX job_events_job_idx ON job_events (job_id, id);

CREATE TABLE audit_events (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    actor       TEXT NOT NULL,
    actor_type  TEXT NOT NULL,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id   TEXT NOT NULL,
    details     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_tenant_idx ON audit_events (tenant_id, id DESC);

CREATE TABLE resource_usage (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    job_id        TEXT NOT NULL,
    execution_id  TEXT NOT NULL,
    provider      TEXT NOT NULL,
    wall_seconds  DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX resource_usage_tenant_idx ON resource_usage (tenant_id, recorded_at);
