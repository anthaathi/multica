/**
 * Normalize a tool_result `output` string into clean display text.
 *
 * The on-disk shape is inconsistent across runtimes and tools (confirmed
 * against staging data):
 *   - `{}`                                  — empty bash result
 *   - `{"content":[{"type":"text","text":…}]}` — content-array wrapper
 *   - a plain JSON object/array             — bash stdout that happens to be JSON
 *   - pseudo-XML `<path>…</path><content>…` — the `read` tool
 *   - raw text                              — everything else (write/grep/bash stdout)
 *
 * All parsing here is defensive: malformed JSON is treated as raw text, never
 * thrown. No casts onto the parsed value — membership is checked at runtime.
 */

/** A tool_result `output` resolved to the text we should show and copy. */
export interface NormalizedOutput {
  text: string;
  /** True when the payload was a JSON object/array we pretty-printed. */
  isStructured: boolean;
}

const EMPTY_MARKERS: Record<string, true> = { "{}": true, "[]": true, null: true, undefined: true };

/**
 * Pull the joined `.text` out of a `{"content":[{"type":"text","text":…}]}`-
 * shaped value. Returns null when the value isn't that shape so the caller
 * can fall back to pretty-printing the raw JSON.
 */
function extractContentArrayText(value: unknown): string | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  if (!("content" in value)) return null;
  // After the `in` check, `value.content` is narrowed to unknown — no cast.
  const content = value.content;
  if (!Array.isArray(content)) return null;
  let out = "";
  for (const item of content) {
    if (item && typeof item === "object" && "text" in item) {
      const t = item.text;
      if (typeof t === "string") out += t;
    }
  }
  return out;
}

/** Normalize any tool_result output into clean display text. */
export function normalizeOutput(output: string | undefined | null): NormalizedOutput {
  if (typeof output !== "string") return { text: "", isStructured: false };
  const trimmed = output.trim();
  if (trimmed.length === 0 || trimmed in EMPTY_MARKERS) return { text: "", isStructured: false };

  // Try JSON for object/array payloads only (raw stdout often starts with `{`
  // in shell output and isn't JSON — JSON.parse guards that).
  if (trimmed[0] === "{" || trimmed[0] === "[") {
    try {
      const parsed: unknown = JSON.parse(trimmed);
      const wrapped = extractContentArrayText(parsed);
      if (wrapped !== null) return { text: wrapped, isStructured: false };
      // Valid JSON that isn't the content-array shape → pretty-print.
      return { text: JSON.stringify(parsed, null, 2), isStructured: true };
    } catch {
      // Not valid JSON; fall through to raw text.
    }
  }
  return { text: output, isStructured: false };
}

export interface ReadFileContent {
  path?: string;
  content: string;
}

/**
 * Parse the `read` tool's pseudo-XML output:
 *   <path>/abs/path</path>\n<type>file</type>\n<content>\n1: line\n2: line\n</content>
 * Returns null when the payload isn't this shape (so the read card can fall
 * back to the generic renderer). The content closer is anchored to the LAST
 * `</content>` so embedded `</content>` text inside code doesn't truncate.
 */
export function parseReadOutput(output: string): ReadFileContent | null {
  const pathMatch = output.match(/<path>([\s\S]*?)<\/path>/);
  const openIdx = output.indexOf("<content>");
  const closeIdx = output.lastIndexOf("</content>");
  if (openIdx === -1 || closeIdx === -1 || closeIdx <= openIdx) return null;
  let content = output.slice(openIdx + "<content>".length, closeIdx);
  // Strip the `N: ` line-number prefix the read tool emits.
  content = content.replace(/^\s*\d+:\s?/gm, "");
  return { path: pathMatch?.[1]?.trim(), content: content.replace(/^\n+|\n+$/g, "") };
}
