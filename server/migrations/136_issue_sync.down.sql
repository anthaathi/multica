DROP TABLE IF EXISTS issue_sync_outbox;
DROP TABLE IF EXISTS external_identity;
DROP TABLE IF EXISTS external_comment_link;
DROP TABLE IF EXISTS external_issue_link;
DROP TABLE IF EXISTS issue_sync_source;
DROP TABLE IF EXISTS jira_connection;

-- Revert to the pre-issue_sync issue_origin_type_check list. Any existing rows
-- with origin_type='issue_sync' would violate the rolled-back constraint; the
-- down migration assumes the operator has already deleted or relabeled those
-- rows. Kept strict to preserve the schema invariant downstream code relies
-- on. Mirrors 111/131.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat'));
