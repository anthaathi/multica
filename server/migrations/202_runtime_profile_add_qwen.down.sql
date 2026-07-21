-- Restore the pre-202 whitelist (with Grok, without Qwen Code).  NOT VALID
-- keeps rollback compatible with historical rows the prior migrations allowed.
-- Fork (anthaathi/multica): the pre-202 state is post-179, which carries `omp`
-- (migrations 143/175/179); restore it too so rollback does not drop omp.
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
        'omp'
    )) NOT VALID;
