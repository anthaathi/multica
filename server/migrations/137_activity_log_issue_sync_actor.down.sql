ALTER TABLE activity_log DROP CONSTRAINT IF EXISTS activity_log_actor_type_check;
ALTER TABLE activity_log ADD CONSTRAINT activity_log_actor_type_check
    CHECK (actor_type IN ('member', 'agent', 'system'));
