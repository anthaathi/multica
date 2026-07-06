"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Label } from "@multica/ui/components/ui/label";
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
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { jiraConnectionKeys } from "@multica/core/issue-sync";
import { jiraConnectionsOptions } from "@multica/core/issue-sync";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useT } from "../../i18n";
import { JiraMark } from "./jira-mark";

// Jira settings tab. Mirrors gitlab-tab's connection section: a Connect button
// that opens the backend OAuth URL, a list of connected accounts with
// disconnect, and a "not configured" surface driven by the `configured` flag.
// Unlike GitHub/GitLab there are no workspace-level feature toggles — Jira
// sync is configured per-project via sync sources (see
// project-sync-sources-section).
export function JiraTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canView = !!currentMember;

  const { data: connectionData } = useQuery({
    ...jiraConnectionsOptions(wsId),
    enabled: !!wsId && canView,
  });
  const connections = connectionData?.connections ?? [];
  const configured = connectionData?.configured ?? false;
  const canManage = connectionData?.can_manage === true;
  const connected = connections.length > 0;
  const primaryConnection = connections[0] ?? null;

  const [connecting, setConnecting] = useState(false);
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleConnect() {
    setConnecting(true);
    try {
      const resp = await api.getJiraConnectURL(wsId);
      if (!resp.configured || !resp.url) {
        toast.error(t(($) => $.jira.toast_not_configured));
        return;
      }
      window.open(resp.url, "_blank", "noopener");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.jira.toast_open_failed));
    } finally {
      setConnecting(false);
    }
  }

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteJiraConnection(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: jiraConnectionKeys.list(wsId) });
      toast.success(t(($) => $.jira.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.jira.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  // canView is false until the member list resolves; render nothing rather
  // than flash an empty "not connected" state for the current user.
  if (!canView) return null;

  return (
    <div className="space-y-8">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">{t(($) => $.jira.page_description)}</p>
      </section>

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">{t(($) => $.jira.section_connection)}</h2>
        <Card>
          <CardContent className="space-y-4">
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-start gap-3">
                <JiraMark className="mt-0.5 h-6 w-6 shrink-0" />
                <div className="space-y-1">
                  <Label className="text-sm font-medium">
                    {t(($) => $.jira.connection_title)}
                  </Label>
                  {connected ? (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.jira.connected_to, {
                        site: primaryConnection?.site_url ?? "",
                        email: primaryConnection?.account_email ?? "",
                      })}
                    </p>
                  ) : canManage ? (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.jira.connection_description)}
                    </p>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      {t(($) => $.jira.contact_admin_to_connect)}
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
                      {t(($) => $.jira.disconnect)}
                    </Button>
                  ) : (
                    <Button
                      size="sm"
                      onClick={handleConnect}
                      disabled={connecting || !configured}
                      title={!configured ? t(($) => $.jira.connect_disabled_tooltip) : undefined}
                    >
                      {connecting
                        ? t(($) => $.jira.connect_opening)
                        : t(($) => $.jira.connect_jira)}
                    </Button>
                  )}
                </div>
              )}
            </div>

            {canManage && !configured && (
              <p className="text-xs text-muted-foreground">{t(($) => $.jira.not_configured)}</p>
            )}

            {!canManage && connected && (
              <p className="text-xs text-muted-foreground">{t(($) => $.jira.read_only_hint)}</p>
            )}

            {connected && primaryConnection?.site_url && (
              <a
                href={primaryConnection.site_url}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
              >
                <ExternalLink className="h-3 w-3" />
                {primaryConnection.site_url}
              </a>
            )}
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
            <AlertDialogTitle>{t(($) => $.jira.disconnect_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.jira.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.jira.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.jira.disconnecting)
                : t(($) => $.jira.disconnect_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
