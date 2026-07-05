"use client";

import { useMemo } from "react";
import { useCurrentWorkspace } from "../paths";
import { deriveGitLabSettings, type GitLabSettings } from "./settings";

/**
 * Reads the GitLab feature flags off the current workspace's settings JSONB.
 * Components should consult this hook rather than poking at `workspace.settings`
 * directly, so the per-flag fallback semantics (see deriveGitLabSettings) stay
 * consistent.
 */
export function useGitLabSettings(): GitLabSettings {
  const workspace = useCurrentWorkspace();
  return useMemo(() => deriveGitLabSettings(workspace), [workspace]);
}
