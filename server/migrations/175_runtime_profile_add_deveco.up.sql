-- Add 'deveco' (DevEco Code, Huawei's coding agent for HarmonyOS, built on the
-- OpenCode engine) to the runtime_profile protocol_family whitelist. Mirrors the
-- drop/re-add NOT VALID pattern from migrations 126/134/136 so historical rows
-- are not revalidated. Kept in lockstep with agent.SupportedTypes and agent.New()
-- (see server/pkg/agent/agent.go).
-- Fork (anthaathi/multica): 'omp' (Oh My Pi, migration 143) is appended here
-- because this migration runs last (prefix 175 > 143) and re-establishes the
-- constraint; without it omp would be dropped from the whitelist on upgrade.
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
        'omp'
    )) NOT VALID;
