-- Add Qwen Code (`qwen`) to the built-in runtime profile protocol whitelist.
-- Kept in lockstep with agent.SupportedTypes and agent.New().  NOT VALID
-- preserves the historical-row tolerance used by the prior family additions.
-- Fork (anthaathi/multica): `omp` (Oh My Pi, migration 143) is appended here
-- because this migration re-establishes the constraint (prefix 202 > 143);
-- without it omp would be dropped from the whitelist on upgrade. Same carry-over
-- applied to migrations 175 (deveco) and 179 (grok).
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'kiro',
        'antigravity',
        'qoder',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'omp'
    )) NOT VALID;
