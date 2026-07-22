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
  Copy,
  Check,
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
import { Marker, MarkerIcon, MarkerContent } from "@multica/ui/components/ui/marker";
import { Bubble, BubbleContent } from "@multica/ui/components/ui/bubble";
import { MemoizedMarkdown } from "@multica/ui/markdown";
import type { ToolNode } from "./conversation";
import type { TimelineItem } from "./build-timeline";
import { redactSecrets } from "./redact";
import { normalizeOutput, parseReadOutput } from "./parse-output";
import { getInputPath, readToolInput } from "./tool-inputs";

// ─── Static lookup tables ─────────────────────────────────────────────────

const TOOL_ICON: Record<string, LucideIcon> = {
  bash: Terminal,
  read: FileText,
  edit: FilePen,
  write: FilePlus2,
  grep: Search,
  glob: Search,
};

const GENERIC_ICON: LucideIcon = Wrench;

/** Minimal collapsed label per tool: "Ran `cmd`" / "Read `file`" / … */
function collapsedLabel(
  tool: string,
  fields: ReturnType<typeof readToolInput>,
): { verb: string; code: string } {
  const fileName = (getInputPath(fields) ?? "").split("/").pop() ?? "";
  switch (tool) {
    case "bash":
      return { verb: "Ran", code: fields.command ?? "" };
    case "read":
      return { verb: "Read", code: fileName };
    case "edit":
      return { verb: "Edited", code: fileName };
    case "write":
      return { verb: "Wrote", code: fileName };
    case "grep":
    case "glob":
      return { verb: "Searched", code: fields.pattern ?? fields.query ?? "" };
    default: {
      // Generic tools: surface the first useful input field.
      const v =
        fields.command ??
        fields.query ??
        fields.pattern ??
        fields.description ??
        fileName ??
        "";
      return { verb: tool || "tool", code: v };
    }
  }
}

// ─── Copy button ──────────────────────────────────────────────────────────

function CopyChip({ getText, label }: { getText: () => string; label: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      onClick={() => {
        void copyText(getText()).then((ok) => {
          if (!ok) return;
          setCopied(true);
          setTimeout(() => setCopied(false), 1600);
        });
      }}
      aria-label={label}
      title={label}
      className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-[10px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
    >
      {copied ? <Check className="size-3 text-emerald-500" /> : <Copy className="size-3" />}
    </button>
  );
}

// ─── Tool bodies (shown on expand) ────────────────────────────────────────

function TerminalBlock({ command, output }: { command?: string; output: string }) {
  const [expanded, setExpanded] = useState(false);
  const long = output.length > 1200;
  const shown = !long || expanded ? output : output.slice(0, 1200);
  return (
    <div className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-950 dark:bg-black/60">
      <div className="flex items-center justify-between border-b border-zinc-800/80 px-3 py-1.5">
        {/* eslint-disable-next-line i18next/no-literal-string */}
        <span className="font-mono text-[11px] text-zinc-400">terminal</span>
        <CopyChip getText={() => `${command ?? ""}\n${output}`} label="Copy output" />
      </div>
      <div className="px-3 py-2 font-mono text-[11px] leading-relaxed">
        {command && <div className="text-emerald-400">$ {command}</div>}
        {output ? (
          <pre className="mt-1 whitespace-pre-wrap break-words text-zinc-300">{shown}</pre>
        ) : (
          // eslint-disable-next-line i18next/no-literal-string
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

function CodeBlock({ code, language, path }: { code: string; language?: string; path?: string }) {
  const fence = "```" + (language ?? "") + "\n" + code + "\n```";
  return (
    <div className="overflow-hidden rounded-lg border bg-muted/30">
      {path && (
        <div className="flex items-center gap-1.5 border-b bg-muted/50 px-3 py-1 font-mono text-[11px] text-muted-foreground">
          <FileText className="size-3" />
          <span className="truncate">{path}</span>
        </div>
      )}
      <div className="max-h-96 overflow-auto px-3 py-2 text-[12px] leading-relaxed [&_pre]:!bg-transparent [&_pre]:!p-0 [&_code]:!bg-transparent [&>*:first-child]:!mt-0 [&>*:last-child]:!mb-0">
        <MemoizedMarkdown>{fence}</MemoizedMarkdown>
      </div>
    </div>
  );
}

function EditDiff({ oldText, newText, language }: { oldText: string; newText: string; language?: string }) {
  return (
    <div className="grid gap-1.5 sm:grid-cols-2">
      <DiffPane label="Removed" tone="del" code={oldText} language={language} />
      <DiffPane label="Added" tone="add" code={newText} language={language} />
    </div>
  );
}

function DiffPane({ label, tone, code, language }: { label: string; tone: "add" | "del"; code: string; language?: string }) {
  const fence = "```" + (language ?? "") + "\n" + code + "\n```";
  return (
    <div className={cn("overflow-hidden rounded-lg border", tone === "del" ? "border-red-500/30 bg-red-500/5" : "border-emerald-500/30 bg-emerald-500/5")}>
      <div className={cn("border-b px-3 py-1 text-[10px] font-medium uppercase tracking-wide", tone === "del" ? "border-red-500/20 text-red-600 dark:text-red-400" : "border-emerald-500/20 text-emerald-600 dark:text-emerald-400")}>
        {label}
      </div>
      <div className="max-h-72 overflow-auto px-3 py-2 text-[12px] [&_pre]:!bg-transparent [&_pre]:!p-0 [&>*:first-child]:!mt-0 [&>*:last-child]:!mb-0">
        <MemoizedMarkdown>{fence}</MemoizedMarkdown>
      </div>
    </div>
  );
}

function parseGrepMatches(output: string): { count?: number; lines: string[] } | null {
  const countMatch = output.match(/Found\s+(\d+)\s+matches?/i);
  const lines = output
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.length > 0 && l !== (countMatch?.[0] ?? ""));
  if (!countMatch && lines.length === 0) return null;
  return { count: countMatch ? Number(countMatch[1]) : undefined, lines };
}

function languageForPath(path: string | undefined): string | undefined {
  if (!path) return undefined;
  const dot = path.lastIndexOf(".");
  if (dot === -1) return /dockerfile/i.test(path) ? "docker" : undefined;
  return LANGUAGE_BY_EXT[path.slice(dot + 1).toLowerCase()];
}

const LANGUAGE_BY_EXT: Record<string, string> = {
  ts: "typescript", tsx: "tsx", js: "javascript", jsx: "jsx", mjs: "javascript",
  json: "json", go: "go", rs: "rust", py: "python", rb: "ruby", java: "java",
  kt: "kotlin", swift: "swift", dart: "dart", yaml: "yaml", yml: "yaml",
  toml: "toml", md: "markdown", sh: "bash", bash: "bash", sql: "sql",
  html: "html", css: "css", scss: "scss", xml: "xml", php: "php", c: "c",
  h: "c", cpp: "cpp", cc: "cpp", hpp: "cpp", cs: "csharp", vue: "vue",
  svelte: "svelte", graphql: "graphql", dockerfile: "docker",
};

// ─── Tool node card: minimal Marker that expands ──────────────────────────

interface ToolNodeCardProps {
  node: ToolNode;
  expanded: boolean;
  onExpandedChange: (open: boolean) => void;
}

export const ToolNodeCard = memo(function ToolNodeCard({ node, expanded, onExpandedChange }: ToolNodeCardProps) {
  const tool = node.use?.tool ?? node.result?.tool ?? "";
  const Icon = TOOL_ICON[tool] ?? GENERIC_ICON;
  const fields = readToolInput(node.use?.input);
  const running = !!node.use && !node.result;
  const { verb, code } = collapsedLabel(tool, fields);

  const out = node.result?.output ?? "";
  const { text: outText } = normalizeOutput(out);
  const path = getInputPath(fields);

  let body: React.ReactNode = null;
  if (tool === "bash") {
    body = <TerminalBlock command={fields.command} output={redactSecrets(outText)} />;
  } else if (tool === "read") {
    const parsed = parseReadOutput(out);
    body = parsed ? (
      <CodeBlock code={parsed.content} language={languageForPath(parsed.path ?? path)} path={path ?? parsed.path} />
    ) : (
      <TerminalBlock output={redactSecrets(outText)} />
    );
  } else if (tool === "edit") {
    body = <EditDiff oldText={fields.oldString ?? ""} newText={fields.newString ?? ""} language={languageForPath(path)} />;
  } else if (tool === "write") {
    body = <CodeBlock code={fields.content ?? redactSecrets(outText)} language={languageForPath(path)} path={path} />;
  } else if (tool === "grep" || tool === "glob") {
    const matches = parseGrepMatches(outText);
    body = matches ? (
      <div className="rounded-lg border bg-muted/30 px-3 py-2 font-mono text-[11px]">
        {matches.count !== undefined && (
          <div className="mb-1 text-muted-foreground">
            {/* eslint-disable-next-line i18next/no-literal-string */}
            {matches.count} match{matches.count === 1 ? "" : "es"}
          </div>
        )}
        <div className="max-h-72 space-y-0.5 overflow-auto">
          {matches.lines.map((l, i) => (
            <div key={i} className="truncate text-foreground/80">{l}</div>
          ))}
        </div>
      </div>
    ) : (
      <TerminalBlock output={redactSecrets(outText)} />
    );
  } else {
    const inputJson =
      node.use?.input && Object.keys(node.use.input).length > 0
        ? redactSecrets(JSON.stringify(node.use.input, null, 2))
        : null;
    body = (
      <div className="space-y-1.5">
        {inputJson && (
          <pre className="max-h-60 overflow-auto rounded-lg border bg-muted/30 p-2.5 font-mono text-[11px] text-muted-foreground whitespace-pre-wrap break-all">{inputJson}</pre>
        )}
        {outText && (
          <pre className="max-h-60 overflow-auto rounded-lg border bg-muted/30 p-2.5 font-mono text-[11px] text-muted-foreground whitespace-pre-wrap break-all">{redactSecrets(outText)}</pre>
        )}
      </div>
    );
  }

  return (
    <Collapsible open={expanded} onOpenChange={onExpandedChange}>
      <CollapsibleTrigger
        render={
          <Marker
            role={undefined}
            className="w-full rounded px-1 -mx-1 py-1 transition-colors hover:bg-accent/40 data-[state=open]:bg-accent/30"
          />
        }
      >
        <MarkerIcon>
          {running ? <Loader2 className="size-3.5 animate-spin" /> : <Icon className="size-3.5" />}
        </MarkerIcon>
        <MarkerContent className="flex min-w-0 items-center gap-1.5 text-xs">
          <ChevronRight className={cn("size-3 shrink-0 text-muted-foreground/60 transition-transform", expanded && "rotate-90")} />
          <span className="shrink-0 font-medium text-foreground">{verb}</span>
          {code && (
            <code className="min-w-0 truncate rounded bg-muted px-1 py-0.5 font-mono text-[11px] text-muted-foreground">{code}</code>
          )}
        </MarkerContent>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="py-1.5 pl-5">{body}</div>
      </CollapsibleContent>
    </Collapsible>
  );
});

// ─── Conversation message renderers ───────────────────────────────────────

export function AssistantMessage({ item }: { item: TimelineItem }) {
  return (
    <Bubble variant="ghost">
      <BubbleContent>
        <div className="prose prose-sm dark:prose-invert max-w-none text-sm leading-relaxed [&>*:first-child]:mt-0 [&>*:last-child]:mb-0">
          <MemoizedMarkdown>{item.content ?? ""}</MemoizedMarkdown>
        </div>
      </BubbleContent>
    </Bubble>
  );
}

export function ThinkingMessage({ item }: { item: TimelineItem }) {
  const text = item.content ?? "";
  const [open, setOpen] = useState(false);
  if (!text.trim()) return null;
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger
        render={<Marker className="w-full rounded px-1 -mx-1 py-1 transition-colors hover:bg-accent/40 data-[state=open]:bg-accent/30" />}
      >
        <MarkerIcon>
          <Brain className="size-3.5 text-violet-500/70" />
        </MarkerIcon>
        <MarkerContent className="flex items-center gap-1.5 text-xs italic text-muted-foreground">
          <ChevronRight className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
          {open ? "Hide reasoning" : "Reasoning"}
          {/* eslint-disable-next-line i18next/no-literal-string */}
          <span className="not-italic text-muted-foreground/50">{text.length.toLocaleString()} chars</span>
        </MarkerContent>
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="py-1.5 pl-5">
          <div className="prose prose-sm dark:prose-invert max-w-none rounded-lg border border-violet-500/20 bg-violet-500/[0.03] px-3 py-2 text-[13px] leading-relaxed text-muted-foreground [&>*:first-child]:mt-0 [&>*:last-child]:mb-0">
            <MemoizedMarkdown>{text}</MemoizedMarkdown>
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

export function ErrorMessage({ item }: { item: TimelineItem }) {
  return (
    <Bubble variant="destructive">
      <BubbleContent>
        <div className="flex items-start gap-2">
          <CircleAlert className="mt-0.5 size-3.5 shrink-0" />
          <pre className="whitespace-pre-wrap break-words text-xs">{item.content ?? ""}</pre>
        </div>
      </BubbleContent>
    </Bubble>
  );
}
