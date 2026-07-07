"use client";

import { memo, useState } from "react";
import {
  Terminal,
  FileText,
  FilePen,
  FilePlus2,
  Search,
  Wrench,
  Brain,
  ChevronRight,
  Check,
  Copy,
  Loader2,
  CircleAlert,
  type LucideIcon,
} from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { copyText } from "@multica/ui/lib/clipboard";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import { MemoizedMarkdown } from "../markdown";
import type { ToolNode } from "./conversation";
import type { TimelineItem } from "./build-timeline";
import { redactSecrets } from "./redact";
import { normalizeOutput, parseReadOutput } from "./parse-output";
import { getInputPath, readToolInput, summarizeToolInput } from "./tool-inputs";

// ─── Static lookup tables ─────────────────────────────────────────────────

/** File extension → shiki language id, for syntax highlighting in cards. */
const LANGUAGE_BY_EXT: Record<string, string> = {
  ts: "typescript", tsx: "tsx", js: "javascript", jsx: "jsx", mjs: "javascript",
  json: "json", go: "go", rs: "rust", py: "python", rb: "ruby", java: "java",
  kt: "kotlin", swift: "swift", dart: "dart", yaml: "yaml", yml: "yaml",
  toml: "toml", md: "markdown", sh: "bash", bash: "bash", sql: "sql",
  html: "html", css: "css", scss: "scss", xml: "xml", php: "php", c: "c",
  h: "c", cpp: "cpp", cc: "cpp", hpp: "cpp", cs: "csharp", vue: "vue",
  svelte: "svelte", graphql: "graphql", dockerfile: "docker",
};

interface ToolMeta {
  Icon: LucideIcon;
  /** Tailwind classes for the icon chip background + foreground. */
  chip: string;
}

const TOOL_META: Record<string, ToolMeta> = {
  bash: { Icon: Terminal, chip: "bg-zinc-900 text-zinc-100 dark:bg-zinc-800" },
  read: { Icon: FileText, chip: "bg-blue-500/15 text-blue-600 dark:text-blue-400" },
  edit: { Icon: FilePen, chip: "bg-amber-500/15 text-amber-600 dark:text-amber-400" },
  write: { Icon: FilePlus2, chip: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400" },
  grep: { Icon: Search, chip: "bg-violet-500/15 text-violet-600 dark:text-violet-400" },
  glob: { Icon: Search, chip: "bg-violet-500/15 text-violet-600 dark:text-violet-400" },
};

const GENERIC_META: ToolMeta = {
  Icon: Wrench,
  chip: "bg-muted text-muted-foreground",
};

function metaFor(tool: string | undefined): ToolMeta {
  return (tool && TOOL_META[tool]) || GENERIC_META;
}
function languageForPath(path: string | undefined): string | undefined {
  if (!path) return undefined;
  const dot = path.lastIndexOf(".");
  if (dot === -1) return /dockerfile/i.test(path) ? "docker" : undefined;
  return LANGUAGE_BY_EXT[path.slice(dot + 1).toLowerCase()];
}

// ─── Card shell: collapsible header + body ────────────────────────────────

interface CardShellProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  Icon: LucideIcon;
  chipClass: string;
  /** Bold label, e.g. the tool name or "bash". */
  label: string;
  /** Muted one-line summary that follows the label. */
  summary?: string;
  running?: boolean;
  time?: string;
  children: React.ReactNode;
}

const CardShell = memo(function CardShell({
  open,
  onOpenChange,
  Icon,
  chipClass,
  label,
  summary,
  running,
  time,
  children,
}: CardShellProps) {
  return (
    <Collapsible open={open} onOpenChange={onOpenChange}>
      <CollapsibleTrigger
        className={cn(
          "group flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left transition-colors",
          "hover:bg-accent/50",
          open && "bg-accent/30",
        )}
      >
        <ChevronRight
          className={cn(
            "h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform duration-150",
            open && "rotate-90",
          )}
        />
        <span
          className={cn(
            "flex h-5 w-5 shrink-0 items-center justify-center rounded-md",
            chipClass,
          )}
        >
          <Icon className="h-3 w-3" />
        </span>
        <span className="shrink-0 text-xs font-semibold text-foreground">{label}</span>
        {summary && (
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">
            {summary}
          </span>
        )}
        {running ? (
          <Loader2 className="h-3 w-3 shrink-0 animate-spin text-muted-foreground" />
        ) : (
          open && (
            <Check className="h-3 w-3 shrink-0 text-muted-foreground/50" />
          )
        )}
        {time && (
          <span className="shrink-0 text-[10px] tabular-nums text-muted-foreground/50">
            {time}
          </span>
        )}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="px-2 pb-1 pt-0.5">{children}</div>
      </CollapsibleContent>
    </Collapsible>
  );
});

// ─── Copy button ──────────────────────────────────────────────────────────

function CopyChip({ getText, label }: { getText: () => string; label: string }) {
  const [copied, setCopied] = useState(false);
  const onClick = () => {
    void copyText(getText()).then((ok) => {
      if (!ok) return;
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    });
  };
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-[10px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
    >
      {copied ? <Check className="h-3 w-3 text-emerald-500" /> : <Copy className="h-3 w-3" />}
    </button>
  );
}

// ─── Per-tool bodies ──────────────────────────────────────────────────────

function TerminalBlock({ command, output }: { command?: string; output: string }) {
  const [expanded, setExpanded] = useState(false);
  const long = output.length > 1200;
  const shown = !long || expanded ? output : output.slice(0, 1200);
  return (
    <div className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-950 dark:bg-black/60">
      <div className="flex items-center justify-between border-b border-zinc-800/80 px-3 py-1.5">
        <span className="flex items-center gap-1.5 font-mono text-[11px] text-zinc-400">
          <span className="h-2 w-2 rounded-full bg-red-500/70" />
          <span className="h-2 w-2 rounded-full bg-amber-500/70" />
          <span className="h-2 w-2 rounded-full bg-emerald-500/70" />
          <span className="ml-1.5">terminal</span>
        </span>
        <CopyChip getText={() => `${command ?? ""}\n${output}`} label="Copy output" />
      </div>
      <div className="px-3 py-2 font-mono text-[11px] leading-relaxed">
        {command && <div className="text-emerald-400">$ {command}</div>}
        {output ? (
          <pre className="mt-1 whitespace-pre-wrap break-words text-zinc-300">{shown}</pre>
        ) : (
          <div className="mt-1 text-zinc-600">— no output —</div>
        )}
        {long && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="mt-1.5 text-[11px] text-sky-400 hover:text-sky-300"
          >
            {expanded ? "Show less" : `Show all (${output.length.toLocaleString()} chars)`}
          </button>
        )}
      </div>
    </div>
  );
}

function CodeBlock({
  code,
  language,
  path,
}: {
  code: string;
  language?: string;
  path?: string;
}) {
  const fence = "```" + (language ?? "") + "\n" + code + "\n```";
  return (
    <div className="overflow-hidden rounded-lg border bg-muted/30">
      {path && (
        <div className="flex items-center gap-1.5 border-b bg-muted/50 px-3 py-1 font-mono text-[11px] text-muted-foreground">
          <FileText className="h-3 w-3" />
          <span className="truncate">{path}</span>
        </div>
      )}
      <div className="max-h-96 overflow-auto px-3 py-2 text-[12px] leading-relaxed [&_pre]:!bg-transparent [&_pre]:!p-0 [&_code]:!bg-transparent [&>*:first-child]:!mt-0 [&>*:last-child]:!mb-0">
        <MemoizedMarkdown>{fence}</MemoizedMarkdown>
      </div>
    </div>
  );
}

/** A two-pane removed/added view for `edit` oldString→newString. */
function EditDiff({
  oldText,
  newText,
  language,
}: {
  oldText: string;
  newText: string;
  language?: string;
}) {
  return (
    <div className="grid gap-1.5 sm:grid-cols-2">
      <DiffPane label="Removed" tone="del" code={oldText} language={language} />
      <DiffPane label="Added" tone="add" code={newText} language={language} />
    </div>
  );
}

function DiffPane({
  label,
  tone,
  code,
  language,
}: {
  label: string;
  tone: "add" | "del";
  code: string;
  language?: string;
}) {
  const fence = "```" + (language ?? "") + "\n" + code + "\n```";
  return (
    <div
      className={cn(
        "overflow-hidden rounded-lg border",
        tone === "del"
          ? "border-red-500/30 bg-red-500/5"
          : "border-emerald-500/30 bg-emerald-500/5",
      )}
    >
      <div
        className={cn(
          "border-b px-3 py-1 text-[10px] font-medium uppercase tracking-wide",
          tone === "del" ? "border-red-500/20 text-red-600 dark:text-red-400" : "border-emerald-500/20 text-emerald-600 dark:text-emerald-400",
        )}
      >
        {label}
      </div>
      <div className="max-h-72 overflow-auto px-3 py-2 text-[12px] [&_pre]:!bg-transparent [&_pre]:!p-0 [&>*:first-child]:!mt-0 [&>*:last-child]:!mb-0">
        <MemoizedMarkdown>{fence}</MemoizedMarkdown>
      </div>
    </div>
  );
}

/** Parse grep-style "Found N matches\n path:\n  Line k: ..." into a list. */
function parseGrepMatches(output: string): { count?: number; lines: string[] } | null {
  const countMatch = output.match(/Found\s+(\d+)\s+matches?/i);
  // Each result line looks like "  Line 63: ..." or "/path/file: ...".
  const lines = output
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.length > 0 && l !== (countMatch?.[0] ?? ""));
  if (!countMatch && lines.length === 0) return null;
  return { count: countMatch ? Number(countMatch[1]) : undefined, lines };
}

// ─── Tool node card (dispatches by tool) ──────────────────────────────────

interface ToolNodeCardProps {
  node: ToolNode;
  expanded: boolean;
  onExpandedChange: (open: boolean) => void;
}

export const ToolNodeCard = memo(function ToolNodeCard({
  node,
  expanded,
  onExpandedChange,
}: ToolNodeCardProps) {
  const tool = node.use?.tool ?? node.result?.tool ?? "";
  const meta = metaFor(tool);
  const fields = readToolInput(node.use?.input);
  const running = !!node.use && !node.result;
  const time = fmtTime(node.result?.created_at ?? node.use?.created_at);
  const summary = summarizeToolInput(node.use?.input);
  const label = tool || "tool";
  const out = node.result?.output ?? "";
  const { text: outText } = normalizeOutput(out);

  let body: React.ReactNode;
  if (tool === "bash") {
    body = <TerminalBlock command={fields.command} output={redactSecrets(outText)} />;
  } else if (tool === "read") {
    const parsed = parseReadOutput(out);
    if (parsed) {
      body = (
        <CodeBlock
          code={parsed.content}
          language={languageForPath(parsed.path ?? getInputPath(fields))}
          path={getInputPath(fields) ?? parsed.path}
        />
      );
    } else {
      body = <TerminalBlock output={redactSecrets(outText)} />;
    }
  } else if (tool === "edit") {
    body = (
      <EditDiff
        oldText={fields.oldString ?? ""}
        newText={fields.newString ?? ""}
        language={languageForPath(getInputPath(fields))}
      />
    );
  } else if (tool === "write") {
    body = (
      <CodeBlock
        code={fields.content ?? redactSecrets(outText)}
        language={languageForPath(getInputPath(fields))}
        path={getInputPath(fields)}
      />
    );
  } else if (tool === "grep" || tool === "glob") {
    const matches = parseGrepMatches(outText);
    body = matches ? (
      <div className="rounded-lg border bg-muted/30 px-3 py-2 font-mono text-[11px]">
        {matches.count !== undefined && (
          <div className="mb-1 text-muted-foreground">
            {matches.count} match{matches.count === 1 ? "" : "es"}
          </div>
        )}
        <div className="max-h-72 space-y-0.5 overflow-auto">
          {matches.lines.map((l, i) => (
            <div key={i} className="truncate text-foreground/80">
              {l}
            </div>
          ))}
        </div>
      </div>
    ) : (
      <TerminalBlock output={redactSecrets(outText)} />
    );
  } else {
    // Generic: pretty input JSON + normalized output.
    const inputJson =
      node.use?.input && Object.keys(node.use.input).length > 0
        ? redactSecrets(JSON.stringify(node.use.input, null, 2))
        : null;
    body = (
      <div className="space-y-1.5">
        {inputJson && (
          <pre className="max-h-60 overflow-auto rounded-lg border bg-muted/30 p-2.5 font-mono text-[11px] text-muted-foreground whitespace-pre-wrap break-all">
            {inputJson}
          </pre>
        )}
        {outText && (
          <pre className="max-h-60 overflow-auto rounded-lg border bg-muted/30 p-2.5 font-mono text-[11px] text-muted-foreground whitespace-pre-wrap break-all">
            {redactSecrets(outText)}
          </pre>
        )}
        {!inputJson && !outText && <div className="px-1 py-2 text-xs text-muted-foreground">—</div>}
      </div>
    );
  }

  return (
    <CardShell
      open={expanded}
      onOpenChange={onExpandedChange}
      Icon={meta.Icon}
      chipClass={meta.chip}
      label={label}
      summary={summary}
      running={running}
      time={time}
    >
      {body}
    </CardShell>
  );
});

// ─── Conversation message renderers (text / thinking / error) ─────────────

function fmtTime(iso: string | undefined): string | undefined {
  if (!iso) return undefined;
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function AssistantMessage({ item }: { item: TimelineItem }) {
  return (
    <div className="px-2 py-2">
      <div className="prose prose-sm dark:prose-invert max-w-none text-sm leading-relaxed [&>*:first-child]:mt-0 [&>*:last-child]:mb-0">
        <MemoizedMarkdown>{item.content ?? ""}</MemoizedMarkdown>
      </div>
    </div>
  );
}

export function ThinkingMessage({ item }: { item: TimelineItem }) {
  const text = item.content ?? "";
  const [open, setOpen] = useState(false);
  if (!text.trim()) return null;
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground">
        <Brain className="h-3.5 w-3.5 text-violet-500/70" />
        <ChevronRight
          className={cn("h-3 w-3 transition-transform", open && "rotate-90")}
        />
        <span className="italic">{open ? "Hide reasoning" : "Reasoning"}</span>
        <span className="text-muted-foreground/50">{text.length.toLocaleString()} chars</span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-1 rounded-lg border border-violet-500/20 bg-violet-500/[0.03] px-3 py-2">
          <div className="prose prose-sm dark:prose-invert max-w-none text-[13px] leading-relaxed text-muted-foreground [&>*:first-child]:mt-0 [&>*:last-child]:mb-0">
            <MemoizedMarkdown>{text}</MemoizedMarkdown>
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

export function ErrorMessage({ item }: { item: TimelineItem }) {
  return (
    <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2">
      <CircleAlert className="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
      <pre className="whitespace-pre-wrap break-words text-xs text-destructive">
        {item.content ?? ""}
      </pre>
    </div>
  );
}
