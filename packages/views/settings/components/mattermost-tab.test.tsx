// @vitest-environment jsdom

import { type ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

type MemberRole = "owner" | "admin" | "member" | "guest";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const installationsRef = vi.hoisted(() => ({
  current: {
    installations: [] as unknown[],
    configured: true,
    install_supported: true,
  },
}));
const mockRegisterBYO = vi.hoisted(() => vi.fn());
const mockDeleteInstallation = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());
const mockToast = vi.hoisted(() => ({
  success: vi.fn(),
  error: vi.fn(),
  message: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current, isLoading: false };
    if (key.includes("installations"))
      return { data: installationsRef.current, isLoading: false };
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getAgentName: (agentId: string) => `Agent ${agentId}`,
    getMemberName: () => "Unknown",
    getSquadName: () => "Unknown Squad",
    getActorName: () => "Unknown",
    getActorInitials: () => "??",
    getActorAvatarUrl: () => null,
  }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => (
    <span data-testid="actor-avatar" data-actor-id={actorId} />
  ),
}));

vi.mock("@multica/core/mattermost", () => ({
  mattermostInstallationsOptions: () => ({
    queryKey: ["mattermost", "installations"],
    queryFn: vi.fn(),
  }),
  mattermostKeys: {
    installations: (wsId: string) => ["mattermost", "installations", wsId],
  },
}));

vi.mock("@multica/core/api", () => ({
  api: {
    registerMattermostBYO: mockRegisterBYO,
    deleteMattermostInstallation: mockDeleteInstallation,
  },
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string } }) => unknown) =>
      sel ? sel({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("sonner", () => ({ toast: mockToast }));

vi.mock("../../platform", () => ({ openExternal: mockOpenExternal }));

import { MattermostAgentBindButton, MattermostTab } from "./mattermost-tab";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

function renderUI(children: ReactNode) {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>,
  );
}

function resetFixtures() {
  vi.clearAllMocks();
  membersRef.current = [{ user_id: "user-1", role: "owner" }];
  installationsRef.current = { installations: [], configured: true, install_supported: true };
}

describe("MattermostAgentBindButton", () => {
  beforeEach(resetFixtures);

  it("opens the BYO dialog and submits the pasted server URL + bot token", async () => {
    mockRegisterBYO.mockResolvedValue({
      id: "i1",
      agent_id: "agent-1",
      status: "active",
    });
    renderUI(<MattermostAgentBindButton agentId="agent-1" agentName="Bot" />);
    await userEvent.click(screen.getByTestId("mattermost-agent-connect"));
    const serverInput = await screen.findByTestId("mattermost-byo-server-url");
    await userEvent.type(serverInput, "https://mm.example.com");
    await userEvent.type(screen.getByTestId("mattermost-byo-bot-token"), "mm-bot-token");
    await userEvent.click(screen.getByTestId("mattermost-byo-submit"));
    await waitFor(() =>
      expect(mockRegisterBYO).toHaveBeenCalledWith("workspace-1", "agent-1", {
        server_url: "https://mm.example.com",
        bot_token: "mm-bot-token",
      }),
    );
    // On success the installations query is invalidated so the connected
    // badge appears immediately, and a success toast fires.
    expect(mockInvalidate).toHaveBeenCalledWith({
      queryKey: ["mattermost", "installations", "workspace-1"],
    });
    expect(mockToast.success).toHaveBeenCalled();
    // BYO is a direct API call — no external browser redirect.
    expect(mockOpenExternal).not.toHaveBeenCalled();
  }, 15000);

  it("shows the failed toast when the bot token is rejected", async () => {
    mockRegisterBYO.mockRejectedValue(new Error("invalid bot token"));
    renderUI(<MattermostAgentBindButton agentId="agent-1" agentName="Bot" />);
    await userEvent.click(screen.getByTestId("mattermost-agent-connect"));
    await userEvent.type(
      await screen.findByTestId("mattermost-byo-server-url"),
      "https://mm.example.com",
    );
    await userEvent.type(screen.getByTestId("mattermost-byo-bot-token"), "bad-token");
    await userEvent.click(screen.getByTestId("mattermost-byo-submit"));
    await waitFor(() => expect(mockToast.error).toHaveBeenCalled());
    // A failed install must not invalidate the list or show a success toast.
    expect(mockInvalidate).not.toHaveBeenCalled();
    expect(mockToast.success).not.toHaveBeenCalled();
  }, 15000);

  it("shows the connected badge (not the CTA) when the agent already has an active install", () => {
    installationsRef.current = {
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          server_url: "https://mm.example.com",
          bot_user_id: "b1",
          bot_username: "multica",
        },
      ],
      configured: true,
      install_supported: true,
    };
    renderUI(<MattermostAgentBindButton agentId="agent-1" />);
    expect(screen.getByTestId("mattermost-agent-bot-connected")).toBeTruthy();
    expect(screen.getByTestId("mattermost-agent-bot-disconnect")).toBeTruthy();
    expect(screen.queryByTestId("mattermost-agent-connect")).toBeNull();
  });

  it("renders nothing for a non-manager", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    const { container } = renderUI(<MattermostAgentBindButton agentId="agent-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when install is unavailable and the agent is unbound", () => {
    installationsRef.current = { installations: [], configured: true, install_supported: false };
    const { container } = renderUI(<MattermostAgentBindButton agentId="agent-1" />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("MattermostTab", () => {
  beforeEach(resetFixtures);

  it("surfaces the not-enabled notice when the deployment has no Mattermost key", () => {
    installationsRef.current = { installations: [], configured: false, install_supported: false };
    renderUI(<MattermostTab />);
    expect(screen.getByText(/Mattermost integration not enabled/i)).toBeTruthy();
  });

  it("shows the empty state when configured but nothing is connected", () => {
    renderUI(<MattermostTab />);
    expect(screen.getByText(/No bots connected yet/i)).toBeTruthy();
  });

  it("lists a connected installation with its agent name and a disconnect control", () => {
    installationsRef.current = {
      installations: [
        {
          id: "i1",
          agent_id: "agent-7",
          status: "active",
          server_url: "https://mm.example.com",
          installed_at: "2026-01-01T00:00:00Z",
        },
      ],
      configured: true,
      install_supported: true,
    };
    renderUI(<MattermostTab />);
    expect(screen.getByText("Agent agent-7")).toBeTruthy();
    expect(screen.getByText(/Disconnect/i)).toBeTruthy();
  });

  it("disconnects an installation after confirmation, then toasts success", async () => {
    mockDeleteInstallation.mockResolvedValue(undefined);
    installationsRef.current = {
      installations: [
        {
          id: "i1",
          agent_id: "agent-7",
          status: "active",
          server_url: "https://mm.example.com",
          installed_at: "2026-01-01T00:00:00Z",
        },
      ],
      configured: true,
      install_supported: true,
    };
    renderUI(<MattermostTab />);
    // The row's Disconnect button opens the confirm dialog.
    await userEvent.click(screen.getByRole("button", { name: /^Disconnect$/i }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(
      within(dialog).getByRole("button", { name: /^Disconnect$/i }),
    );
    await waitFor(() =>
      expect(mockDeleteInstallation).toHaveBeenCalledWith("workspace-1", "i1"),
    );
    expect(mockInvalidate).toHaveBeenCalledWith({
      queryKey: ["mattermost", "installations", "workspace-1"],
    });
    expect(mockToast.success).toHaveBeenCalled();
  });
});
