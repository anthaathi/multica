import { describe, expect, it } from "vitest";
import { normalizeOutput, parseReadOutput } from "./parse-output";

describe("normalizeOutput", () => {
  it("treats empty markers as empty text", () => {
    for (const v of ["", "{}", "[]", "null", "undefined", "   "]) {
      expect(normalizeOutput(v)).toEqual({ text: "", isStructured: false });
    }
  });

  it("returns empty for non-string input", () => {
    expect(normalizeOutput(undefined)).toEqual({ text: "", isStructured: false });
    expect(normalizeOutput(null)).toEqual({ text: "", isStructured: false });
  });

  it("unwraps the content-array wrapper shape", () => {
    const wrapped = JSON.stringify({
      content: [
        { type: "text", text: "first " },
        { type: "text", text: "second" },
      ],
    });
    expect(normalizeOutput(wrapped)).toEqual({ text: "first second", isStructured: false });
  });

  it("pretty-prints plain JSON objects as structured text", () => {
    const out = normalizeOutput('{"assignee_id":"abc","status":"ok"}');
    expect(out.isStructured).toBe(true);
    expect(out.text).toBe(JSON.stringify({ assignee_id: "abc", status: "ok" }, null, 2));
  });

  it("passes raw stdout through when it is not JSON", () => {
    const raw = "Found 1 matches\n/lib/main.dart:\n  Line 63: void main() async {";
    expect(normalizeOutput(raw)).toEqual({ text: raw, isStructured: false });
  });

  it("passes raw text through even if it starts with a brace that isn't valid JSON", () => {
    const raw = "{ not json at all";
    expect(normalizeOutput(raw)).toEqual({ text: raw, isStructured: false });
  });

  it("keeps the pseudo-XML read output as-is (the read card parses it)", () => {
    const raw = "<path>/a/b.ts</path>\n<content>\n1: hi\n</content>";
    expect(normalizeOutput(raw)).toEqual({ text: raw, isStructured: false });
  });

  it("ignores non-text items in a content array", () => {
    const wrapped = JSON.stringify({
      content: [
        { type: "text", text: "only this" },
        { type: "image", data: "x" },
      ],
    });
    expect(normalizeOutput(wrapped)).toEqual({ text: "only this", isStructured: false });
  });
});

describe("parseReadOutput", () => {
  it("extracts path + content and strips N: line prefixes", () => {
    const raw =
      "<path>/home/ubuntu/work/pubspec.yaml</path>\n<type>file</type>\n<content>\n1: name: e2hub\n2: description: app\n</content>";
    expect(parseReadOutput(raw)).toEqual({
      path: "/home/ubuntu/work/pubspec.yaml",
      content: "name: e2hub\ndescription: app",
    });
  });

  it("anchors to the last </content> so embedded closers in code don't truncate", () => {
    const raw = "<content>\n1: const s = \"</content>\"\n2: x\n</content>";
    expect(parseReadOutput(raw)?.content).toBe('const s = "</content>"\nx');
  });

  it("returns null when the payload is not the read shape", () => {
    expect(parseReadOutput("just plain text")).toBeNull();
    expect(parseReadOutput("Wrote file successfully.")).toBeNull();
    expect(parseReadOutput("<content>no closer")).toBeNull();
  });

  it("omits path when the <path> tag is absent", () => {
    const raw = "<content>\n1: hi\n</content>";
    expect(parseReadOutput(raw)).toEqual({ path: undefined, content: "hi" });
  });
});
