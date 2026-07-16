ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

-- Widen the whitelist to include Grok Build CLI (`grok`), driven over ACP via
-- `grok agent --always-approve stdio` (#2895). Builds on migration 175's shape
-- (which added `deveco`); this fork also carries `omp` (143/175), so all three stay. NOT VALID mirrors
-- migrations 126/134/136/175 so a historical Gemini row they intentionally
-- tolerated does not block the upgrade.
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
        'omp',
        'grok'
    )) NOT VALID;
