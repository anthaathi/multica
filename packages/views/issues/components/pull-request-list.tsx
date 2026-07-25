"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  Circle,
  CircleDashed,
  CircleSlash,
  GitMerge,
  GitPullRequest,
  GitPullRequestArrow,
  GitPullRequestClosed,
  GitPullRequestDraft,
  TriangleAlert,
  XCircle,
} from "lucide-react";
import {
  issuePullRequestsOptions,
  deriveChecksStatus,
  deriveMergeStatus,
  shouldShowPullRequestStats,
  type PullRequestChecksStatus,
  type PullRequestMergeStatus,
} from "@multica/core/github";
import type { GitHubPullRequest, GitHubPullRequestState, PullRequestProvider } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT, useTimeAgo } from "../../i18n";

type IssuesT = ReturnType<typeof useT<"issues">>["t"];

// Keep the existing sidebar density: show the first 3 PR rows inline, then
// collapse the rest once the section reaches 4 rows.
const PR_LIMIT_BEFORE_COLLAPSE = 4;

const STATE_ICON: Record<
  GitHubPullRequestState,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  open: { icon: GitPullRequestArrow, className: "text-emerald-600 dark:text-emerald-400" },
  draft: { icon: GitPullRequestDraft, className: "text-muted-foreground" },
  merged: { icon: GitMerge, className: "text-violet-600 dark:text-violet-400" },
  closed: { icon: GitPullRequestClosed, className: "text-rose-600 dark:text-rose-400" },
};

export function PullRequestList({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const [expanded, setExpanded] = useState(false);
  const { data, isLoading } = useQuery(issuePullRequestsOptions(issueId));
  const prs = data?.pull_requests ?? [];

  if (isLoading) {
    return <p className="text-xs text-muted-foreground px-2">{t(($) => $.detail.pull_requests_loading)}</p>;
  }
  if (prs.length === 0) {
    return (
      <p className="text-xs text-muted-foreground px-2">
        {t(($) => $.detail.pull_requests_empty)}
      </p>
    );
  }

  // Render rule:
  //   - <  PR_LIMIT_BEFORE_COLLAPSE: every PR row is visible.
  //   - >= PR_LIMIT_BEFORE_COLLAPSE: first (LIMIT - 1) rows are visible and
  //     the remainder sits behind a toggle.
  const useCollapse = prs.length >= PR_LIMIT_BEFORE_COLLAPSE;
  const expandedHead = useCollapse ? prs.slice(0, PR_LIMIT_BEFORE_COLLAPSE - 1) : prs;
  const collapsedTail = useCollapse ? prs.slice(PR_LIMIT_BEFORE_COLLAPSE - 1) : [];

  return (
    <div className="space-y-1">
      {expandedHead.map((pr) => (
        <PullRequestRow key={pr.id} pr={pr} />
      ))}
      {useCollapse ? (
        <div className="space-y-1">
          {expanded
            ? collapsedTail.map((pr) => <PullRequestRow key={pr.id} pr={pr} />)
            : null}
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="block w-[calc(100%+1rem)] -mx-2 rounded-md px-2 py-1.5 text-left text-[11px] text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
          >
            {expanded
              ? t(($) => $.detail.pull_request_card_show_less)
              : t(($) => $.detail.pull_request_card_show_more, { count: collapsedTail.length })}
          </button>
        </div>
      ) : null}
    </div>
  );
}

function PullRequestRow({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[pr.state] ?? { icon: GitPullRequest, className: "" };
  const StateIcon = cfg.icon;
  const isDraft = pr.state === "draft";
  const stateLabel = getStateLabel(pr.state, t);

  return (
    <a
      data-testid="pull-request-row"
      href={pr.html_url}
      target="_blank"
      rel="noreferrer noopener"
      className={cn(
        "flex items-start gap-2 rounded-md px-2 py-1.5 -mx-2 hover:bg-accent/50 transition-colors group",
        isDraft ? "opacity-80" : null,
      )}
    >
      <StateIcon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium leading-snug truncate group-hover:text-foreground">
          {pr.title}
        </p>
        <p className="text-[11px] text-muted-foreground truncate flex items-center gap-1">
          <ProviderGlyph provider={pr.provider} />
          <span className="truncate">
            {pr.repo_owner}/{pr.repo_name}#{pr.number} · {stateLabel}
            {pr.author_login ? ` · @${pr.author_login}` : null}
          </span>
        </p>
        <PullRequestRowDetails pr={pr} />
      </div>
    </a>
  );
}

// ProviderGlyph renders the forge mark (GitHub octocat / GitLab tanuki) so a
// mixed list makes each row's origin obvious. Defaults to GitHub for rows from
// older backends that predate the `provider` field.
function ProviderGlyph({ provider }: { provider?: PullRequestProvider }) {
  if (provider === "gitlab") {
    return (
      <svg viewBox="0 0 24 24" aria-hidden="true" className="h-3 w-3 shrink-0" fill="currentColor">
        <path d="M23.955 13.587l-1.342-4.135-2.664-8.189a.455.455 0 0 0-.867 0L16.418 9.45H7.582L4.919 1.263a.455.455 0 0 0-.867 0L1.386 9.452.044 13.587a.924.924 0 0 0 .331 1.023L12 23.054l11.625-8.443a.92.92 0 0 0 .33-1.024" />
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="h-3 w-3 shrink-0" fill="currentColor">
      <path d="M12 .5C5.6.5.5 5.6.5 12c0 5.1 3.3 9.4 7.9 10.9.6.1.8-.2.8-.6v-2.2c-3.2.7-3.9-1.5-3.9-1.5-.5-1.3-1.3-1.7-1.3-1.7-1.1-.7.1-.7.1-.7 1.2.1 1.8 1.2 1.8 1.2 1 1.8 2.7 1.3 3.4 1 .1-.8.4-1.3.8-1.6-2.6-.3-5.3-1.3-5.3-5.7 0-1.3.5-2.3 1.2-3.1-.1-.3-.5-1.5.1-3.1 0 0 1-.3 3.3 1.2.9-.3 1.9-.4 2.9-.4s2 .1 2.9.4c2.3-1.5 3.3-1.2 3.3-1.2.6 1.6.2 2.8.1 3.1.7.8 1.2 1.8 1.2 3.1 0 4.4-2.7 5.4-5.3 5.7.4.4.8 1.1.8 2.2v3.3c0 .3.2.7.8.6 4.6-1.5 7.9-5.8 7.9-10.9C23.5 5.6 18.4.5 12 .5z" />
    </svg>
  );
}

function PullRequestRowDetails({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  const timeAgo = useTimeAgo();

  const showStats = shouldShowPullRequestStats({
    additions: pr.additions,
    deletions: pr.deletions,
    changed_files: pr.changed_files,
  });

  // Neither status element is shown for terminal PRs — the leading state icon
  // already conveys merged / closed, and CI / mergeability are no longer
  // actionable there.
  const isTerminal = pr.state === "merged" || pr.state === "closed";
  const checksBadge = isTerminal ? null : getChecksBadge(deriveChecksStatus(pr), t);
  const mergeBadge = isTerminal ? null : getMergeBadge(deriveMergeStatus(pr), t);

  // A stale snapshot (GitHub outage / revoked key) greys out both elements and
  // annotates them with the snapshot age instead of hiding the last-known data.
  const stale = !isTerminal && pr.snapshot_stale === true;
  const staleTitle = stale
    ? pr.snapshot_fetched_at
      ? t(($) => $.detail.pull_request_snapshot_stale, { time: timeAgo(pr.snapshot_fetched_at) })
      : t(($) => $.detail.pull_request_snapshot_stale_unknown)
    : undefined;

  if (!showStats && !checksBadge && !mergeBadge) return null;

  return (
    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
      {showStats ? <PullRequestStats pr={pr} /> : null}
      {checksBadge ? <PullRequestBadge badge={checksBadge} stale={stale} title={staleTitle} /> : null}
      {mergeBadge ? <PullRequestBadge badge={mergeBadge} stale={stale} title={staleTitle} /> : null}
    </div>
  );
}

function PullRequestStats({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  return (
    <span className="inline-flex items-center gap-1.5 tabular-nums">
      <span className="text-emerald-600 dark:text-emerald-400">+{pr.additions ?? 0}</span>
      <span className="text-rose-600 dark:text-rose-400">−{pr.deletions ?? 0}</span>
      <span aria-hidden="true">·</span>
      <span>
        {t(($) => $.detail.pull_request_card_files_count, {
          count: pr.changed_files ?? 0,
        })}
      </span>
    </span>
  );
}

interface PullRequestBadgeConfig {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  className: string;
}

function PullRequestBadge({
  badge,
  stale,
  title,
}: {
  badge: PullRequestBadgeConfig;
  stale?: boolean;
  title?: string;
}) {
  const Icon = badge.icon;
  return (
    <span
      className={cn("inline-flex items-center gap-1", stale ? "opacity-60" : null)}
      title={title}
    >
      <Icon className={cn("h-3 w-3", badge.className)} />
      {badge.label}
    </span>
  );
}

// CI element. A current snapshot with a null rollup renders "no checks yet";
// an unavailable/disabled snapshot renders nothing.
function getChecksBadge(
  status: PullRequestChecksStatus,
  t: IssuesT,
): PullRequestBadgeConfig | null {
  switch (status.kind) {
    case "failed":
      return {
        icon: XCircle,
        className: "text-rose-600 dark:text-rose-400",
        label: checksFailedLabel(status, t),
      };
    case "pending":
      return {
        icon: CircleDashed,
        className: "text-amber-600 dark:text-amber-400",
        label: t(($) => $.detail.pull_request_checks_running, {
          passed: status.passed,
          total: status.total,
          running: status.running,
        }),
      };
    case "passed":
      return {
        icon: CheckCircle2,
        className: "text-emerald-600 dark:text-emerald-400",
        label: t(($) => $.detail.pull_request_checks_all_passed, { total: status.total }),
      };
    case "none":
      return {
        icon: Circle,
        className: "text-muted-foreground",
        label: t(($) => $.detail.pull_request_checks_none),
      };
    case "unavailable":
      return null;
  }
}

function checksFailedLabel(
  status: Extract<PullRequestChecksStatus, { kind: "failed" }>,
  t: IssuesT,
): string {
  const shown = status.names.slice(0, 2);
  if (shown.length === 0) {
    return t(($) => $.detail.pull_request_checks_failed_count, {
      failed: status.failed,
      total: status.total,
    });
  }
  const remaining = status.names.length - shown.length;
  const parts = [...shown];
  if (remaining > 0) {
    parts.push(t(($) => $.detail.pull_request_checks_more, { count: remaining }));
  }
  return t(($) => $.detail.pull_request_checks_failed_named, {
    failed: status.failed,
    total: status.total,
    names: parts.join(", "),
  });
}

// Mergeability element. Returns null for the "none" state — when GitHub has not
// decided, the card asserts neither "conflict" nor "ready".
function getMergeBadge(status: PullRequestMergeStatus, t: IssuesT): PullRequestBadgeConfig | null {
  switch (status.kind) {
    case "conflicting":
      return {
        icon: TriangleAlert,
        className: "text-amber-600 dark:text-amber-400",
        label: t(($) => $.detail.pull_request_merge_conflicting),
      };
    case "ready":
      return {
        icon: CheckCircle2,
        className: "text-emerald-600 dark:text-emerald-400",
        label: t(($) => $.detail.pull_request_merge_ready),
      };
    case "blocked":
      return {
        icon: CircleSlash,
        className: "text-muted-foreground",
        label: t(($) => $.detail.pull_request_merge_blocked),
      };
    case "behind":
      return {
        icon: CircleSlash,
        className: "text-muted-foreground",
        label: t(($) => $.detail.pull_request_merge_behind),
      };
    case "unstable":
      return {
        icon: CircleSlash,
        className: "text-muted-foreground",
        label: t(($) => $.detail.pull_request_merge_unstable),
      };
    case "has_hooks":
      return {
        icon: CircleSlash,
        className: "text-muted-foreground",
        label: t(($) => $.detail.pull_request_merge_has_hooks),
      };
    case "none":
      return null;
  }
}

function getStateLabel(state: GitHubPullRequestState, t: IssuesT): string {
  return state === "open"
    ? t(($) => $.detail.pull_request_state_open)
    : state === "draft"
      ? t(($) => $.detail.pull_request_state_draft)
      : state === "merged"
        ? t(($) => $.detail.pull_request_state_merged)
        : state === "closed"
          ? t(($) => $.detail.pull_request_state_closed)
          : state;
}
