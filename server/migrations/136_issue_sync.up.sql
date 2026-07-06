-- Bidirectional issue sync: Jira Cloud connections, per-project sync sources,
-- external issue/comment link tables, external identity mapping, and the
-- outbox that drives outbound pushes. Providers: github / gitlab / jira.
--
-- Structurally parallel to the GitHub (079) and GitLab (135) integrations:
-- a jira_connection is per-workspace OAuth like gitlab_connection, tokens are
-- encrypted at rest with a provider-specific secretbox key
-- (MULTICA_JIRA_SECRET_KEY), and webhook secrets are per-connection.

CREATE TABLE jira_connection (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id            UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    -- Atlassian cloud id resolved from /oauth/token/accessible-resources.
    -- All REST calls go through https://api.atlassian.com/ex/jira/{cloud_id}.
    cloud_id                TEXT NOT NULL,
    -- Browser-facing site URL, e.g. https://acme.atlassian.net. Stored
    -- without a trailing slash; used for web links only, never API calls.
    site_url                TEXT NOT NULL,
    account_id              TEXT NOT NULL,
    account_email           TEXT,
    account_avatar_url      TEXT,
    -- Jira Cloud OAuth 2.0 (3LO) uses ROTATING refresh tokens: every refresh
    -- returns a new refresh token and invalidates the old one, so the token
    -- refresher must persist the replacement in the same transaction.
    access_token_encrypted  BYTEA NOT NULL,
    refresh_token_encrypted BYTEA NOT NULL,
    token_expires_at        TIMESTAMPTZ,
    webhook_secret_encrypted BYTEA NOT NULL,
    connected_by_id         UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, cloud_id)
);

CREATE INDEX idx_jira_connection_workspace ON jira_connection(workspace_id);

-- One row per (Multica project, external repo/project) pair. connection_id is
-- deliberately not a foreign key: it points into github_installation,
-- gitlab_connection, or jira_connection depending on provider. Rows are
-- garbage-collected by the delete handlers of those connections (mirroring
-- how PR/MR mirrors cascade), and every read re-validates the connection row
-- exists before using it.
CREATE TABLE issue_sync_source (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    project_id      UUID NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'jira')),
    connection_id   UUID NOT NULL,
    -- Provider-specific pointer to the remote container:
    --   github: {"owner": "acme", "name": "api", "repo_id": 123}
    --   gitlab: {"project_id": 42, "path_with_namespace": "acme/api"}
    --   jira:   {"project_id": "10001", "key": "PROJ"}
    -- external_key is the normalized dedupe string derived from the ref
    -- ("acme/api", "42", "10001") so uniqueness doesn't depend on JSONB
    -- field ordering.
    external_ref    JSONB NOT NULL,
    external_key    TEXT NOT NULL,
    -- Per-source overrides of the default status maps; empty object means
    -- defaults. Shape: {"inbound": {"<remote>": "<multica>"}, "outbound":
    -- {"<multica>": "<remote>"}} with provider-specific remote vocabulary.
    status_mapping  JSONB NOT NULL DEFAULT '{}',
    sync_enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    -- The source that new Multica issues in this project are pushed to.
    -- At most one per project (partial unique index below).
    push_default    BOOLEAN NOT NULL DEFAULT FALSE,
    backfill_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (backfill_status IN ('pending', 'running', 'done', 'failed')),
    backfill_cursor TEXT,
    created_by      UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, provider, external_key)
);

CREATE INDEX idx_issue_sync_source_workspace ON issue_sync_source(workspace_id);
CREATE INDEX idx_issue_sync_source_project ON issue_sync_source(project_id);
-- Webhook routing: find sources for a given remote container fast.
CREATE INDEX idx_issue_sync_source_lookup ON issue_sync_source(provider, external_key);
CREATE UNIQUE INDEX idx_issue_sync_source_push_default
    ON issue_sync_source(project_id) WHERE push_default;

-- Identity of one Multica issue on one remote source. external_id is the
-- provider-stable identifier (GitHub issue node/number pair is not stable
-- across transfers, so we store the numeric issue id; GitLab issue iid is
-- scoped by project so the numeric global id is stored; Jira issue id, not
-- key, survives project moves). external_key is the human handle ("#123",
-- "PROJ-42") used for display.
CREATE TABLE external_issue_link (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    issue_id          UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    sync_source_id    UUID NOT NULL REFERENCES issue_sync_source(id) ON DELETE CASCADE,
    external_id       TEXT NOT NULL,
    external_key      TEXT NOT NULL,
    web_url           TEXT NOT NULL,
    -- Remote-side updated_at from the last applied inbound event; inbound
    -- events older than this are dropped (per-field last-write-wins uses
    -- this as the remote clock).
    remote_updated_at TIMESTAMPTZ,
    -- Hash of the content we last pushed outbound (title|description|status|
    -- labels|assignee). An inbound event whose normalized content hashes to
    -- this value is our own write echoing back and is dropped.
    last_pushed_hash  TEXT NOT NULL DEFAULT '',
    sync_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (sync_source_id, external_id),
    UNIQUE (issue_id, sync_source_id)
);

CREATE INDEX idx_external_issue_link_issue ON external_issue_link(issue_id);
CREATE INDEX idx_external_issue_link_workspace ON external_issue_link(workspace_id);

-- Comment mirror bookkeeping. origin records which side authored the
-- comment; last_pushed_hash suppresses echoes the same way as issues.
CREATE TABLE external_comment_link (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_link_id       UUID NOT NULL REFERENCES external_issue_link(id) ON DELETE CASCADE,
    comment_id          UUID NOT NULL REFERENCES comment(id) ON DELETE CASCADE,
    external_comment_id TEXT NOT NULL,
    origin              TEXT NOT NULL CHECK (origin IN ('local', 'remote')),
    last_pushed_hash    TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issue_link_id, external_comment_id),
    UNIQUE (comment_id, issue_link_id)
);

CREATE INDEX idx_external_comment_link_comment ON external_comment_link(comment_id);

-- Remote account → Multica user mapping. user_id is auto-filled when the
-- remote account's email matches a workspace member's verified email and can
-- be corrected manually later. Rows are created lazily as authors/assignees
-- are first seen in webhook or backfill payloads.
CREATE TABLE external_identity (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    provider            TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'jira')),
    external_account_id TEXT NOT NULL,
    external_login      TEXT,
    display_name        TEXT,
    email               TEXT,
    avatar_url          TEXT,
    user_id             UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, provider, external_account_id)
);

-- Outbound push queue. A DB-backed outbox drained by a polling worker
-- (SELECT ... FOR UPDATE SKIP LOCKED) with exponential backoff. Terminal
-- failures surface on external_issue_link.sync_error.
CREATE TABLE issue_sync_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    sync_source_id  UUID NOT NULL REFERENCES issue_sync_source(id) ON DELETE CASCADE,
    issue_id        UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    op              TEXT NOT NULL
        CHECK (op IN ('create_remote', 'push_issue', 'push_status', 'push_comment')),
    payload         JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'inflight', 'done', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_issue_sync_outbox_dispatch
    ON issue_sync_outbox(status, next_attempt_at) WHERE status IN ('pending', 'inflight');
CREATE INDEX idx_issue_sync_outbox_issue ON issue_sync_outbox(issue_id);

-- Issues created by inbound sync are stamped origin_type='issue_sync' with
-- origin_id = external_issue_link.sync_source_id, following the
-- autopilot/quick_create/lark_chat/slack_chat precedent (042/060/111/131).
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'issue_sync'));
