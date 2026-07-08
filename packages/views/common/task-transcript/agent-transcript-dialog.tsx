"use client";

import { useState, useRef, useCallback, useEffect, useMemo } from "react";
import {
  Bot,
  CheckCircle2,
  XCircle,
  X,
  Loader2,
  Clock,
  Copy,
  Check,
  Monitor,
  Cloud,
  Cpu,
  Filter,
  Folder,
  ArrowDownNarrowWide,
  ArrowUpNarrowWide,
} from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { copyText } from "@multica/ui/lib/clipboard";
import { Dialog, DialogContent, DialogTitle } from "@multica/ui/components/ui/dialog";
import {
  MessageScrollerProvider,
  MessageScroller,
  MessageScrollerViewport,
  MessageScrollerContent,
  MessageScrollerButton,
} from "@multica/ui/components/ui/message-scroller";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuSeparator,
  DropdownMenuCheckboxItem,
  DropdownMenuItem,
} from "@multica/ui/components/ui/dropdown-menu";
import { ActorAvatar } from "../actor-avatar";
import { api } from "@multica/core/api";
import {
  useTranscriptViewStore,
  type TranscriptFilterKey,
  type TranscriptSortDirection,
} from "@multica/core/agents/stores";
import type { AgentTask, Agent, AgentRuntime } from "@multica/core/types/agent";
import type { TimelineItem } from "./build-timeline";
import {
  buildConversationNodes,
  type ConversationNode,
} from "./conversation";
import {
  AssistantMessage,
  ErrorMessage,
  ThinkingMessage,
  ToolNodeCard,
} from "./tool-cards";
import { summarizeToolInput } from "./tool-inputs";
import { normalizeOutput } from "./parse-output";
import { useT } from "../../i18n";

interface AgentTranscriptDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  task: AgentTask;
  items: TimelineItem[];
  agentName: string;
  isLive?: boolean;
  /**
   * Optional content rendered between the header chips and the event list.
   * Used by autopilot run rows to surface the inbound webhook trigger
   * payload so it's visible regardless of whether the agent echoes it.
   */
  headerSlot?: React.ReactNode;
}

// ─── Node helpers ─────────────────────────────────────────────────────────

type NodeColor = "agent" | "thinking" | "tool" | "error";

function nodeColor(node: ConversationNode): NodeColor {
  switch (node.kind) {
    case "text":
      return "agent";
    case "thinking":
      return "thinking";
    case "tool":
      return "tool";
    case "error":
      return "error";
  }
}

function nodeKey(node: ConversationNode): string {
  return node.kind === "tool" ? `t-${node.orderSeq}` : `${node.kind}-${node.item.seq}`;
}

function nodeFilterKey(node: ConversationNode): TranscriptFilterKey {
  if (node.kind === "tool") {
    const tool = node.use?.tool ?? node.result?.tool ?? "";
    return tool ? `tool:${tool}` : "tool";
  }
  return node.kind;
}

function nodeLabel(node: ConversationNode): string {
  if (node.kind === "tool") return node.use?.tool ?? node.result?.tool ?? "tool";
  if (node.kind === "text") return "Agent";
  if (node.kind === "thinking") return "Reasoning";
  return "Error";
}

function nodeTime(node: ConversationNode): string | undefined {
  const iso =
    node.kind === "tool"
      ? (node.result?.created_at ?? node.use?.created_at)
      : node.item.created_at;
  if (!iso) return undefined;
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

const SEGMENT_COLORS: Record<NodeColor, { bg: string; bgActive: string }> = {
  agent: { bg: "bg-emerald-400/60", bgActive: "bg-emerald-500" },
  thinking: { bg: "bg-violet-400/60", bgActive: "bg-violet-500" },
  tool: { bg: "bg-blue-400/60", bgActive: "bg-blue-500" },
  error: { bg: "bg-red-400/60", bgActive: "bg-red-500" },
};

function formatDuration(start: string, end: string): string {
  const seconds = Math.floor(
    (new Date(end).getTime() - new Date(start).getTime()) / 1000,
  );
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes}m ${seconds % 60}s`;
}

function formatElapsedMs(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes}m ${seconds % 60}s`;
}

// ─── Main dialog ──────────────────────────────────────────────────────────

export function AgentTranscriptDialog({
  open,
  onOpenChange,
  task,
  items,
  agentName,
  isLive = false,
  headerSlot,
}: AgentTranscriptDialogProps) {
  const { t } = useT("agents");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const [elapsed, setElapsed] = useState("");
  const [copied, setCopied] = useState(false);
  const [copiedWorkdir, setCopiedWorkdir] = useState(false);
  const [agentInfo, setAgentInfo] = useState<Agent | null>(null);
  const [runtimeInfo, setRuntimeInfo] = useState<AgentRuntime | null>(null);
  const [sessionFilterKeys, setSessionFilterKeys] = useState<TranscriptFilterKey[]>([]);
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(() => new Set());
  const sortDirection = useTranscriptViewStore((s) => s.sortDirection);
  const setSortDirection = useTranscriptViewStore((s) => s.setSortDirection);
  const preserveFilters = useTranscriptViewStore((s) => s.preserveFilters);
  const setPreserveFilters = useTranscriptViewStore((s) => s.setPreserveFilters);
  const persistedFilterKeys = useTranscriptViewStore((s) => s.selectedFilterKeys);
  const setPersistedFilterKeys = useTranscriptViewStore((s) => s.setSelectedFilterKeys);
  const togglePersistedFilterKey = useTranscriptViewStore((s) => s.toggleFilterKey);
  const clearPersistedFilterKeys = useTranscriptViewStore((s) => s.clearFilterKeys);
  const defaultExpanded = useTranscriptViewStore((s) => s.defaultExpanded);
  const setDefaultExpanded = useTranscriptViewStore((s) => s.setDefaultExpanded);
  const nodeRefs = useRef<Map<string, HTMLDivElement>>(new Map());
  const autoExpandedRef = useRef<Set<string>>(new Set());
  const initializedTaskRef = useRef<string | null>(null);
  const previousDefaultExpandedRef = useRef(defaultExpanded);
  const selectedFilterKeys = preserveFilters ? persistedFilterKeys : sessionFilterKeys;

  // Pair + coalesce the raw timeline into conversation nodes (memoized on the
  // items array identity — mergeTaskMessagesBySeq preserves it for dupes).
  const nodes = useMemo(() => buildConversationNodes(items), [items]);

  const filterOptions = useMemo(() => {
    const options = new Map<string, string>();
    for (const node of nodes) {
      const key = nodeFilterKey(node);
      if (!options.has(key)) options.set(key, nodeLabel(node));
    }
    return Array.from(options.entries()).sort((a, b) => a[1].localeCompare(b[1]));
  }, [nodes]);

  const filterOptionKeys = useMemo(
    () => new Set(filterOptions.map(([value]) => value)),
    [filterOptions],
  );

  const activeFilterKeys = useMemo(
    () => selectedFilterKeys.filter((key) => filterOptionKeys.has(key)),
    [selectedFilterKeys, filterOptionKeys],
  );
  const activeFilterSet = useMemo(() => new Set(activeFilterKeys), [activeFilterKeys]);

  const filteredNodes = useMemo(() => {
    if (activeFilterSet.size === 0) return nodes;
    return nodes.filter((node) => activeFilterSet.has(nodeFilterKey(node)));
  }, [nodes, activeFilterSet]);

  const displayNodes = useMemo(
    () => (sortDirection === "newest_first" ? [...filteredNodes].reverse() : filteredNodes),
    [filteredNodes, sortDirection],
  );

  // Tool-card keys (the only expandable node kind).
  const expandableKeys = useMemo(
    () => displayNodes.filter((n) => n.kind === "tool").map(nodeKey),
    [displayNodes],
  );
  const allExpanded =
    expandableKeys.length > 0 && expandableKeys.every((k) => expandedKeys.has(k));

  // Seed expand state on task switch / defaultExpanded toggle; auto-expand new
  // tool nodes as they stream in when defaultExpanded is on.
  useEffect(() => {
    const switchedOn = defaultExpanded && previousDefaultExpandedRef.current !== defaultExpanded;
    previousDefaultExpandedRef.current = defaultExpanded;

    if (initializedTaskRef.current !== task.id || switchedOn) {
      initializedTaskRef.current = task.id;
      autoExpandedRef.current = new Set(defaultExpanded ? expandableKeys : []);
      setExpandedKeys(defaultExpanded ? new Set(expandableKeys) : new Set());
      return;
    }
    if (!defaultExpanded) return;
    const unseen = expandableKeys.filter((k) => !autoExpandedRef.current.has(k));
    if (unseen.length === 0) return;
    for (const k of unseen) autoExpandedRef.current.add(k);
    setExpandedKeys((prev) => new Set([...prev, ...unseen]));
  }, [task.id, defaultExpanded, expandableKeys]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    if (task.agent_id) {
      api.getAgent(task.agent_id).then((agent) => {
        if (!cancelled) setAgentInfo(agent);
      }).catch(() => {});
    }
    if (task.runtime_id) {
      api.listRuntimes().then((runtimes) => {
        if (cancelled) return;
        const rt = runtimes.find((r) => r.id === task.runtime_id);
        if (rt) setRuntimeInfo(rt);
      }).catch(() => {});
    }
    return () => {
      cancelled = true;
    };
  }, [open, task.agent_id, task.runtime_id]);

  useEffect(() => {
    if (!isLive || (!task.started_at && !task.dispatched_at)) return;
    const startRef = task.started_at ?? task.dispatched_at!;
    const update = () =>
      setElapsed(formatElapsedMs(Date.now() - new Date(startRef).getTime()));
    update();
    const interval = setInterval(update, 1000);
    return () => clearInterval(interval);
  }, [isLive, task.started_at, task.dispatched_at]);

  const handleSortDirectionChange = useCallback(
    (dir: TranscriptSortDirection) => {
      if (dir === sortDirection) return;
      setSortDirection(dir);
      const first = displayNodes[0];
      if (first) nodeRefs.current.get(nodeKey(first))?.scrollIntoView({ behavior: "smooth" });
    },
    [sortDirection, setSortDirection, displayNodes],
  );

  const handleCopyWorkdir = useCallback(() => {
    if (!task.relative_work_dir) return;
    void copyText(task.relative_work_dir).then((ok) => {
      if (!ok) return;
      setCopiedWorkdir(true);
      setTimeout(() => setCopiedWorkdir(false), 2000);
    });
  }, [task.relative_work_dir]);

  const handleCopyAll = useCallback(() => {
    const text = displayNodes
      .map((node) => {
        if (node.kind === "tool") {
          const summary = summarizeToolInput(node.use?.input);
          const out = normalizeOutput(node.result?.output).text;
          const label = nodeLabel(node);
          return out ? `[${label}] ${summary}\n${out}` : `[${label}] ${summary}`;
        }
        return `[${nodeLabel(node)}] ${node.item.content ?? ""}`;
      })
      .join("\n");
    void copyText(text).then((ok) => {
      if (!ok) return;
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    });
  }, [displayNodes]);

  const toggleSessionFilterKey = useCallback((key: TranscriptFilterKey) => {
    setSessionFilterKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return Array.from(next);
    });
  }, []);

  const clearFilters = useCallback(() => {
    if (preserveFilters) {
      clearPersistedFilterKeys();
      return;
    }
    setSessionFilterKeys([]);
  }, [clearPersistedFilterKeys, preserveFilters]);

  const toggleFilterKey = useCallback(
    (key: TranscriptFilterKey) => {
      if (preserveFilters) {
        togglePersistedFilterKey(key);
        return;
      }
      toggleSessionFilterKey(key);
    },
    [preserveFilters, togglePersistedFilterKey, toggleSessionFilterKey],
  );

  const handlePreserveFiltersChange = useCallback(
    (next: boolean) => {
      if (next) setPersistedFilterKeys(sessionFilterKeys);
      else setSessionFilterKeys(persistedFilterKeys);
      setPreserveFilters(next);
    },
    [persistedFilterKeys, sessionFilterKeys, setPersistedFilterKeys, setPreserveFilters],
  );

  const handleToggleVisibleExpanded = useCallback(() => {
    for (const k of expandableKeys) autoExpandedRef.current.add(k);
    setExpandedKeys((prev) => {
      if (allExpanded) {
        const next = new Set(prev);
        for (const k of expandableKeys) next.delete(k);
        return next;
      }
      return new Set([...prev, ...expandableKeys]);
    });
  }, [allExpanded, expandableKeys]);

  const handleNodeExpandedChange = useCallback((key: string, expanded: boolean) => {
    autoExpandedRef.current.add(key);
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (expanded) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);

  const duration =
    task.started_at && task.completed_at
      ? formatDuration(task.started_at, task.completed_at)
      : isLive
        ? elapsed
        : null;

  const toolCount = nodes.filter((n) => n.kind === "tool").length;
  const copyTranscriptLabel = copied
    ? t(($) => $.transcript.copied)
    : activeFilterKeys.length > 0
      ? t(($) => $.transcript.copy_filtered)
      : t(($) => $.transcript.copy_all);

  const statusBadge = isLive ? (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-info/15 px-2 py-0.5 text-xs font-medium text-info">
      <Loader2 className="h-3 w-3 animate-spin" />
      {t(($) => $.transcript.status_running)}
    </span>
  ) : task.status === "completed" ? (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-success/15 px-2 py-0.5 text-xs font-medium text-success">
      <CheckCircle2 className="h-3 w-3" />
      {t(($) => $.transcript.status_completed)}
    </span>
  ) : task.status === "failed" ? (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-destructive/15 px-2 py-0.5 text-xs font-medium text-destructive">
      <XCircle className="h-3 w-3" />
      {t(($) => $.transcript.status_failed)}
    </span>
  ) : (
    <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground capitalize">
      {task.status}
    </span>
  );

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="!max-w-4xl !w-[calc(100vw-4rem)] !max-h-[calc(100vh-4rem)] !h-[calc(100vh-4rem)] flex flex-col !p-0 !gap-0 overflow-hidden"
        showCloseButton={false}
      >
        <DialogTitle className="sr-only">{t(($) => $.transcript.dialog_title)}</DialogTitle>

        {/* ── Header ─────────────────────────────────────────────── */}
        <div className="border-b px-4 py-3 shrink-0 space-y-2">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <div className="flex min-w-0 items-center gap-2">
              {task.agent_id ? (
                <ActorAvatar actorType="agent" actorId={task.agent_id} size={24} />
              ) : (
                <div className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-info/10 text-info">
                  <Bot className="h-3.5 w-3.5" />
                </div>
              )}
              <span className="truncate font-medium text-sm">{agentName}</span>
            </div>

            {statusBadge}

            <div className="flex w-full max-w-full flex-wrap items-center justify-end gap-1 sm:ml-auto sm:w-auto">
              {expandableKeys.length > 0 && (
                <button
                  type="button"
                  onClick={handleToggleVisibleExpanded}
                  aria-label={
                    allExpanded
                      ? t(($) => $.transcript.collapse_visible)
                      : t(($) => $.transcript.expand_visible)
                  }
                  className="flex shrink-0 items-center gap-1 rounded px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                >
                  {allExpanded
                    ? t(($) => $.transcript.collapse_visible)
                    : t(($) => $.transcript.expand_visible)}
                </button>
              )}
              {nodes.length > 1 && (
                <SortDirectionToggle
                  value={sortDirection}
                  onChange={handleSortDirectionChange}
                  labels={{
                    chronological: t(($) => $.transcript.sort_chronological),
                    newestFirst: t(($) => $.transcript.sort_newest_first),
                    ariaLabel: t(($) => $.transcript.sort_label),
                  }}
                />
              )}
              {filterOptions.length > 0 && (
                <DropdownMenu>
                  <DropdownMenuTrigger
                    aria-label={t(($) => $.transcript.filter)}
                    className={cn(
                      "flex shrink-0 items-center gap-1 rounded px-2 py-1 text-xs transition-colors",
                      activeFilterKeys.length > 0
                        ? "text-blue-600 dark:text-blue-400 bg-blue-500/10 hover:bg-blue-500/20"
                        : "text-muted-foreground hover:text-foreground hover:bg-accent",
                    )}
                  >
                    <Filter className="h-3 w-3" />
                    <span className="hidden sm:inline">{t(($) => $.transcript.filter)}</span>
                    {activeFilterKeys.length > 0 && (
                      <span className="ml-0.5 rounded-full bg-blue-500/20 px-1.5 py-0 text-[10px] font-medium">
                        {activeFilterKeys.length}
                      </span>
                    )}
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end" className="w-auto">
                    {filterOptions.map(([value, label]) => (
                      <DropdownMenuCheckboxItem
                        key={value}
                        checked={selectedFilterKeys.includes(value)}
                        onCheckedChange={() => toggleFilterKey(value)}
                      >
                        {label}
                      </DropdownMenuCheckboxItem>
                    ))}
                    <DropdownMenuSeparator />
                    <DropdownMenuCheckboxItem
                      checked={preserveFilters}
                      onCheckedChange={(checked) => handlePreserveFiltersChange(checked === true)}
                    >
                      {t(($) => $.transcript.preserve_filters)}
                    </DropdownMenuCheckboxItem>
                    <DropdownMenuCheckboxItem
                      checked={defaultExpanded}
                      onCheckedChange={(checked) => setDefaultExpanded(checked === true)}
                    >
                      {t(($) => $.transcript.default_expanded)}
                    </DropdownMenuCheckboxItem>
                    {selectedFilterKeys.length > 0 && (
                      <>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem onClick={clearFilters} className="text-muted-foreground">
                          {t(($) => $.transcript.clear_filters)}
                        </DropdownMenuItem>
                      </>
                    )}
                  </DropdownMenuContent>
                </DropdownMenu>
              )}
              <button
                type="button"
                onClick={handleCopyAll}
                aria-label={copyTranscriptLabel}
                className="flex shrink-0 items-center gap-1 rounded px-2 py-1 text-xs text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
              >
                {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
                <span className="hidden sm:inline">{copyTranscriptLabel}</span>
              </button>
              <button
                type="button"
                onClick={() => onOpenChange(false)}
                className="flex shrink-0 items-center justify-center rounded p-1 text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
          </div>

          {/* Metadata chips */}
          <div className="flex items-center gap-2 flex-wrap text-xs">
            {runtimeInfo?.provider && (
              <MetadataChip icon={<Cpu className="h-3 w-3" />}>
                {formatProvider(runtimeInfo.provider)}
              </MetadataChip>
            )}
            {runtimeInfo && (
              <MetadataChip
                icon={
                  runtimeInfo.runtime_mode === "cloud" ? (
                    <Cloud className="h-3 w-3" />
                  ) : (
                    <Monitor className="h-3 w-3" />
                  )
                }
              >
                {runtimeInfo.name}
                <span className="text-muted-foreground/60 ml-0.5">({runtimeInfo.runtime_mode})</span>
              </MetadataChip>
            )}
            {agentInfo?.description && (
              <MetadataChip icon={<Bot className="h-3 w-3" />}>
                {agentInfo.description.length > 40
                  ? agentInfo.description.slice(0, 40) + "..."
                  : agentInfo.description}
              </MetadataChip>
            )}
            {duration && (
              <MetadataChip icon={<Clock className="h-3 w-3" />}>{duration}</MetadataChip>
            )}
            {toolCount > 0 && (
              <MetadataChip>
                {t(($) => $.transcript.tool_calls, { count: toolCount })}
              </MetadataChip>
            )}
            <MetadataChip>
              {activeFilterKeys.length > 0
                ? t(($) => $.transcript.events_filtered, {
                    shown: filteredNodes.length,
                    total: nodes.length,
                  })
                : t(($) => $.transcript.events, { count: nodes.length })}
            </MetadataChip>
            {task.relative_work_dir && (
              <button
                type="button"
                onClick={handleCopyWorkdir}
                title={task.relative_work_dir}
                className="inline-flex max-w-[16rem] items-center gap-1 rounded-md border bg-muted/50 px-2 py-0.5 text-[11px] text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                {copiedWorkdir ? (
                  <Check className="h-3 w-3 shrink-0 text-emerald-500" />
                ) : (
                  <Folder className="h-3 w-3 shrink-0" />
                )}
                <span className="truncate font-mono">{task.relative_work_dir}</span>
              </button>
            )}
            {task.created_at && (
              <MetadataChip>
                {new Date(task.created_at).toLocaleString(undefined, {
                  month: "short",
                  day: "numeric",
                  hour: "2-digit",
                  minute: "2-digit",
                })}
              </MetadataChip>
            )}
          </div>
        </div>

        {/* ── Timeline progress bar ─────────────────────────────── */}
        {displayNodes.length > 0 && (
          <div className="border-b px-4 py-2.5 shrink-0">
            <TimelineBar
              nodes={displayNodes}
              selectedKey={selectedKey}
              onSelect={(key) => {
                setSelectedKey(key);
                nodeRefs.current.get(key)?.scrollIntoView({ behavior: "smooth", block: "center" });
              }}
            />
          </div>
        )}

        {headerSlot && (
          <div className="border-b px-4 py-3 shrink-0 bg-muted/20">{headerSlot}</div>
        )}

        {/* ── Conversation list ─────────────────────────────────── */}
        {displayNodes.length === 0 ? (
          <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
            {isLive ? (
              <div className="flex items-center gap-2">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t(($) => $.transcript.waiting_events)}
              </div>
            ) : (
              t(($) => $.transcript.no_data)
            )}
          </div>
        ) : (
          <MessageScrollerProvider>
            <MessageScroller className="flex-1">
              <MessageScrollerViewport>
                <MessageScrollerContent className="flex flex-col gap-2 p-4">
                  {displayNodes.map((node) => {
                    const key = nodeKey(node);
                    return (
                      <ConversationRow
                        key={key}
                        node={node}
                        expanded={expandedKeys.has(nodeKey(node))}
                        onExpandedChange={(open) => handleNodeExpandedChange(nodeKey(node), open)}
                        rowRef={(el) => {
                          if (el) nodeRefs.current.set(key, el);
                          else nodeRefs.current.delete(key);
                        }}
                      />
                    );
                  })}
                </MessageScrollerContent>
                <MessageScrollerButton />
              </MessageScrollerViewport>
            </MessageScroller>
          </MessageScrollerProvider>
        )}
      </DialogContent>
    </Dialog>
  );
}

// ─── Conversation row ─────────────────────────────────────────────────────

function ConversationRow({
  node,
  expanded,
  onExpandedChange,
  rowRef,
}: {
  node: ConversationNode;
  expanded: boolean;
  onExpandedChange: (open: boolean) => void;
  rowRef?: (el: HTMLDivElement | null) => void;
}) {
  const time = nodeTime(node);
  if (node.kind === "tool") {
    return (
      <div ref={rowRef} data-node-key={nodeKey(node)}>
        <ToolNodeCard node={node} expanded={expanded} onExpandedChange={onExpandedChange} />
      </div>
    );
  }
  if (node.kind === "text") {
    return (
      <div ref={rowRef} data-node-key={nodeKey(node)}>
        <AssistantMessage item={node.item} />
        {time && (
          <div className="mt-1 text-[10px] tabular-nums text-muted-foreground/40">
            {time}
          </div>
        )}
      </div>
    );
  }
  if (node.kind === "thinking") {
    return (
      <div ref={rowRef} data-node-key={nodeKey(node)}>
        <ThinkingMessage item={node.item} />
      </div>
    );
  }
  return (
    <div ref={rowRef} data-node-key={nodeKey(node)}>
      <ErrorMessage item={node.item} />
    </div>
  );
}

// ─── Timeline bar (segment runs by node color) ────────────────────────────

function TimelineBar({
  nodes,
  selectedKey,
  onSelect,
}: {
  nodes: ConversationNode[];
  selectedKey: string | null;
  onSelect: (key: string) => void;
}) {
  const segments: { startIndex: number; count: number; color: NodeColor; keys: string[] }[] = [];
  let current: NodeColor | null = null;
  let start = 0;
  let keys: string[] = [];

  for (let i = 0; i < nodes.length; i++) {
    const color = nodeColor(nodes[i]!);
    const key = nodeKey(nodes[i]!);
    if (color !== current) {
      if (current !== null) {
        segments.push({ startIndex: start, count: i - start, color: current, keys });
      }
      current = color;
      start = i;
      keys = [key];
    } else {
      keys.push(key);
    }
  }
  if (current !== null) {
    segments.push({ startIndex: start, count: nodes.length - start, color: current, keys });
  }

  return (
    <div
      className="flex h-5 gap-0.5 overflow-hidden rounded"
      role="navigation"
      aria-label="Timeline"
    >
      {segments.map((seg) => {
        const c = SEGMENT_COLORS[seg.color];
        const width = (seg.count / nodes.length) * 100;
        const label =
          seg.color === "agent"
            ? "Agent"
            : seg.color === "thinking"
              ? "Reasoning"
              : seg.color === "tool"
                ? "Tool"
                : "Error";
        return (
          <button
            type="button"
            key={seg.startIndex}
            className={cn(
              "relative h-full min-w-[4px] transition-all duration-150 hover:opacity-80",
              seg.keys.some((k) => k === selectedKey) ? c.bgActive : c.bg,
            )}
            style={{ width: `${Math.max(width, 0.5)}%` }}
            onClick={() => onSelect(seg.keys[0]!)}
            title={`${label}${seg.count > 1 ? ` (${seg.count})` : ""}`}
          />
        );
      })}
    </div>
  );
}

// ─── Sort direction toggle ────────────────────────────────────────────────

interface SortDirectionToggleProps {
  value: TranscriptSortDirection;
  onChange: (dir: TranscriptSortDirection) => void;
  labels: { chronological: string; newestFirst: string; ariaLabel: string };
}

function SortDirectionToggle({ value, onChange, labels }: SortDirectionToggleProps) {
  const isChrono = value === "chronological";
  return (
    <div
      role="group"
      aria-label={labels.ariaLabel}
      className="flex shrink-0 items-center rounded border bg-background p-0.5"
    >
      <ToggleBtn
        active={isChrono}
        onClick={() => onChange("chronological")}
        icon={<ArrowUpNarrowWide className="h-3 w-3" />}
        label={labels.chronological}
      />
      <ToggleBtn
        active={!isChrono}
        onClick={() => onChange("newest_first")}
        icon={<ArrowDownNarrowWide className="h-3 w-3" />}
        label={labels.newestFirst}
      />
    </div>
  );
}

function ToggleBtn({
  active,
  onClick,
  icon,
  label,
}: {
  active: boolean;
  onClick: () => void;
  icon: React.ReactNode;
  label: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      title={label}
      className={cn(
        "flex items-center gap-1 rounded px-1.5 py-0.5 text-xs transition-colors",
        active
          ? "bg-accent text-foreground"
          : "text-muted-foreground hover:text-foreground",
      )}
    >
      {icon}
      <span className="hidden lg:inline">{label}</span>
    </button>
  );
}

// ─── Misc ─────────────────────────────────────────────────────────────────

function MetadataChip({
  icon,
  children,
}: {
  icon?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <span className="inline-flex items-center gap-1 rounded-md border bg-muted/50 px-2 py-0.5 text-[11px] text-muted-foreground">
      {icon}
      {children}
    </span>
  );
}

function formatProvider(provider: string): string {
  const map: Record<string, string> = {
    claude: "Claude Code",
    "claude-code": "Claude Code",
  };
  return map[provider] ?? provider;
}
