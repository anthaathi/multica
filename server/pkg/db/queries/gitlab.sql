-- =====================
-- GitLab Connection
-- =====================

-- name: CreateGitLabConnection :one
INSERT INTO gitlab_connection (
    workspace_id, gitlab_base_url, gitlab_user_id, gitlab_username,
    gitlab_avatar_url, access_token_encrypted, refresh_token_encrypted,
    token_expires_at, webhook_secret_encrypted, connected_by_id
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg('gitlab_avatar_url'), $5, sqlc.narg('refresh_token_encrypted'),
    sqlc.narg('token_expires_at'), $6, sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, gitlab_base_url, gitlab_user_id) DO UPDATE SET
    gitlab_username          = EXCLUDED.gitlab_username,
    gitlab_avatar_url        = EXCLUDED.gitlab_avatar_url,
    access_token_encrypted   = EXCLUDED.access_token_encrypted,
    refresh_token_encrypted  = EXCLUDED.refresh_token_encrypted,
    token_expires_at         = EXCLUDED.token_expires_at,
    webhook_secret_encrypted = EXCLUDED.webhook_secret_encrypted,
    connected_by_id          = EXCLUDED.connected_by_id,
    updated_at               = now()
RETURNING *;

-- name: ListGitLabConnectionsByWorkspace :many
SELECT * FROM gitlab_connection
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetGitLabConnectionByID :one
SELECT * FROM gitlab_connection
WHERE id = $1;

-- name: ListGitLabConnectionsByProject :many
-- Every workspace whose connection to this GitLab instance could own the
-- project. Ordered so the oldest binding is the deterministic routing fallback.
SELECT * FROM gitlab_connection
WHERE gitlab_base_url = $1
ORDER BY created_at ASC, id ASC;

-- name: DeleteGitLabConnection :exec
DELETE FROM gitlab_connection WHERE id = $1 AND workspace_id = $2;

-- name: UpdateGitLabConnectionTokens :exec
-- GitLab rotates refresh tokens: every refresh returns a replacement and
-- invalidates the old one, so both tokens are always written together.
UPDATE gitlab_connection
SET access_token_encrypted  = $2,
    refresh_token_encrypted = $3,
    token_expires_at        = $4,
    updated_at              = now()
WHERE id = $1;

-- =====================
-- GitLab Merge Request
-- =====================

-- name: UpsertGitLabMergeRequest :one
-- merge_status has the same three-state semantics on UPDATE as GitHub's
-- mergeable_state:
--   1. clear_merge_status=true → write NULL (state-changing actions invalidate
--      the prior verdict).
--   2. clear_merge_status=false, merge_status non-null → write the value.
--   3. clear_merge_status=false, merge_status null → preserve existing column.
INSERT INTO gitlab_merge_request (
    workspace_id, connection_id, project_id, namespace_path, project_path,
    mr_iid, title, state, web_url, source_branch, author_username,
    author_avatar_url, merged_at, closed_at, mr_created_at, mr_updated_at,
    head_sha, merge_status, additions, deletions, changed_files
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, sqlc.narg('source_branch'), sqlc.narg('author_username'),
    sqlc.narg('author_avatar_url'), sqlc.narg('merged_at'), sqlc.narg('closed_at'), $10, $11,
    $12, sqlc.narg('merge_status'), $13, $14, $15
)
ON CONFLICT (workspace_id, project_id, mr_iid) DO UPDATE SET
    connection_id  = EXCLUDED.connection_id,
    namespace_path = EXCLUDED.namespace_path,
    project_path   = EXCLUDED.project_path,
    title          = EXCLUDED.title,
    state          = EXCLUDED.state,
    web_url        = EXCLUDED.web_url,
    source_branch  = EXCLUDED.source_branch,
    author_username = EXCLUDED.author_username,
    author_avatar_url = EXCLUDED.author_avatar_url,
    merged_at      = EXCLUDED.merged_at,
    closed_at      = EXCLUDED.closed_at,
    mr_updated_at  = EXCLUDED.mr_updated_at,
    head_sha       = EXCLUDED.head_sha,
    merge_status   = CASE
        WHEN COALESCE(sqlc.narg('clear_merge_status')::boolean, FALSE) THEN NULL
        WHEN EXCLUDED.merge_status IS NOT NULL THEN EXCLUDED.merge_status
        ELSE gitlab_merge_request.merge_status
    END,
    additions     = EXCLUDED.additions,
    deletions     = EXCLUDED.deletions,
    changed_files = EXCLUDED.changed_files,
    updated_at    = now()
RETURNING *;

-- name: GetGitLabMergeRequest :one
SELECT * FROM gitlab_merge_request
WHERE workspace_id = $1 AND project_id = $2 AND mr_iid = $3;

-- name: ListMergeRequestsByIssue :many
-- Returns the issue's linked MRs with the aggregated pipeline status for the
-- MR's CURRENT head SHA. Mirrors ListPullRequestsByIssue: narrow to this
-- issue's MR ids first, take the latest pipeline per MR for the current head,
-- and collapse pipeline status into passed/failed/pending counts. reference_only
-- links are filtered out (a bare mention is not a working MR for the issue).
WITH issue_mrs AS (
    SELECT mr.id, mr.head_sha
    FROM gitlab_merge_request mr
    JOIN issue_merge_request imr ON imr.merge_request_id = mr.id
    WHERE imr.issue_id = sqlc.arg('issue_id') AND NOT imr.reference_only
),
latest_pipeline AS (
    SELECT DISTINCT ON (pl.mr_id)
        pl.mr_id, pl.status
    FROM gitlab_merge_request_pipeline pl
    JOIN issue_mrs im ON im.id = pl.mr_id
    WHERE pl.head_sha = im.head_sha AND im.head_sha <> ''
    ORDER BY pl.mr_id, pl.updated_at DESC
),
checks AS (
    SELECT
        mr_id,
        COUNT(*)::bigint AS total,
        SUM(CASE WHEN status IN ('failed', 'canceled', 'cancelled') THEN 1 ELSE 0 END)::bigint AS failed,
        SUM(CASE WHEN status IN ('success', 'manual', 'skipped') THEN 1 ELSE 0 END)::bigint AS passed,
        SUM(CASE WHEN status IN ('created', 'waiting_for_resource', 'preparing',
                'pending', 'running', 'scheduled') THEN 1 ELSE 0 END)::bigint AS pending
    FROM latest_pipeline
    GROUP BY mr_id
)
SELECT
    mr.id, mr.workspace_id, mr.connection_id, mr.project_id, mr.namespace_path,
    mr.project_path, mr.mr_iid, mr.title, mr.state, mr.web_url, mr.source_branch,
    mr.author_username, mr.author_avatar_url, mr.merged_at, mr.closed_at,
    mr.mr_created_at, mr.mr_updated_at, mr.head_sha, mr.merge_status,
    mr.additions, mr.deletions, mr.changed_files,
    mr.created_at, mr.updated_at,
    COALESCE(c.total, 0)::bigint   AS checks_total,
    COALESCE(c.passed, 0)::bigint  AS checks_passed,
    COALESCE(c.failed, 0)::bigint  AS checks_failed,
    COALESCE(c.pending, 0)::bigint AS checks_pending
FROM gitlab_merge_request mr
JOIN issue_merge_request imr ON imr.merge_request_id = mr.id
LEFT JOIN checks c ON c.mr_id = mr.id
WHERE imr.issue_id = sqlc.arg('issue_id') AND NOT imr.reference_only
ORDER BY mr.mr_created_at DESC;

-- name: ListIssueIDsForMergeRequest :many
SELECT issue_id FROM issue_merge_request
WHERE merge_request_id = $1;

-- name: GetIssueMergeRequestCloseAggregate :one
-- Aggregates the issue's linked MRs into the two counts that gate auto-advance:
-- how many are still in flight (open/draft) and how many merged MRs declared
-- explicit closing intent. reference_only links are excluded (see the GitHub
-- equivalent GetIssuePullRequestCloseAggregate for the full rationale).
SELECT
    COALESCE(SUM(CASE WHEN mr.state IN ('open', 'draft') THEN 1 ELSE 0 END), 0)::bigint AS open_count,
    COALESCE(SUM(CASE WHEN mr.state = 'merged' AND imr.close_intent THEN 1 ELSE 0 END), 0)::bigint AS merged_with_close_intent_count
FROM gitlab_merge_request mr
JOIN issue_merge_request imr ON imr.merge_request_id = mr.id
WHERE imr.issue_id = $1 AND NOT imr.reference_only;

-- =====================
-- GitLab MR pipeline
-- =====================

-- name: UpsertGitLabPipeline :exec
-- Upserts a single pipeline row keyed by (mr_id, pipeline_id). The updated_at
-- guard prevents a late-arriving older event from overwriting a newer one.
INSERT INTO gitlab_merge_request_pipeline (
    mr_id, pipeline_id, head_sha, status, updated_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (mr_id, pipeline_id) DO UPDATE SET
    head_sha   = EXCLUDED.head_sha,
    status     = EXCLUDED.status,
    updated_at = EXCLUDED.updated_at
WHERE EXCLUDED.updated_at >= gitlab_merge_request_pipeline.updated_at;

-- =====================
-- GitLab pending pipeline (out-of-order arrival stash)
-- =====================

-- name: UpsertPendingPipeline :exec
INSERT INTO gitlab_pending_pipeline (
    workspace_id, connection_id, project_id, mr_iid,
    pipeline_id, head_sha, status, pipeline_updated_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8
)
ON CONFLICT (workspace_id, project_id, mr_iid, pipeline_id) DO UPDATE SET
    connection_id       = EXCLUDED.connection_id,
    head_sha            = EXCLUDED.head_sha,
    status              = EXCLUDED.status,
    pipeline_updated_at = EXCLUDED.pipeline_updated_at,
    received_at         = now()
WHERE EXCLUDED.pipeline_updated_at >= gitlab_pending_pipeline.pipeline_updated_at;

-- name: DrainPendingPipelinesForMR :many
-- Atomically reads + deletes all pending pipelines for the given MR address.
DELETE FROM gitlab_pending_pipeline
WHERE workspace_id = $1
  AND project_id   = $2
  AND mr_iid       = $3
RETURNING pipeline_id, head_sha, status, pipeline_updated_at;

-- =====================
-- Issue ↔ Merge Request link
-- =====================

-- name: LinkIssueToMergeRequest :exec
-- Mirrors LinkIssueToPullRequest: close_intent and reference_only follow the
-- same preserve gate so a post-terminal edit can't rewrite the merge-time
-- decision or retroactively hide a working MR.
INSERT INTO issue_merge_request (
    issue_id, merge_request_id, linked_by_type, linked_by_id, close_intent, reference_only
) VALUES (
    $1, $2, sqlc.narg('linked_by_type'), sqlc.narg('linked_by_id'), $3, sqlc.arg('reference_only')
)
ON CONFLICT (issue_id, merge_request_id) DO UPDATE SET
    close_intent = CASE
        WHEN sqlc.arg('preserve_close_intent') THEN issue_merge_request.close_intent
        ELSE EXCLUDED.close_intent
    END,
    reference_only = CASE
        WHEN sqlc.arg('preserve_close_intent') THEN issue_merge_request.reference_only
        ELSE EXCLUDED.reference_only
    END;

-- name: UnlinkIssueFromMergeRequest :exec
DELETE FROM issue_merge_request
WHERE issue_id = $1 AND merge_request_id = $2;
