"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, ExternalLink, Link2, PanelRight } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Label } from "@multica/ui/components/ui/label";
import { Switch } from "@multica/ui/components/ui/switch";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { memberListOptions, workspaceKeys } from "@multica/core/workspace/queries";
import { deriveGitLabSettings, gitlabConnectionsOptions } from "@multica/core/gitlab";
import { api } from "@multica/core/api";
import type { Workspace } from "@multica/core/types";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { GitLabMark } from "./gitlab-mark";

type SettingsKey =
  | "gitlab_enabled"
  | "gitlab_mr_sidebar_enabled"
  | "gitlab_auto_link_mrs_enabled";

export function GitLabTab() {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const navigation = useNavigation();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canView = !!currentMember;

  const { data: connectionData } = useQuery({
    ...gitlabConnectionsOptions(wsId),
    enabled: !!wsId && canView,
  });
  const connections = connectionData?.connections ?? [];
  const configured = connectionData?.configured ?? false;
  const canManage = connectionData?.can_manage === true;
  const connected = connections.length > 0;
  const primaryConnection = connections[0] ?? null;

  const flags = deriveGitLabSettings(workspace);
  const [savingKey, setSavingKey] = useState<SettingsKey | null>(null);
  const [connecting, setConnecting] = useState(false);
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function persistSetting(key: SettingsKey, next: boolean) {
    if (!workspace || savingKey) return;
    setSavingKey(key);
    try {
      const merged = {
        ...((workspace.settings as Record<string, unknown>) ?? {}),
        [key]: next,
      };
      const updated = await api.updateWorkspace(workspace.id, { settings: merged });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_failed));
    } finally {
      setSavingKey(null);
    }
  }

  async function handleConnect() {
    setConnecting(true);
    try {
      const resp = await api.getGitLabConnectURL(wsId);
      if (!resp.configured || !resp.url) {
        toast.error(t(($) => $.gitlab.toast_not_configured));
        return;
      }
      window.open(resp.url, "_blank", "noopener");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_open_failed));
    } finally {
      setConnecting(false);
    }
  }

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteGitLabConnection(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: ["gitlab", wsId] });
      toast.success(t(($) => $.gitlab.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.gitlab.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  async function copyToClipboard(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      toast.success(t(($) => $.gitlab.webhook_copied));
    } catch {
      // Clipboard can be unavailable (insecure context); ignore silently.
    }
  }

  if (!workspace) return null;

  const repositoriesHref = `${navigation.pathname}?tab=repositories`;

  return (
    <div className="space-y-8">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">{t(($) => $.gitlab.page_description)}</p>
      </section>

      <section className="space-y-3">
        <Card>
          <CardContent>
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-start gap-3">
                <div className="rounded-md border bg-muted/50 p-2 text-muted-foreground">
                  <GitLabMark className="h-4 w-4" />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="gitlab-master" className="text-sm font-medium">
                    {t(($) => $.gitlab.section_master)}
                  </Label>
                  <p className="text-sm text-muted-foreground">
                    {flags.enabled
                      ? t(($) => $.gitlab.master_description_on)
                      : t(($) => $.gitlab.master_description_off)}
                  </p>
                </div>
              </div>
              <Switch
                id="gitlab-master"
                checked={flags.enabled}
                onCheckedChange={(v) => persistSetting("gitlab_enabled", v)}
                disabled={!canManage || savingKey === "gitlab_enabled"}
              />
            </div>
          </CardContent>
        </Card>
      </section>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.gitlab.section_connection)}</h2>
        <Card>
          <CardContent className="space-y-4">
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-start gap-3">
                <GitLabMark className="h-6 w-6 mt-0.5 shrink-0" />
                <div className="space-y-1">
                  <p className="text-sm font-medium">{t(($) => $.gitlab.connection_title)}</p>
                  {connected ? (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.gitlab.connected_to, {
                        login: connections.map((c) => c.gitlab_username).join(", "),
                      })}
                    </p>
                  ) : canManage ? (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.gitlab.connection_description)}
                    </p>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.gitlab.contact_admin_to_connect)}
                    </p>
                  )}
                </div>
              </div>
              {canManage && (
                <div className="flex items-center gap-2">
                  {connected && primaryConnection ? (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setDisconnectTarget(primaryConnection.id)}
                    >
                      {t(($) => $.gitlab.disconnect)}
                    </Button>
                  ) : (
                    <Button
                      size="sm"
                      onClick={handleConnect}
                      disabled={connecting || !configured}
                      title={!configured ? t(($) => $.gitlab.connect_disabled_tooltip) : undefined}
                    >
                      {connecting
                        ? t(($) => $.gitlab.connect_opening)
                        : t(($) => $.gitlab.connect_gitlab)}
                    </Button>
                  )}
                </div>
              )}
            </div>

            {canManage && !configured && (
              <p className="text-xs text-muted-foreground">{t(($) => $.gitlab.not_configured)}</p>
            )}

            {!canManage && connected && (
              <p className="text-xs text-muted-foreground">{t(($) => $.gitlab.read_only_hint)}</p>
            )}
          </CardContent>
        </Card>
      </section>

      {canManage && connected && primaryConnection?.webhook_url && (
        <section className="space-y-3">
          <h2 className="text-sm font-semibold">{t(($) => $.gitlab.section_webhook)}</h2>
          <Card>
            <CardContent className="space-y-3">
              <p className="text-xs text-muted-foreground">{t(($) => $.gitlab.webhook_description)}</p>
              <WebhookField
                label={t(($) => $.gitlab.webhook_url_label)}
                value={primaryConnection.webhook_url}
                onCopy={copyToClipboard}
              />
              {primaryConnection.webhook_secret && (
                <WebhookField
                  label={t(($) => $.gitlab.webhook_secret_label)}
                  value={primaryConnection.webhook_secret}
                  onCopy={copyToClipboard}
                />
              )}
            </CardContent>
          </Card>
        </section>
      )}

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.gitlab.section_features)}</h2>
        <Card>
          <CardContent className="space-y-4">
            <FeatureRow
              id="gitlab-mr-sidebar"
              icon={<PanelRight className="h-4 w-4" />}
              label={t(($) => $.gitlab.feature_mr_sidebar_label)}
              description={t(($) => $.gitlab.feature_mr_sidebar_description)}
              checked={flags.mrSidebar}
              disabled={!canManage || !flags.enabled || savingKey === "gitlab_mr_sidebar_enabled"}
              onCheckedChange={(v) => persistSetting("gitlab_mr_sidebar_enabled", v)}
            />

            <FeatureRow
              id="gitlab-auto-link"
              icon={<Link2 className="h-4 w-4" />}
              label={t(($) => $.gitlab.feature_auto_link_label)}
              description={t(($) => $.gitlab.feature_auto_link_description)}
              checked={flags.autoLinkMRs}
              disabled={!canManage || !flags.enabled || savingKey === "gitlab_auto_link_mrs_enabled"}
              onCheckedChange={(v) => persistSetting("gitlab_auto_link_mrs_enabled", v)}
            />
          </CardContent>
        </Card>
      </section>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.gitlab.section_repositories)}</h2>
        <Card>
          <CardContent>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="text-sm font-medium">
                {t(($) => $.gitlab.repositories_shortcut_label)}
              </p>
              <Button
                variant="outline"
                size="sm"
                onClick={() => navigation.push(repositoriesHref)}
              >
                <ExternalLink className="h-3 w-3" />
                {t(($) => $.gitlab.repositories_shortcut_link)}
              </Button>
            </div>
          </CardContent>
        </Card>
      </section>

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.gitlab.disconnect_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.gitlab.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.gitlab.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.gitlab.disconnecting)
                : t(($) => $.gitlab.disconnect_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function WebhookField({
  label,
  value,
  onCopy,
}: {
  label: string;
  value: string;
  onCopy: (value: string) => void;
}) {
  return (
    <div className="space-y-1">
      <Label className="text-xs text-muted-foreground">{label}</Label>
      <div className="flex items-center gap-2">
        <code className="flex-1 truncate rounded bg-muted px-2 py-1 text-[11px]">{value}</code>
        <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => onCopy(value)}>
          <Copy className="h-3.5 w-3.5" />
        </Button>
      </div>
    </div>
  );
}

function FeatureRow({
  id,
  icon,
  label,
  description,
  checked,
  disabled,
  onCheckedChange,
}: {
  id: string;
  icon: React.ReactNode;
  label: string;
  description: string;
  checked: boolean;
  disabled: boolean;
  onCheckedChange: (v: boolean) => void;
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div className="flex items-start gap-3">
        <div className="rounded-md border bg-muted/50 p-2 text-muted-foreground">{icon}</div>
        <div className="space-y-1">
          <Label htmlFor={id} className="text-sm font-medium">
            {label}
          </Label>
          <p className="text-sm text-muted-foreground">{description}</p>
        </div>
      </div>
      <Switch id={id} checked={checked} disabled={disabled} onCheckedChange={onCheckedChange} />
    </div>
  );
}
