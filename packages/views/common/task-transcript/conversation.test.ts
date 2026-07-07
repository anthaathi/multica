import { describe, expect, it } from "vitest";
import type { TimelineItem } from "./build-timeline";
import { buildConversationNodes, isToolNodeEmpty } from "./conversation";

function item(seq: number, type: TimelineItem["type"], tool?: string): TimelineItem {
  return { seq, type, tool, content: `c${seq}`, input: { x: seq }, output: `o${seq}` };
}

describe("buildConversationNodes", () => {
  it("pairs tool_use ↔ tool_result FIFO per tool when results return out of order", () => {
    // Staging-shaped: three calls fire, results come back out of order.
    const items: TimelineItem[] = [
      item(15, "tool_use", "read"),
      item(16, "tool_use", "grep"),
      item(17, "tool_use", "bash"),
      item(18, "tool_result", "grep"),
      item(19, "tool_result", "read"),
      item(21, "tool_result", "bash"),
    ];
    const nodes = buildConversationNodes(items);
    expect(nodes).toHaveLength(3);
    // Each is a paired tool node anchored at its result seq (chronological).
    expect(nodes.map((n) => (n.kind === "tool" ? n.use?.seq : null))).toEqual([16, 15, 17]);
    expect(nodes.map((n) => (n.kind === "tool" ? n.orderSeq : null))).toEqual([18, 19, 21]);
  });

  it("matches repeated calls to the same tool FIFO", () => {
    const items: TimelineItem[] = [
      item(1, "tool_use", "bash"),
      item(2, "tool_use", "bash"),
      item(3, "tool_result", "bash"),
      item(4, "tool_result", "bash"),
    ];
    const nodes = buildConversationNodes(items);
    expect(nodes.map((n) => (n.kind === "tool" ? n.use?.seq : null))).toEqual([1, 2]);
  });

  it("emits an orphan result as a tool node with use=null", () => {
    const nodes = buildConversationNodes([item(5, "tool_result", "bash")]);
    expect(nodes).toHaveLength(1);
    expect(nodes[0]).toMatchObject({ kind: "tool", use: null });
  });

  it("flushes in-flight uses (no result) as tool nodes anchored at use seq", () => {
    const nodes = buildConversationNodes([item(7, "tool_use", "bash")]);
    expect(nodes).toHaveLength(1);
    expect(nodes[0]).toMatchObject({ kind: "tool", result: null, orderSeq: 7 });
  });

  it("passes text / thinking / error through unchanged", () => {
    const nodes = buildConversationNodes([
      item(1, "thinking"),
      item(2, "text"),
      item(3, "error"),
    ]);
    expect(nodes.map((n) => n.kind)).toEqual(["thinking", "text", "error"]);
  });

  it("keeps a result anchored behind a later text node when its result lands later", () => {
    const items: TimelineItem[] = [
      item(10, "tool_use", "bash"),
      item(11, "text"),
      item(12, "tool_result", "bash"),
    ];
    const nodes = buildConversationNodes(items);
    // text(11) sorts before the bash node anchored at 12.
    expect(nodes.map((n) => (n.kind === "tool" ? n.orderSeq : n.item.seq))).toEqual([11, 12]);
  });
});

describe("isToolNodeEmpty", () => {
  it("is empty only when there is no input detail and no output", () => {
    expect(isToolNodeEmpty({ kind: "tool", orderSeq: 1, use: null, result: null })).toBe(true);
    expect(
      isToolNodeEmpty({ kind: "tool", orderSeq: 1, use: item(1, "tool_use"), result: null }),
    ).toBe(false);
    expect(
      isToolNodeEmpty({ kind: "tool", orderSeq: 1, use: null, result: item(2, "tool_result") }),
    ).toBe(false);
  });
});
