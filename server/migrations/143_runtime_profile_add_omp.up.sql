ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

-- Widen the whitelist to include Oh My Pi (`omp`). omp is a Pi fork that keeps
-- Pi's JSON event protocol but diverges on session identity, resume, thinking,
-- and model discovery, so it has a dedicated backend (server/pkg/agent/omp.go)
-- rather than reusing pi's. Shipping the backend without this whitelist entry
-- would reject omp-based custom runtime profiles in the family picker — the
-- same gap traecli hit at launch (#4945). NOT VALID mirrors migrations 126/134/
-- 136 so a historical Gemini row they intentionally tolerated does not block
-- the upgrade.
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
        'omp'
    )) NOT VALID;
