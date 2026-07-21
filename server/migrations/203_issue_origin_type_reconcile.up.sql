-- Reconcile issue.origin_type CHECK constraint to the full union of values
-- across upstream and the fork. Renumbered 157 -> 161 -> 163 -> 164 -> 175
-- -> 191 -> 197 -> 202 -> 203 as upstream claimed prefix 157
-- (157_agent_task_delivered_comments), then 161 (161_agent_skill_enabled), then
-- 163 (163_agent_builder), then 164 (164_attachment_task_id, PR #5307), then
-- 175 (175_runtime_profile_add_deveco), then 191 (191_issue_properties,
-- MUL-4463), then 202 (202_runtime_profile_add_qwen); runs last so the union
-- survives regardless of which same-prefix 149 migration ran.
--
-- Two prefix-149 migrations each redefined issue_origin_type_check with a
-- hardcoded list, and they run in sorted-filename order:
--   149_issue_origin_agent_create   (upstream) -> adds 'agent_create'
--   149_issue_origin_mattermost_chat (fork)     -> re-adds the constraint with
--       the fork's own values ('issue_sync', 'mattermost_chat') but WITHOUT
--       'agent_create' (it predated the upstream change), so the fork's 149
--       ran second and dropped 'agent_create'.
-- The result rejected origin_type='agent_create', breaking agent-created
-- issues (MUL-4305). This forward-only migration restores the complete value
-- set so agent-created issues are accepted alongside the fork's issue_sync /
-- mattermost_chat origins. Idempotent: re-establishes the constraint with the
-- union regardless of which 149 ran last, so it is correct on both fresh and
-- already-migrated databases.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'issue_sync', 'mattermost_chat'));
