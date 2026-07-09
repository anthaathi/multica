-- Revert to the pre-mattermost_chat issue_origin_type_check list. Any existing
-- rows with origin_type='mattermost_chat' would violate the rolled-back
-- constraint; the down migration assumes the operator has already deleted or
-- relabeled those rows. Mirrors 131.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat'));
