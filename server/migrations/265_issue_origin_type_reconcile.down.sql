-- Best-effort rollback: undo the fork's union reconcile and restore the
-- constraint to the state the immediately-preceding upstream migrations
-- (263_issue_origin_wecom_chat + 264_..._validate) left — i.e. upstream's
-- widened list. This drops the fork-only 'issue_sync'/'mattermost_chat'
-- values that this reconcile re-added, matching the schema as if the fork
-- reconcile never ran.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat'));
