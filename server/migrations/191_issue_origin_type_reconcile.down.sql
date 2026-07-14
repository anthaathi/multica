-- Best-effort rollback: drop 'agent_create' support, restoring the
-- pre-reconcile constraint (the values established by the fork's
-- 149_issue_origin_mattermost_chat). This matches the schema state before
-- this migration ran on a fresh database.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'issue_sync', 'mattermost_chat'));
