-- Extend issue.origin_type so the Mattermost `/issue` message-prefix command
-- can stamp issues with origin_type='mattermost_chat'. Mirrors 131
-- (slack_chat) and 111 (lark_chat), which added the same origin label for
-- their respective channel adapters.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'mattermost_chat'));
