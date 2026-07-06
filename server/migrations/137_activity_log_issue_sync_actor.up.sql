-- The issue-sync engine writes activity_log rows with actor_type='issue_sync'
-- (ActorTypeSync constant). The original CHECK on activity_log only allowed
-- member/agent/system; widen it so synced-issue activity entries persist
-- instead of silently failing (the issues still create fine — this only
-- affected the activity timeline).
ALTER TABLE activity_log DROP CONSTRAINT IF EXISTS activity_log_actor_type_check;
ALTER TABLE activity_log ADD CONSTRAINT activity_log_actor_type_check
    CHECK (actor_type IN ('member', 'agent', 'system', 'issue_sync'));
