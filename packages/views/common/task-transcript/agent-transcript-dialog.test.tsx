// @vitest-environment jsdom

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ButtonHTMLAttributes, ReactNode } from "react";
import type { AgentTask } from "@multica/core/types/agent";
import { useTranscriptViewStore } from "@multica/core/agents/stores";
import { renderWithI18n } from "../../test/i18n";
import { AgentTranscriptDialog } from "./agent-transcript-dialog";
import type { TimelineItem } from "./build-timeline";

vi.mock("@multica/core/api", () => ({
  api: {
    getAgent: vi.fn().mockResolvedValue(null),
    listRuntimes: vi.fn().mockResolvedValue([]),
  },
}));

vi.mock("../actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="actor-avatar" />,
}));

// Real MemoizedMarkdown pulls in shiki (heavy/flaky in jsdom). Render the raw
// children so text-content assertions stay deterministic.
vi.mock("../markdown", () => ({
  MemoizedMarkdown: ({ children }: { children: ReactNode }) => <>{children}</>,
}));

vi.mock("@multica/ui/components/ui/dialog", () => ({
  Dialog: ({ open, children }: { open: boolean; children: ReactNode }) =>
    open ? <>{children}</> : null,
  DialogContent: ({ children }: { children: ReactNode }) => (
    <div role="dialog">{children}</div>
  ),
  DialogTitle: ({ children }: { children: ReactNode }) => <h2>{children}</h2>,
}));

vi.mock("@multica/ui/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DropdownMenuTrigger: (props: ButtonHTMLAttributes<HTMLButtonElement>) => (
    <button type="button" {...props}>
      {props.children}
    </button>
  ),
  DropdownMenuContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DropdownMenuSeparator: () => <hr />,
  DropdownMenuCheckboxItem: ({
    checked,
    onCheckedChange,
    children,
  }: {
    checked?: boolean;
    onCheckedChange?: (checked: boolean) => void;
    children: ReactNode;
  }) => (
    <button
      type="button"
      role="menuitemcheckbox"
      aria-checked={checked === true}
      onClick={() => onCheckedChange?.(checked !== true)}
    >
      {children}
    </button>
  ),
  DropdownMenuItem: ({
    children,
    onClick,
  }: ButtonHTMLAttributes<HTMLButtonElement>) => (
    <button type="button" onClick={onClick}>
      {children}
    </button>
  ),
}));

vi.mock("@multica/ui/components/ui/collapsible", async () => {
  // vi.mock factories hoist above top-level imports, so the React binding is
  // unavailable here. Resolving it inside the factory is the documented vitest
  // pattern (a module-loading test boundary).
  const React = await import("react");
  const Context = React.createContext<{ open: boolean; onOpenChange?: (o: boolean) => void }>({
    open: false,
  });
  return {
    Collapsible: ({
      open,
      onOpenChange,
      children,
    }: {
      open: boolean;
      onOpenChange?: (open: boolean) => void;
      children: ReactNode;
    }) => <Context.Provider value={{ open, onOpenChange }}>{children}</Context.Provider>,
    CollapsibleTrigger: ({ disabled, children }: ButtonHTMLAttributes<HTMLButtonElement>) => {
      const ctx = React.useContext(Context);
      return (
        <button
          type="button"
          disabled={disabled}
          onClick={() => {
            if (!disabled) ctx.onOpenChange?.(!ctx.open);
          }}
        >
          {children}
        </button>
      );
    },
    CollapsibleContent: ({ children }: { children: ReactNode }) => {
      const ctx = React.useContext(Context);
      return ctx.open ? <div>{children}</div> : null;
    },
  };
});

// Virtuoso renders asynchronously in jsdom (no measured viewport), making sync
// getByText flaky. Render every item synchronously instead.
vi.mock("react-virtuoso", () => ({
  Virtuoso: ({
    data,
    itemContent,
  }: {
    data: unknown[];
    itemContent: (index: number, item: unknown) => ReactNode;
  }) => (
    <div data-testid="virtuoso">
      {data.map((item, i) => (
        <div key={i}>{itemContent(i, item)}</div>
      ))}
    </div>
  ),
}));

const baseTask: AgentTask = {
  id: "task-1",
  agent_id: "",
  runtime_id: "",
  issue_id: "issue-1",
  status: "completed",
  priority: 0,
  dispatched_at: null,
  started_at: "2026-06-08T08:00:00Z",
  completed_at: "2026-06-08T08:01:00Z",
  result: null,
  error: null,
  created_at: "2026-06-08T08:00:00Z",
};

// text (always-visible prose) + thinking (collapsed by default) + an in-flight
// bash call with no result yet.
const items: TimelineItem[] = [
  { seq: 1, type: "text", content: "Agent summary\nAgent hidden detail" },
  { seq: 2, type: "thinking", content: "Thinking summary\nThinking hidden detail" },
  { seq: 3, type: "tool_use", tool: "bash", input: { command: "pnpm test" } },
];

function renderDialog(dialogItems: TimelineItem[] = items) {
  return renderWithI18n(
    <AgentTranscriptDialog
      open
      onOpenChange={vi.fn()}
      task={baseTask}
      items={dialogItems}
      agentName="Codex"
    />,
  );
}

beforeEach(() => {
  cleanup();
  useTranscriptViewStore.setState({
    sortDirection: "chronological",
    preserveFilters: false,
    selectedFilterKeys: [],
    defaultExpanded: false,
  });
});

afterEach(() => {
  cleanup();
});

describe("AgentTranscriptDialog — ChatGPT-style conversation", () => {
  it("shows assistant text immediately (chat-like, no expand needed)", () => {
    renderDialog();
    expect(screen.getByText(/Agent hidden detail/)).toBeInTheDocument();
  });

  it("keeps reasoning collapsed by default and reveals it on click", () => {
    renderDialog();
    const list = screen.getByTestId("virtuoso");

    expect(screen.queryByText(/Thinking hidden detail/)).not.toBeInTheDocument();

    // Scope to the conversation list: the TimelineBar segments share the
    // "Reasoning" label via their title attributes.
    fireEvent.click(within(list).getByText("Reasoning"));

    expect(screen.getByText(/Thinking hidden detail/)).toBeInTheDocument();
  });

  it("renders a paired tool card collapsed by default and expands to show the command", () => {
    renderDialog();
    const list = screen.getByTestId("virtuoso");

    expect(within(list).getByText("bash")).toBeInTheDocument();
    expect(screen.queryByText("$ pnpm test")).not.toBeInTheDocument();

    fireEvent.click(within(list).getByText("bash"));

    expect(screen.getByText("$ pnpm test")).toBeInTheDocument();
    expect(screen.getByText(/no output/)).toBeInTheDocument();
  });

  it("preserves selected filters across dialog remounts when enabled", () => {
    const first = renderDialog();

    fireEvent.click(screen.getByRole("menuitemcheckbox", { name: "Reasoning" }));
    fireEvent.click(screen.getByRole("menuitemcheckbox", { name: "Preserve filters" }));

    // Only the reasoning node remains; agent text is filtered out. The
    // reasoning trigger is visible (the content stays collapsed until opened).
    expect(screen.queryByText(/Agent hidden detail/)).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /Reasoning/ }).length).toBeGreaterThan(0);
    expect(useTranscriptViewStore.getState().selectedFilterKeys).toEqual(["thinking"]);

    first.unmount();
    renderDialog();

    expect(screen.queryByText(/Agent hidden detail/)).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /Reasoning/ }).length).toBeGreaterThan(0);
  });

  it("ignores stale persisted filter keys that are not available in the current transcript", () => {
    useTranscriptViewStore.setState({
      preserveFilters: true,
      selectedFilterKeys: ["thinking"],
    });

    renderDialog([{ seq: 1, type: "text", content: "Only agent summary" }]);

    expect(screen.getByText("Only agent summary")).toBeInTheDocument();
    expect(screen.queryByText("No execution data recorded.")).not.toBeInTheDocument();
  });

  it("expands and collapses tool cards via the expand-all toggle", () => {
    renderDialog();

    expect(screen.queryByText("$ pnpm test")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Expand visible" }));

    expect(screen.getByText("$ pnpm test")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Collapse visible" }));

    expect(screen.queryByText("$ pnpm test")).not.toBeInTheDocument();
  });

  it("uses the default-expanded preference to auto-open tool cards", () => {
    useTranscriptViewStore.setState({ defaultExpanded: true });

    renderDialog();

    expect(screen.getByText("$ pnpm test")).toBeInTheDocument();
  });
});
