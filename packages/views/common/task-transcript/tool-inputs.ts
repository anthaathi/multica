/**
 * Type-safe extraction of display fields from a tool's `input` payload.
 *
 * `input` arrives from the network (task_message.input) typed as
 * `Record<string, unknown>`. Shapes vary per tool and per runtime, so we never
 * cast it to a fabricated shape — we walk entries and keep only the string
 * values for keys we render. Everything else is dropped, never stringified by
 * a lying cast.
 */

/** The string input fields we surface in summaries and tool cards. */
export interface ToolInputFields {
  command?: string;
  /** Multica / Claude-style path keys. */
  file_path?: string;
  filePath?: string;
  path?: string;
  pattern?: string;
  query?: string;
  description?: string;
  prompt?: string;
  skill?: string;
  /** edit-style before/after. */
  oldString?: string;
  newString?: string;
  /** write-style content. */
  content?: string;
}

const STRING_FIELDS = [
  "command",
  "file_path",
  "filePath",
  "path",
  "pattern",
  "query",
  "description",
  "prompt",
  "skill",
  "oldString",
  "newString",
  "content",
] as const;
function isStringField(key: string): key is StringFieldName {
  return (STRING_FIELDS as readonly string[]).includes(key);
}
type StringFieldName = (typeof STRING_FIELDS)[number];

/**
 * Narrow an unknown tool input to the string fields we render. Non-string
 * values and unknown keys are ignored. Returns an empty object for anything
 * that isn't a plain object.
 */
export function readToolInput(input: unknown): ToolInputFields {
  if (!input || typeof input !== "object" || Array.isArray(input)) return {};
  const out: ToolInputFields = {};
  for (const [key, value] of Object.entries(input)) {
    if (typeof value === "string" && isStringField(key)) {
      out[key] = value;
    }
  }
  return out;
}

/** Path-like field, preferring the most specific key present. */
export function getInputPath(fields: ToolInputFields): string | undefined {
  return fields.file_path ?? fields.filePath ?? fields.path;
}

/** Compact a long filesystem path to its last two segments for display. */
export function shortenPath(p: string): string {
  const parts = p.split("/");
  if (parts.length <= 3) return p;
  return ".../" + parts.slice(-2).join("/");
}
export function summarizeToolInput(input: unknown): string {
  const f = readToolInput(input);
  if (f.command) return f.command;
  if (f.query) return f.query;
  if (f.pattern) return f.pattern;
  const p = getInputPath(f);
  if (p) return shortenPath(p);
  if (f.description) return f.description;
  if (f.prompt) return f.prompt;
  if (f.skill) return f.skill;
  if (f.oldString || f.newString) return "<edit>";
  // Fallback: first short string field actually present.
  for (const key of STRING_FIELDS) {
    const v = f[key];
    if (typeof v === "string" && v.length > 0 && v.length <= 120) return v;
  }
  return "";
}
