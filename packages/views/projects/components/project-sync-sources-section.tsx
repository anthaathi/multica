"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Trash2, ExternalLink, RefreshCw } from "lucide-react";
import { api } from "@multica/core/api";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Label } from "@multica/ui/components/ui/label";
import { Badge } from "@multica/ui/components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
  DialogClose,
} from "@multica/ui/components/ui/dialog";
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
import {
  syncSourcesOptions,
  useCreateSyncSource,
  useUpdateSyncSource,
  useDeleteSyncSource,
  remoteContainersOptions,
  jiraConnectionsOptions,
} from "@multica/core/issue-sync";
import { githubInstallationsOptions } from "@multica/core/github";
import { gitlabConnectionsOptions } from "@multica/core/gitlab";
import type { SyncProvider, IssueSyncSource } from "@multica/core/types";
import { useT } from "../../i18n";
import { GitHubMark } from "../../settings/components/github-mark";
import { GitLabMark } from "../../settings/components/gitlab-mark";
import { JiraMark } from "../../settings/components/jira-mark";

// Project sidebar section listing attached issue-sync sources.
//
// Mirrors project-resources-section.tsx in shape: a header with an add button,
// a list of rows, and an add-flow dialog. Each row surfaces the sync status
// (backfill_status), an enable toggle, a status-mapping editor, and remove.
// All server state flows through React Query hooks from @multica/core; this
// component holds no Zustand stores.

const PROVIDERS: SyncProvider[] = ["github", "gitlab", "jira"];



function ProviderGlyph({ provider, className }: { provider: string; className?: string }) {
  switch (provider) {
    case "github":
      return <GitHubMark className={className} />;
    case "gitlab":
      return <GitLabMark className={className} />;
    case "jira":
      return <JiraMark className={className} />;
    default:
      return null;
  }
}

export function ProjectSyncSourcesSection({ projectId }: { projectId: string }) {
  const { t } = useT("projects");
  const wsId = useWorkspaceId();
  const { data: sources = [], isLoading } = useQuery(
    syncSourcesOptions(wsId, projectId),
  );
  const [addOpen, setAddOpen] = useState(false);
  const [syncingAll, setSyncingAll] = useState(false);
  const handleSyncAll = async () => {
    setSyncingAll(true);
    try {
      const res = await api.syncAllProjectIssues(projectId);
      toast.success(t(($) => $.sync_sources.sync_all_done, { count: res.count }));
    } catch {
      toast.error(t(($) => $.sync_sources.sync_all_failed));
    } finally {
      setSyncingAll(false);
    }
  };

  return (
    <div>
      <div className="flex items-center justify-between">
        <button
          type="button"
          className="-ml-2 flex w-full items-center gap-1 rounded px-2 py-0.5 text-xs font-medium text-muted-foreground hover:text-foreground"
        >
          {t(($) => $.sync_sources.section_title)}
        </button>
        <div className="flex items-center gap-1">
          <Button variant="ghost" size="icon-sm" onClick={handleSyncAll} disabled={syncingAll || sources.length === 0} title={t(($) => $.sync_sources.sync_all)}>
            <RefreshCw className={`h-3.5 w-3.5 ${syncingAll ? "animate-spin" : ""}`} />
          </Button>
          <Button variant="ghost" size="icon-sm" onClick={() => setAddOpen(true)} title={t(($) => $.sync_sources.add)}>
            <Plus className="h-3.5 w-3.5" />
          </Button>
        </div>
      </div>

      <div className="mt-1 space-y-2">
        {isLoading ? (
          <Skeleton className="h-8 w-full" />
        ) : sources.length === 0 ? (
          <p className="px-2 text-xs text-muted-foreground">
            {t(($) => $.sync_sources.empty)}
          </p>
        ) : (
          sources.map((s) => (
            <SyncSourceRow key={s.id} wsId={wsId} projectId={projectId} source={s} />
          ))
        )}
      </div>

      <AddSyncSourceDialog
        wsId={wsId}
        projectId={projectId}
        open={addOpen}
        onOpenChange={setAddOpen}
      />
    </div>
  );
}

function SyncSourceRow({
  wsId,
  projectId,
  source,
}: {
  wsId: string;
  projectId: string;
  source: IssueSyncSource;
}) {
  const { t } = useT("projects");
  const updateMut = useUpdateSyncSource(wsId, projectId);
  const deleteMut = useDeleteSyncSource(wsId, projectId);
  const [removeTarget, setRemoveTarget] = useState(false);

  const backfill = source.backfill_status;
  const backfillLabel = (() => {
    switch (backfill) {
      case "running":
        return t(($) => $.sync_sources.backfill_running);
      case "done":
        return t(($) => $.sync_sources.backfill_completed);
      case "failed":
        return t(($) => $.sync_sources.backfill_failed);
      case "pending":
        return t(($) => $.sync_sources.backfill_idle);
      default:
        return t(($) => $.sync_sources.backfill_unknown);
    }
  })();

  async function handleToggleEnabled(next: boolean) {
    try {
      await updateMut.mutateAsync({
        sourceId: source.id,
        data: { sync_enabled: next },
      });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.sync_sources.toast_update_failed));
    }
  }


  async function handleRemove() {
    try {
      await deleteMut.mutateAsync(source.id);
      toast.success(t(($) => $.sync_sources.toast_removed));
      setRemoveTarget(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.sync_sources.toast_remove_failed));
    }
  }

  return (
    <div className="flex items-center gap-2 rounded-md border px-2.5 py-1.5">
      <ProviderGlyph provider={source.provider} className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
      <span className="min-w-0 flex-1 truncate text-xs font-medium">
        {source.external_key}
      </span>
      <Badge variant="secondary" className="shrink-0 px-1.5 py-0 text-[10px] font-normal">
        {backfillLabel}
      </Badge>
      <Switch
        checked={source.sync_enabled}
        onCheckedChange={handleToggleEnabled}
        disabled={updateMut.isPending}
        className="scale-75"
      />
      <Button
        variant="ghost"
        size="sm"
        className="h-6 shrink-0 px-1.5 text-xs text-muted-foreground"
        onClick={() => setRemoveTarget(true)}
      >
        <Trash2 className="h-3 w-3" />
      </Button>
      <AlertDialog open={removeTarget} onOpenChange={setRemoveTarget}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.sync_sources.remove_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.sync_sources.remove_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.sync_sources.remove_confirm_cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleRemove}
              disabled={deleteMut.isPending}
              className="bg-destructive text-white hover:bg-destructive/90"
            >
              {t(($) => $.sync_sources.remove_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

    </div>
  );

}

// StatusMappingEditor removed — compact row layout doesn't surface it yet.

// Connection option normalized across providers so the picker render stays
// provider-agnostic. id is the connection_id to send on create.
interface ConnectionOption {
  id: string;
  label: string;
}

function AddSyncSourceDialog({
  wsId,
  projectId,
  open,
  onOpenChange,
}: {
  wsId: string;
  projectId: string;
  open: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const { t } = useT("projects");
  const createMut = useCreateSyncSource(wsId, projectId);

  const [provider, setProvider] = useState<SyncProvider | "">("");
  const [connectionId, setConnectionId] = useState("");
  const [containerKey, setContainerKey] = useState("");
  const [pushDefault, setPushDefault] = useState(false);

  // Provider-specific connection lists. Each query is gated on the chosen
  // provider so we don't fire all three on open.
  const gh = useQuery({
    ...githubInstallationsOptions(wsId),
    enabled: open && provider === "github",
  });
  const gl = useQuery({
    ...gitlabConnectionsOptions(wsId),
    enabled: open && provider === "gitlab",
  });
  const ji = useQuery({
    ...jiraConnectionsOptions(wsId),
    enabled: open && provider === "jira",
  });

  const connections: ConnectionOption[] = useMemo(() => {
    if (provider === "github")
      return (gh.data?.installations ?? []).map((i) => ({
        id: i.id,
        label: i.account_login,
      }));
    if (provider === "gitlab")
      return (gl.data?.connections ?? []).map((c) => ({
        id: c.id,
        label: c.gitlab_username || c.gitlab_base_url,
      }));
    if (provider === "jira")
      return (ji.data?.connections ?? []).map((c) => ({
        id: c.id,
        label: c.site_url || c.account_email,
      }));
    return [];
  }, [provider, gh.data, gl.data, ji.data]);

  // Remote containers for the selected provider+connection.
  const containers = useQuery({
    ...remoteContainersOptions(wsId, provider, connectionId || undefined),
    enabled: open && !!provider && !!connectionId,
  });

  const selectedContainer = (containers.data ?? []).find((c) => c.key === containerKey) ?? null;

  function reset() {
    setProvider("");
    setConnectionId("");
    setContainerKey("");
    setPushDefault(false);
  }

  async function handleSubmit() {
    if (!provider || !connectionId || !selectedContainer) return;
    try {
      await createMut.mutateAsync({
        provider,
        connection_id: connectionId,
        external_ref: selectedContainer.ref,
        push_default: pushDefault,
      });
      reset();
      onOpenChange(false);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.sync_sources.toast_create_failed));
    }
  }

  const canSubmit = !!provider && !!connectionId && !!selectedContainer && !createMut.isPending;

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        if (!v) reset();
        onOpenChange(v);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t(($) => $.sync_sources.dialog_title)}</DialogTitle>
        </DialogHeader>

        <div className="space-y-3">
          {/* Provider */}
          <div className="space-y-1">
            <Label className="text-xs">{t(($) => $.sync_sources.step_provider)}</Label>
            <Select
              value={provider}
              onValueChange={(next) => {
                setProvider((next as SyncProvider) ?? "");
                setConnectionId("");
                setContainerKey("");
              }}
            >
              <SelectTrigger className="w-full">
                <SelectValue placeholder={t(($) => $.sync_sources.step_provider)} />
              </SelectTrigger>
              <SelectContent>
                {PROVIDERS.map((p) => (
                  <SelectItem key={p} value={p}>
                    {t(($) => $.sync_sources[`provider_${p}` as keyof typeof $.sync_sources] as string)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {/* Connection */}
          {provider && (
            <div className="space-y-1">
              <Label className="text-xs">{t(($) => $.sync_sources.step_connection)}</Label>
              {connections.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.sync_sources.connection_none)}
                </p>
              ) : (
                <Select
                  value={connectionId}
                  onValueChange={(next) => {
                    setConnectionId(next ?? "");
                    setContainerKey("");
                  }}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder={t(($) => $.sync_sources.connection_placeholder)} />
                  </SelectTrigger>
                  <SelectContent>
                    {connections.map((c) => (
                      <SelectItem key={c.id} value={c.id}>
                        {c.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </div>
          )}

          {/* Remote container */}
          {provider && connectionId && (
            <div className="space-y-1">
              <Label className="text-xs">{t(($) => $.sync_sources.step_container)}</Label>
              <Select value={containerKey} onValueChange={(next) => setContainerKey(next ?? "")}>
                <SelectTrigger className="w-full">
                  <SelectValue placeholder={t(($) => $.sync_sources.container_placeholder)} />
                  {selectedContainer?.url && (
                    <a
                      href={selectedContainer.url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="ml-auto text-muted-foreground hover:text-foreground"
                      onClick={(e) => e.stopPropagation()}
                    >
                      <ExternalLink className="h-3.5 w-3.5" />
                    </a>
                  )}
                </SelectTrigger>
                <SelectContent>
                  {containers.isLoading ? (
                    <div className="px-2 py-1 text-xs text-muted-foreground">
                      {t(($) => $.sync_sources.container_loading)}
                    </div>
                  ) : (containers.data ?? []).length === 0 ? (
                    <div className="px-2 py-1 text-xs text-muted-foreground">
                      {t(($) => $.sync_sources.container_empty)}
                    </div>
                  ) : (
                    (containers.data ?? []).map((c) => (
                      <SelectItem key={c.key} value={c.key}>
                        {c.name || c.key}
                      </SelectItem>
                    ))
                  )}
                </SelectContent>
              </Select>
            </div>
          )}

          {/* Push default */}
          {provider && connectionId && (
            <label className="flex items-center justify-between rounded-md border px-3 py-2">
              <span className="text-xs">
                <span className="font-medium">{t(($) => $.sync_sources.push_default)}</span>
                <span className="ml-2 text-muted-foreground">
                  {t(($) => $.sync_sources.push_default_hint)}
                </span>
              </span>
              <Switch checked={pushDefault} onCheckedChange={setPushDefault} />
            </label>
          )}
        </div>

        <DialogFooter>
          <DialogClose render={<Button type="button" variant="outline" />}>
            {t(($) => $.sync_sources.cancel)}
          </DialogClose>
          <Button onClick={handleSubmit} disabled={!canSubmit}>
            {createMut.isPending
              ? t(($) => $.sync_sources.submitting)
              : t(($) => $.sync_sources.submit)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
