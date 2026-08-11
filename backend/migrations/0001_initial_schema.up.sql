-- Beuvian initial schema.
--
-- Conventions applied throughout (see docs/DATABASE.md for the reasoning):
--   * TEXT primary keys holding prefixed ULIDs ("dev_01J9Z…") — time-sortable, so
--     inserts land at the right edge of the B-tree instead of scattering pages.
--   * TIMESTAMPTZ everywhere, never TIMESTAMP: users span time zones and a naive
--     column silently discards the offset.
--   * Every foreign key has an explicit ON DELETE and its own index. PostgreSQL
--     does not index FKs automatically, and a missing one turns a cascade delete
--     into a sequential scan per row.
--   * Soft deletes only where recovery matters. Soft-deleting everything forces
--     `WHERE deleted_at IS NULL` into every query, and one omission silently
--     resurrects deleted data.
--   * organization_id is present but unused: teams are deferred, and adding the
--     column now makes enabling them a constraint change rather than a backfill.

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    -- Keyed on GitHub's numeric id, NOT the login: logins are renameable, and
    -- keying on one would turn a renamed account into a different person.
    github_id       BIGINT      NOT NULL UNIQUE,
    github_login    TEXT        NOT NULL,
    email           TEXT,
    name            TEXT,
    avatar_url      TEXT,
    organization_id TEXT,
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,

    CONSTRAINT users_github_login_not_blank CHECK (length(trim(github_login)) > 0)
);

CREATE INDEX idx_users_github_login ON users (github_login);
CREATE INDEX idx_users_organization ON users (organization_id) WHERE organization_id IS NOT NULL;
CREATE INDEX idx_users_active ON users (id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- oauth_accounts
-- Separate from users so a second provider is a new row, not a schema change.
-- ---------------------------------------------------------------------------
CREATE TABLE oauth_accounts (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider               TEXT        NOT NULL,
    provider_user_id       TEXT        NOT NULL,
    -- Encrypted at rest. Used only to read repository metadata; this is never a
    -- model-provider credential, because Beuvian holds none.
    access_token_encrypted TEXT,
    scopes                 TEXT[]      NOT NULL DEFAULT '{}',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT oauth_accounts_provider_valid CHECK (provider IN ('github')),
    CONSTRAINT oauth_accounts_unique_identity UNIQUE (provider, provider_user_id)
);

CREATE INDEX idx_oauth_accounts_user ON oauth_accounts (user_id);

-- ---------------------------------------------------------------------------
-- devices
-- ---------------------------------------------------------------------------
CREATE TABLE devices (
    id               TEXT PRIMARY KEY,
    user_id          TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name             TEXT        NOT NULL,
    platform         TEXT        NOT NULL,
    agent_version    TEXT,
    -- Adapters actually INSTALLED on that machine, from the AUTH handshake. Not
    -- what the binary supports: this is what stops a prompt being dispatched to a
    -- device that cannot service it.
    capabilities     TEXT[]      NOT NULL DEFAULT '{}',
    -- Only the SHA-256 is stored. The plaintext is returned once at registration,
    -- so a database dump yields no working credentials.
    token_hash       TEXT        NOT NULL,
    token_expires_at TIMESTAMPTZ NOT NULL,
    last_seen_at     TIMESTAMPTZ,
    -- Separate from deleted_at: revoking a compromised machine's access is not the
    -- same act as removing it from the user's list.
    revoked_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at       TIMESTAMPTZ,

    CONSTRAINT devices_name_not_blank CHECK (length(trim(name)) > 0)
);

CREATE INDEX idx_devices_user ON devices (user_id);
CREATE UNIQUE INDEX idx_devices_token_hash ON devices (token_hash);
-- Partial index for the hot path: listing a user's live devices. Stays small no
-- matter how many devices are retired.
CREATE INDEX idx_devices_user_active ON devices (user_id, last_seen_at DESC)
    WHERE deleted_at IS NULL AND revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- repositories
-- ---------------------------------------------------------------------------
CREATE TABLE repositories (
    id             TEXT PRIMARY KEY,
    user_id        TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    full_name      TEXT        NOT NULL,
    local_path     TEXT,
    -- SET NULL, not CASCADE: removing a laptop must not delete the record of
    -- repositories worked on there.
    device_id      TEXT REFERENCES devices (id) ON DELETE SET NULL,
    default_branch TEXT,
    github_id      BIGINT,
    is_private     BOOLEAN     NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ,

    CONSTRAINT repositories_full_name_shape CHECK (full_name ~ '^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$'),
    -- A local path without a device is meaningless: the same path on another
    -- machine is a different directory.
    CONSTRAINT repositories_path_needs_device CHECK (local_path IS NULL OR device_id IS NOT NULL)
);

CREATE INDEX idx_repositories_user ON repositories (user_id);
CREATE INDEX idx_repositories_device ON repositories (device_id) WHERE device_id IS NOT NULL;
CREATE UNIQUE INDEX idx_repositories_unique ON repositories (user_id, full_name, COALESCE(device_id, ''))
    WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id                TEXT PRIMARY KEY,
    user_id           TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id         TEXT        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    repository_id     TEXT REFERENCES repositories (id) ON DELETE SET NULL,
    adapter           TEXT        NOT NULL,
    -- TEXT + CHECK rather than a PostgreSQL ENUM: adding a value to an ENUM needs
    -- ALTER TYPE, which complicates rollback, whereas a CHECK is edited like any
    -- other constraint. The values are validated in Go regardless.
    state             TEXT        NOT NULL,
    current_task      TEXT,
    working_directory TEXT        NOT NULL,
    pid               INTEGER,
    -- NULL means "has not exited". A sentinel integer cannot express that: -1 is a
    -- real exit code and 0 reads as success.
    exit_code         INTEGER,
    started_at        TIMESTAMPTZ NOT NULL,
    ended_at          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT sessions_state_valid CHECK (state IN
        ('idle', 'starting', 'running', 'waiting_input', 'stopping', 'stopped', 'crashed')),
    CONSTRAINT sessions_ended_after_started CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE INDEX idx_sessions_user_started ON sessions (user_id, started_at DESC);
CREATE INDEX idx_sessions_device_started ON sessions (device_id, started_at DESC);
CREATE INDEX idx_sessions_repository ON sessions (repository_id) WHERE repository_id IS NOT NULL;
-- The hot query: "is anything running on this device?". Partial, so it stays small
-- no matter how much finished history accumulates.
CREATE INDEX idx_sessions_active ON sessions (device_id, started_at DESC) WHERE ended_at IS NULL;
-- At most one live session per device, enforced by the database rather than by a
-- read-then-write in application code that two concurrent requests could both pass.
CREATE UNIQUE INDEX idx_sessions_one_active_per_device ON sessions (device_id) WHERE ended_at IS NULL;

-- ---------------------------------------------------------------------------
-- session_logs
-- The highest-volume table by a wide margin.
-- ---------------------------------------------------------------------------
CREATE TABLE session_logs (
    -- BIGSERIAL, not a ULID: 8 bytes versus 26 is material across millions of
    -- rows, and these are never referenced by external clients.
    id         BIGSERIAL PRIMARY KEY,
    session_id TEXT        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        BIGINT      NOT NULL,
    stream     TEXT        NOT NULL,
    content    TEXT        NOT NULL,
    truncated  BOOLEAN     NOT NULL DEFAULT false,
    at         TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT session_logs_stream_valid CHECK (stream IN ('stdout', 'stderr', 'system')),
    -- Makes ingestion idempotent: a batch redelivered after a reconnect conflicts
    -- instead of duplicating the transcript.
    CONSTRAINT session_logs_unique_seq UNIQUE (session_id, seq)
);

CREATE INDEX idx_session_logs_session_seq ON session_logs (session_id, seq);
-- Supports the retention sweep without scanning the whole table.
CREATE INDEX idx_session_logs_created ON session_logs (created_at);

-- ---------------------------------------------------------------------------
-- messages
-- The human-readable conversation, distinct from raw process output.
-- ---------------------------------------------------------------------------
CREATE TABLE messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    user_id    TEXT REFERENCES users (id) ON DELETE SET NULL,
    role       TEXT        NOT NULL,
    content    TEXT        NOT NULL,
    prompt_id  TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT messages_role_valid CHECK (role IN ('user', 'agent', 'system'))
);

CREATE INDEX idx_messages_session ON messages (session_id, created_at);

-- ---------------------------------------------------------------------------
-- prompt_queue
-- The durable system of record for prompts. Redis only accelerates dispatch;
-- see docs/adr/0006-prompt-queue-postgres-authoritative.md.
-- ---------------------------------------------------------------------------
CREATE TABLE prompt_queue (
    id             TEXT PRIMARY KEY,
    user_id        TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id      TEXT        NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    session_id     TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    text           TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'pending',
    attempts       INTEGER     NOT NULL DEFAULT 0,
    enqueued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at  TIMESTAMPTZ,
    delivered_at   TIMESTAMPTZ,
    error          TEXT,
    correlation_id TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT prompt_queue_status_valid CHECK (status IN
        ('pending', 'dispatched', 'delivered', 'failed', 'cancelled')),
    CONSTRAINT prompt_queue_text_not_blank CHECK (length(trim(text)) > 0),
    CONSTRAINT prompt_queue_attempts_sane CHECK (attempts >= 0)
);

CREATE INDEX idx_prompt_queue_user ON prompt_queue (user_id, enqueued_at DESC);
-- The reconciliation path that makes Redis disposable.
CREATE INDEX idx_prompt_queue_pending ON prompt_queue (device_id, enqueued_at)
    WHERE status IN ('pending', 'dispatched');
CREATE INDEX idx_prompt_queue_session ON prompt_queue (session_id) WHERE session_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- agent_status
-- One row per device, updated in place. Deliberately NOT a time series: a
-- 10-second heartbeat appended forever would make this the largest table in the
-- database in exchange for history nobody asked for.
-- ---------------------------------------------------------------------------
CREATE TABLE agent_status (
    device_id      TEXT PRIMARY KEY REFERENCES devices (id) ON DELETE CASCADE,
    state          TEXT        NOT NULL,
    adapter        TEXT,
    repository     TEXT,
    current_task   TEXT,
    cpu_percent    REAL        NOT NULL DEFAULT 0,
    memory_bytes   BIGINT      NOT NULL DEFAULT 0,
    queued_prompts INTEGER     NOT NULL DEFAULT 0,
    session_id     TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    reported_at    TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT agent_status_state_valid CHECK (state IN
        ('idle', 'starting', 'running', 'waiting_input', 'stopping', 'stopped', 'crashed')),
    CONSTRAINT agent_status_cpu_sane CHECK (cpu_percent >= 0),
    CONSTRAINT agent_status_memory_sane CHECK (memory_bytes >= 0)
);

-- ---------------------------------------------------------------------------
-- notifications
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    device_id          TEXT REFERENCES devices (id) ON DELETE SET NULL,
    session_id         TEXT REFERENCES sessions (id) ON DELETE SET NULL,
    -- Machine-readable, so the planned WhatsApp/Telegram/push channels can route
    -- on it without parsing prose.
    kind               TEXT        NOT NULL,
    title              TEXT        NOT NULL,
    body               TEXT,
    severity           TEXT        NOT NULL DEFAULT 'info',
    read_at            TIMESTAMPTZ,
    -- Present now so adding a delivery channel needs no migration.
    delivered_channels TEXT[]      NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notifications_severity_valid CHECK (severity IN ('info', 'warning', 'error')),
    CONSTRAINT notifications_title_not_blank CHECK (length(trim(title)) > 0)
);

CREATE INDEX idx_notifications_user_created ON notifications (user_id, created_at DESC);
-- Powers the unread badge without scanning a user's whole history.
CREATE INDEX idx_notifications_unread ON notifications (user_id, created_at DESC) WHERE read_at IS NULL;

-- ---------------------------------------------------------------------------
-- user_settings
-- One row per user, created on first login so reads never handle absence.
-- ---------------------------------------------------------------------------
CREATE TABLE user_settings (
    user_id                   TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    notify_on_complete        BOOLEAN     NOT NULL DEFAULT true,
    notify_on_waiting         BOOLEAN     NOT NULL DEFAULT true,
    -- Defaults OFF: laptops disconnect constantly, and notifying every time would
    -- train the user to ignore all notifications.
    notify_on_device_offline  BOOLEAN     NOT NULL DEFAULT false,
    theme                     TEXT        NOT NULL DEFAULT 'system',
    timezone                  TEXT        NOT NULL DEFAULT 'UTC',
    log_retention_days        INTEGER     NOT NULL DEFAULT 30,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT user_settings_theme_valid CHECK (theme IN ('system', 'light', 'dark')),
    CONSTRAINT user_settings_retention_sane CHECK (log_retention_days BETWEEN 1 AND 365)
);

-- ---------------------------------------------------------------------------
-- refresh_tokens
-- Rotation with reuse detection: each refresh issues a new token in the same
-- family and marks the old one used. Presenting an already-used token means it was
-- stolen, so the entire family is revoked.
-- ---------------------------------------------------------------------------
CREATE TABLE refresh_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Only the hash. The plaintext exists solely in the client's cookie, so a
    -- database dump cannot be replayed as a login.
    token_hash TEXT        NOT NULL UNIQUE,
    family_id  TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    user_agent TEXT,
    ip_address TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id);
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens (family_id);
CREATE INDEX idx_refresh_tokens_expires ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------------------
CREATE TABLE audit_log (
    id             BIGSERIAL PRIMARY KEY,
    -- SET NULL, not CASCADE: deleting a user must not erase the audit trail of
    -- what that user did. This is the one place preserving the row outranks the
    -- cascade, and getting it wrong makes the log useless exactly when it matters.
    user_id        TEXT REFERENCES users (id) ON DELETE SET NULL,
    action         TEXT        NOT NULL,
    target_type    TEXT,
    target_id      TEXT,
    metadata       JSONB,
    ip_address     TEXT,
    user_agent     TEXT,
    correlation_id TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_user_created ON audit_log (user_id, created_at DESC);
CREATE INDEX idx_audit_log_action ON audit_log (action, created_at DESC);
CREATE INDEX idx_audit_log_correlation ON audit_log (correlation_id) WHERE correlation_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- updated_at maintenance
--
-- A trigger rather than relying on every UPDATE to set it. An application that
-- forgets leaves a stale timestamp, and stale updated_at values are the kind of
-- bug that is discovered months later while debugging something else.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_oauth_accounts_updated_at BEFORE UPDATE ON oauth_accounts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_devices_updated_at BEFORE UPDATE ON devices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_repositories_updated_at BEFORE UPDATE ON repositories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_sessions_updated_at BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_prompt_queue_updated_at BEFORE UPDATE ON prompt_queue
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_notifications_updated_at BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_user_settings_updated_at BEFORE UPDATE ON user_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_agent_status_updated_at BEFORE UPDATE ON agent_status
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
