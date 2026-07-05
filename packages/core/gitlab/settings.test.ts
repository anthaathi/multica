import { describe, it, expect } from "vitest";
import { deriveGitLabSettings } from "./settings";
import type { Workspace } from "../types";

function ws(settings: Record<string, unknown>): Pick<Workspace, "settings"> {
  return { settings };
}

describe("deriveGitLabSettings", () => {
  it("defaults every flag to true when workspace is null", () => {
    expect(deriveGitLabSettings(null)).toEqual({
      enabled: true,
      mrSidebar: true,
      autoLinkMRs: true,
    });
  });

  it("defaults every flag to true on empty settings", () => {
    expect(deriveGitLabSettings(ws({}))).toEqual({
      enabled: true,
      mrSidebar: true,
      autoLinkMRs: true,
    });
  });

  it("master switch off forces every dependent flag off", () => {
    const got = deriveGitLabSettings(
      ws({
        gitlab_enabled: false,
        gitlab_mr_sidebar_enabled: true,
        gitlab_auto_link_mrs_enabled: true,
      }),
    );
    expect(got).toEqual({
      enabled: false,
      mrSidebar: false,
      autoLinkMRs: false,
    });
  });

  it("each sub-flag can be flipped independently when master is on", () => {
    expect(
      deriveGitLabSettings(ws({ gitlab_mr_sidebar_enabled: false })),
    ).toMatchObject({ enabled: true, mrSidebar: false, autoLinkMRs: true });

    expect(
      deriveGitLabSettings(ws({ gitlab_auto_link_mrs_enabled: false })),
    ).toMatchObject({ enabled: true, mrSidebar: true, autoLinkMRs: false });
  });

  it("treats non-false values (true, null, missing) as enabled", () => {
    expect(
      deriveGitLabSettings(ws({ gitlab_enabled: true, gitlab_mr_sidebar_enabled: null })),
    ).toMatchObject({ enabled: true, mrSidebar: true });
  });
});
