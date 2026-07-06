-- =====================
-- Issue Sync Sources
-- =====================

-- name: CreateIssueSyncSource :one
INSERT INTO issue_sync_source (
    workspace_id, project_id, provider, connection_id, external_ref,
    external_key, status_mapping, push_default, created_by
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, sqlc.narg('created_by')
)
RETURNING *;

-- name: ListIssueSyncSourcesByProject :many
SELECT * FROM issue_sync_source
WHERE project_id = $1
ORDER BY created_at ASC;

-- name: ListIssueSyncSourcesByWorkspace :many
SELECT * FROM issue_sync_source
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetIssueSyncSource :one
SELECT * FROM issue_sync_source
WHERE id = $1;

-- name: GetIssueSyncSourceInWorkspace :one
SELECT * FROM issue_sync_source
WHERE id = $1 AND workspace_id = $2;

-- name: ListIssueSyncSourcesByExternalKey :many
-- Webhook routing: all enabled sources bound to one remote container.
SELECT * FROM issue_sync_source
WHERE provider = $1 AND external_key = $2 AND sync_enabled
ORDER BY created_at ASC;

-- name: ListIssueSyncSourcesByConnection :many
SELECT * FROM issue_sync_source
WHERE provider = $1 AND connection_id = $2
ORDER BY created_at ASC;

-- name: UpdateIssueSyncSource :one
UPDATE issue_sync_source
SET status_mapping = COALESCE(sqlc.narg('status_mapping'), status_mapping),
    sync_enabled   = COALESCE(sqlc.narg('sync_enabled'), sync_enabled),
    push_default   = COALESCE(sqlc.narg('push_default'), push_default),
    updated_at     = now()
WHERE id = $1
RETURNING *;

-- name: ClearIssueSyncSourcePushDefault :exec
-- At most one push_default per project (partial unique index); clear the
-- previous holder before setting a new one.
UPDATE issue_sync_source
SET push_default = FALSE, updated_at = now()
WHERE project_id = $1 AND push_default;

-- name: UpdateIssueSyncSourceBackfill :exec
UPDATE issue_sync_source
SET backfill_status = $2,
    backfill_cursor = sqlc.narg('backfill_cursor'),
    updated_at      = now()
WHERE id = $1;

-- name: DeleteIssueSyncSource :exec
DELETE FROM issue_sync_source WHERE id = $1 AND workspace_id = $2;

-- name: DeleteIssueSyncSourcesByConnection :exec
-- Garbage collection when a provider connection is removed; connection_id has
-- no FK (it is provider-polymorphic), so the connection delete handlers call
-- this explicitly.
DELETE FROM issue_sync_source WHERE provider = $1 AND connection_id = $2;

-- =====================
-- External Issue Links
-- =====================

-- name: CreateExternalIssueLink :one
INSERT INTO external_issue_link (
    workspace_id, issue_id, sync_source_id, external_id, external_key,
    web_url, remote_updated_at, last_pushed_hash
) VALUES (
    $1, $2, $3, $4, $5,
    $6, sqlc.narg('remote_updated_at'), $7
)
ON CONFLICT (sync_source_id, external_id) DO UPDATE SET
    external_key = EXCLUDED.external_key,
    web_url      = EXCLUDED.web_url,
    updated_at   = now()
RETURNING *;

-- name: GetExternalIssueLinkByExternalID :one
SELECT * FROM external_issue_link
WHERE sync_source_id = $1 AND external_id = $2;

-- name: GetExternalIssueLinkForIssue :one
SELECT * FROM external_issue_link
WHERE issue_id = $1 AND sync_source_id = $2;

-- name: ListExternalIssueLinksByIssue :many
SELECT * FROM external_issue_link
WHERE issue_id = $1
ORDER BY created_at ASC;

-- name: ListExternalIssueLinksByIssues :many
SELECT * FROM external_issue_link
WHERE issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
ORDER BY issue_id, created_at ASC;

-- name: UpdateExternalIssueLinkRemoteState :exec
UPDATE external_issue_link
SET remote_updated_at = $2,
    external_key      = $3,
    web_url           = $4,
    sync_error        = NULL,
    updated_at        = now()
WHERE id = $1;

-- name: UpdateExternalIssueLinkPushedHash :exec
UPDATE external_issue_link
SET last_pushed_hash = $2,
    sync_error       = NULL,
    updated_at       = now()
WHERE id = $1;

-- name: SetExternalIssueLinkSyncError :exec
UPDATE external_issue_link
SET sync_error = $2,
    updated_at = now()
WHERE id = $1;

-- name: DeleteExternalIssueLink :exec
DELETE FROM external_issue_link WHERE id = $1;

-- =====================
-- External Comment Links
-- =====================

-- name: CreateExternalCommentLink :one
INSERT INTO external_comment_link (
    issue_link_id, comment_id, external_comment_id, origin, last_pushed_hash
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (issue_link_id, external_comment_id) DO UPDATE SET
    last_pushed_hash = EXCLUDED.last_pushed_hash,
    updated_at       = now()
RETURNING *;

-- name: GetExternalCommentLinkByExternalID :one
SELECT * FROM external_comment_link
WHERE issue_link_id = $1 AND external_comment_id = $2;

-- name: GetExternalCommentLinkByComment :one
SELECT * FROM external_comment_link
WHERE comment_id = $1 AND issue_link_id = $2;

-- name: UpdateExternalCommentLinkPushedHash :exec
UPDATE external_comment_link
SET last_pushed_hash = $2,
    updated_at       = now()
WHERE id = $1;

-- =====================
-- External Identities
-- =====================

-- name: UpsertExternalIdentity :one
INSERT INTO external_identity (
    workspace_id, provider, external_account_id, external_login,
    display_name, email, avatar_url, user_id
) VALUES (
    $1, $2, $3, sqlc.narg('external_login'),
    sqlc.narg('display_name'), sqlc.narg('email'), sqlc.narg('avatar_url'),
    sqlc.narg('user_id')
)
ON CONFLICT (workspace_id, provider, external_account_id) DO UPDATE SET
    external_login = COALESCE(EXCLUDED.external_login, external_identity.external_login),
    display_name   = COALESCE(EXCLUDED.display_name, external_identity.display_name),
    email          = COALESCE(EXCLUDED.email, external_identity.email),
    avatar_url     = COALESCE(EXCLUDED.avatar_url, external_identity.avatar_url),
    user_id        = COALESCE(external_identity.user_id, EXCLUDED.user_id),
    updated_at     = now()
RETURNING *;

-- name: GetExternalIdentity :one
SELECT * FROM external_identity
WHERE workspace_id = $1 AND provider = $2 AND external_account_id = $3;

-- name: GetExternalIdentityByUser :one
-- Reverse lookup for outbound assignee mapping: which remote account does
-- this workspace member correspond to on the given provider?
SELECT * FROM external_identity
WHERE workspace_id = $1 AND provider = $2 AND user_id = $3
ORDER BY updated_at DESC
LIMIT 1;

-- name: FindWorkspaceUserByEmail :one
-- Resolve a remote account to a workspace member by verified email for
-- assignee mapping. Only members count — agents have no email identity.
SELECT u.id FROM "user" u
JOIN member m ON m.user_id = u.id
WHERE m.workspace_id = $1 AND lower(u.email) = lower(sqlc.arg('email'))
LIMIT 1;

-- =====================
-- Issue Sync Outbox
-- =====================

-- name: EnqueueIssueSyncOutbox :one
INSERT INTO issue_sync_outbox (
    workspace_id, sync_source_id, issue_id, op, payload
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: ClaimIssueSyncOutbox :many
-- Claim a batch of due outbox rows for dispatch. SKIP LOCKED lets multiple
-- replicas drain concurrently without double-delivery; rows stuck 'inflight'
-- longer than the stale threshold are reclaimed (worker crashed mid-push —
-- providers are idempotent enough that a rare double-apply is acceptable).
UPDATE issue_sync_outbox
SET status = 'inflight', attempts = attempts + 1, updated_at = now()
WHERE id IN (
    SELECT id FROM issue_sync_outbox
    WHERE (status = 'pending' AND next_attempt_at <= now())
       OR (status = 'inflight' AND updated_at < now() - interval '5 minutes')
    ORDER BY next_attempt_at ASC
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: CompleteIssueSyncOutbox :exec
UPDATE issue_sync_outbox
SET status = 'done', last_error = NULL, updated_at = now()
WHERE id = $1;

-- name: RetryIssueSyncOutbox :exec
UPDATE issue_sync_outbox
SET status = 'pending',
    next_attempt_at = $2,
    last_error      = $3,
    updated_at      = now()
WHERE id = $1;

-- name: FailIssueSyncOutbox :exec
UPDATE issue_sync_outbox
SET status = 'failed', last_error = $2, updated_at = now()
WHERE id = $1;

-- name: DeleteDoneIssueSyncOutbox :exec
-- Retention sweep: done rows older than the cutoff are noise.
DELETE FROM issue_sync_outbox
WHERE status = 'done' AND updated_at < $1;
