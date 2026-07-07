/**
 * Transform a flat, coalesced `TimelineItem[]` into a conversation-shaped
 * node list for ChatGPT-style rendering.
 *
 * The one real transformation: **pair each `tool_use` with its matching
 * `tool_result`.** Raw task_message rows carry no call id, so we match
 * FIFO-per-tool — the oldest in-flight call for a given tool name claims the
 * next result for that name. This mirrors how a tool call only re-enters the
 * queue once its predecessor for that tool has resolved. Confirmed against
 * staging data, where results return out of invocation order.
 *
 * Pairing lives in the presentation layer (not build-timeline) because the
 * inline chat view's `splitTimeline` needs the flat item list.
 */
import type { TimelineItem } from "./build-timeline";

/** A paired tool call: invocation + its resolved result (either may be null
 *  for orphan results or in-flight calls on live runs). */
export interface ToolNode {
  kind: "tool";
  /** Seq used for stable chronological ordering (result.seq when paired,
   *  else the use seq). */
  orderSeq: number;
  use: TimelineItem | null;
  result: TimelineItem | null;
}

export type ConversationNode =
  | { kind: "text"; item: TimelineItem }
  | { kind: "thinking"; item: TimelineItem }
  | { kind: "error"; item: TimelineItem }
  | ToolNode;

function nodeOrderSeq(node: ConversationNode): number {
  return node.kind === "tool" ? node.orderSeq : node.item.seq;
}

/** True when a tool node has neither input detail nor a result to show. */
export function isToolNodeEmpty(node: ToolNode): boolean {
  const hasInput = !!(node.use?.input && Object.keys(node.use.input).length > 0);
  const hasOutput = !!node.result?.output;
  return !hasInput && !hasOutput;
}

/**
 * Build conversation nodes from coalesced timeline items. Tool calls are
 * paired FIFO-per-tool; non-tool items pass through unchanged. Output is
 * sorted chronologically by each node's anchor seq.
 */
export function buildConversationNodes(items: TimelineItem[]): ConversationNode[] {
  // Pending tool_use items awaiting a result, FIFO per tool name. A Map is
  // correct here: keys are dynamic, inserted/shifted at runtime.
  const pendingByTool = new Map<string, TimelineItem[]>();
  const nodes: ConversationNode[] = [];

  for (const item of items) {
    if (item.type === "tool_use") {
      const tool = item.tool ?? "";
      const queue = pendingByTool.get(tool);
      if (queue) queue.push(item);
      else pendingByTool.set(tool, [item]);
      continue;
    }

    if (item.type === "tool_result") {
      const tool = item.tool ?? "";
      const queue = pendingByTool.get(tool);
      const use = queue?.shift() ?? null;
      // Anchor at the result seq — the moment the call became complete.
      nodes.push({ kind: "tool", orderSeq: item.seq, use, result: item });
      continue;
    }

    // text / thinking / error pass straight through.
    nodes.push({ kind: item.type, item });
  }

  // Flush never-returned calls (live runs, or a result that never landed) as
  // in-flight tool nodes anchored at their invocation seq.
  for (const queue of pendingByTool.values()) {
    for (const use of queue) {
      nodes.push({ kind: "tool", orderSeq: use.seq, use, result: null });
    }
  }

  nodes.sort((a, b) => nodeOrderSeq(a) - nodeOrderSeq(b));
  return nodes;
}
