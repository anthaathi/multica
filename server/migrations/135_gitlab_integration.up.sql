-- GitLab (self-hosted) integration: OAuth-connected instances, mirrored merge
-- request state, pipeline (CI) status, and the link table joining issues ↔ MRs.
-- Structurally parallel to the GitHub integration (migrations 079/091/096/…),
-- but keyed for GitLab: a connection is per-workspace (OAuth token) rather than
-- a shared numeric App installation, and CI status comes from pipelines rather
-- than check suites.

CREATE TABLE gitlab_connection (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id            UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    -- Base URL of the self-hosted (or SaaS) GitLab instance, e.g.
    -- https://gitlab.example.com. Stored without a trailing slash.
    gitlab_base_url         TEXT NOT NULL,
    gitlab_user_id          BIGINT NOT NULL,
    gitlab_username         TEXT NOT NULL,
    gitlab_avatar_url       TEXT,
    -- OAuth tokens and the per-connection webhook secret are encrypted at rest
    -- with the MULTICA_GITLAB_SECRET_KEY secretbox, mirroring the Slack/Lark
    -- token storage. Never stored in plaintext.
    access_token_encrypted  BYTEA NOT NULL,
    refresh_token_encrypted BYTEA,
    token_expires_at        TIMESTAMPTZ,
    webhook_secret_encrypted BYTEA NOT NULL,
    connected_by_id         UUID REFERENCES "user"(id) ON DELETE SET NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, gitlab_base_url, gitlab_user_id)
);

CREATE INDEX idx_gitlab_connection_workspace ON gitlab_connection(workspace_id);

CREATE TABLE gitlab_merge_request (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    connection_id   UUID NOT NULL REFERENCES gitlab_connection(id) ON DELETE CASCADE,
    -- project_id is GitLab's numeric project id (stable across renames);
    -- namespace_path/project_path mirror GitHub's repo_owner/repo_name for
    -- display and identifier extraction.
    project_id      BIGINT NOT NULL,
    namespace_path  TEXT NOT NULL,
    project_path    TEXT NOT NULL,
    mr_iid          INTEGER NOT NULL,
    title           TEXT NOT NULL,
    state           TEXT NOT NULL
        CHECK (state IN ('open', 'closed', 'merged', 'draft')),
    web_url         TEXT NOT NULL,
    source_branch   TEXT,
    author_username TEXT,
    author_avatar_url TEXT,
    merged_at       TIMESTAMPTZ,
    closed_at       TIMESTAMPTZ,
    mr_created_at   TIMESTAMPTZ NOT NULL,
    mr_updated_at   TIMESTAMPTZ NOT NULL,
    head_sha        TEXT NOT NULL DEFAULT '',
    -- Mirrors GitLab's merge_status. The UI only surfaces clean/dirty (mapped
    -- from can_be_merged/cannot_be_merged); other values round-trip.
    merge_status    TEXT,
    additions       INTEGER NOT NULL DEFAULT 0,
    deletions       INTEGER NOT NULL DEFAULT 0,
    changed_files   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, project_id, mr_iid)
);

CREATE INDEX idx_gitlab_merge_request_workspace ON gitlab_merge_request(workspace_id);

CREATE TABLE gitlab_merge_request_pipeline (
    mr_id        UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    pipeline_id  BIGINT NOT NULL,
    head_sha     TEXT NOT NULL,
    status       TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (mr_id, pipeline_id)
);

CREATE INDEX idx_gitlab_mr_pipeline_aggregate
    ON gitlab_merge_request_pipeline (mr_id, head_sha, updated_at DESC);

-- Stash for Pipeline Hook events that arrive before the matching MR row has
-- been mirrored (Merge Request and Pipeline hooks are delivered independently
-- and ordering is not guaranteed). Drained and replayed when the MR upsert
-- lands. Keyed so repeated deliveries of the same pipeline are idempotent.
CREATE TABLE gitlab_pending_pipeline (
    workspace_id     UUID NOT NULL,
    connection_id    UUID NOT NULL,
    project_id       BIGINT NOT NULL,
    mr_iid           INTEGER NOT NULL,
    pipeline_id      BIGINT NOT NULL,
    head_sha         TEXT NOT NULL,
    status           TEXT NOT NULL,
    pipeline_updated_at TIMESTAMPTZ NOT NULL,
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, project_id, mr_iid, pipeline_id)
);

CREATE INDEX idx_gitlab_pending_pipeline_received_at
    ON gitlab_pending_pipeline(received_at);

-- Link table joining issues ↔ merge requests, mirroring issue_pull_request
-- (including the close_intent and reference_only columns introduced for GitHub
-- in migrations 109 and 127).
CREATE TABLE issue_merge_request (
    issue_id         UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    merge_request_id UUID NOT NULL REFERENCES gitlab_merge_request(id) ON DELETE CASCADE,
    linked_by_type   TEXT,
    linked_by_id     UUID,
    close_intent     BOOLEAN NOT NULL DEFAULT FALSE,
    reference_only   BOOLEAN NOT NULL DEFAULT FALSE,
    linked_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, merge_request_id)
);

CREATE INDEX idx_issue_merge_request_mr ON issue_merge_request(merge_request_id);
