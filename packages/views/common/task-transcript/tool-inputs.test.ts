import { describe, expect, it } from "vitest";
import {
  getInputPath,
  readToolInput,
  shortenPath,
  summarizeToolInput,
} from "./tool-inputs";

describe("readToolInput", () => {
  it("keeps known string fields and drops non-strings + unknown keys", () => {
    const input = {
      command: "ls -la",
      pattern: "foo",
      count: 3, // non-string → dropped
      weird: "x", // unknown → dropped
      file_path: "/a/b.ts",
    };
    expect(readToolInput(input)).toEqual({
      command: "ls -la",
      pattern: "foo",
      file_path: "/a/b.ts",
    });
  });

  it.each([null, undefined, "nope", 42, [], true])("returns {} for non-objects (%s)", (v) => {
    expect(readToolInput(v)).toEqual({});
  });
});

describe("getInputPath", () => {
  it("prefers file_path, then filePath, then path", () => {
    expect(getInputPath({ file_path: "/a", filePath: "/b", path: "/c" })).toBe("/a");
    expect(getInputPath({ filePath: "/b", path: "/c" })).toBe("/b");
    expect(getInputPath({ path: "/c" })).toBe("/c");
    expect(getInputPath({})).toBeUndefined();
  });
});

describe("shortenPath", () => {
  it("leaves short paths untouched, collapses long ones to last two segments", () => {
    expect(shortenPath("a/b")).toBe("a/b");
    expect(shortenPath("/home/ubuntu/work/dir/file.ts")).toBe(".../dir/file.ts");
  });
});

describe("summarizeToolInput", () => {
  it("prefers command for bash-like tools", () => {
    expect(summarizeToolInput({ command: "git status", timeout: 60000 })).toBe("git status");
  });

  it("uses the path for read/edit", () => {
    expect(summarizeToolInput({ file_path: "/home/ubuntu/work/dir/file.ts" })).toBe(
      ".../dir/file.ts",
    );
  });

  it("uses pattern for grep", () => {
    expect(summarizeToolInput({ pattern: "main\\.dart", path: "/lib" })).toBe("main\\.dart");
  });

  it("returns <edit> for edit old/new strings", () => {
    expect(summarizeToolInput({ oldString: "a", newString: "b" })).toBe("<edit>");
  });

  it("returns empty string for empty input", () => {
    expect(summarizeToolInput({})).toBe("");
    expect(summarizeToolInput(null)).toBe("");
  });
});
