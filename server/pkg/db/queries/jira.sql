-- =====================
-- Jira Connection
-- =====================

-- name: CreateJiraConnection :one
INSERT INTO jira_connection (
    workspace_id, cloud_id, site_url, account_id, account_email,
    account_avatar_url, access_token_encrypted, refresh_token_encrypted,
    token_expires_at, webhook_secret_encrypted, connected_by_id
) VALUES (
    $1, $2, $3, $4, sqlc.narg('account_email'),
    sqlc.narg('account_avatar_url'), $5, $6,
    sqlc.narg('token_expires_at'), $7, sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, cloud_id) DO UPDATE SET
    site_url                 = EXCLUDED.site_url,
    account_id               = EXCLUDED.account_id,
    account_email            = EXCLUDED.account_email,
    account_avatar_url       = EXCLUDED.account_avatar_url,
    access_token_encrypted   = EXCLUDED.access_token_encrypted,
    refresh_token_encrypted  = EXCLUDED.refresh_token_encrypted,
    token_expires_at         = EXCLUDED.token_expires_at,
    webhook_secret_encrypted = EXCLUDED.webhook_secret_encrypted,
    connected_by_id          = EXCLUDED.connected_by_id,
    updated_at               = now()
RETURNING *;

-- name: ListJiraConnectionsByWorkspace :many
SELECT * FROM jira_connection
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: GetJiraConnectionByID :one
SELECT * FROM jira_connection
WHERE id = $1;

-- name: DeleteJiraConnection :exec
DELETE FROM jira_connection WHERE id = $1 AND workspace_id = $2;

-- name: UpdateJiraConnectionTokens :exec
-- Jira Cloud rotates refresh tokens: every refresh returns a replacement and
-- invalidates the old one, so both tokens are always written together.
UPDATE jira_connection
SET access_token_encrypted  = $2,
    refresh_token_encrypted = $3,
    token_expires_at        = $4,
    updated_at              = now()
WHERE id = $1;
