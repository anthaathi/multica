"use client";
import { ExternalLink } from "lucide-react";
import type { IssueSyncLink } from "@multica/core/types";
import { GitHubMark } from "../../settings/components/github-mark";
import { GitLabMark } from "../../settings/components/gitlab-mark";
import { JiraMark } from "../../settings/components/jira-mark";
import { useT } from "../../i18n";

// Renders a compact row of provider badges for an issue's synced external
// links (e.g. a GitHub issue, a Jira ticket). The backend issue response does
// not yet include sync links, so this component is defensive: callers pass an
// optional `syncLinks` prop and it renders nothing when absent or empty.
//
// Place this in the issue detail header area.
export function IssueSyncBadges({
  syncLinks,
}: {
  /** Optional synced external links. Absent on current backends — render
   *  nothing in that case. */
  syncLinks?: IssueSyncLink[];
}) {
  const { t } = useT("issues");
  if (!syncLinks || syncLinks.length === 0) return null;

  return (
    <div className="mt-2 flex flex-wrap items-center gap-1.5">
      {syncLinks.map((link, i) => {
        const Glyph =
          link.provider === "github"
            ? GitHubMark
            : link.provider === "gitlab"
              ? GitLabMark
              : link.provider === "jira"
                ? JiraMark
                : null;
        const content = (
          <>
            {Glyph && <Glyph className="h-3 w-3" />}
            <span className="max-w-[16rem] truncate">{link.external_key}</span>
            {link.web_url && <ExternalLink className="h-3 w-3 opacity-70" />}
          </>
        );
        const cls =
          "inline-flex items-center gap-1 rounded-full border bg-muted/40 px-2 py-0.5 text-xs text-muted-foreground hover:text-foreground transition-colors";
        return link.web_url ? (
          <a
            key={`${link.provider}-${link.external_key}-${i}`}
            href={link.web_url}
            target="_blank"
            rel="noopener noreferrer"
            className={cls}
            title={t(($) => $.sync_badges.open)}
          >
            {content}
          </a>
        ) : (
          <span
            key={`${link.provider}-${link.external_key}-${i}`}
            className={cls}
            title={t(($) => $.sync_badges.label)}
          >
            {content}
          </span>
        );
      })}
    </div>
  );
}
