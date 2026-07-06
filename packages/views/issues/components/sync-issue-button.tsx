"use client";

import { useState } from "react";
import { RefreshCw } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import { toast } from "sonner";
import { useT } from "../../i18n";

// A compact button that manually pushes this issue to its project's default
// sync source (Jira/GitLab/GitHub). Shows a spinner while the push is queued,
// then a toast. The actual provider API call happens asynchronously in the
// outbox worker — this just enqueues it.
export function SyncIssueButton({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const [loading, setLoading] = useState(false);

  const handleSync = async () => {
    setLoading(true);
    try {
      const res = await api.syncIssue(issueId);
      toast.success(t(($) => $.sync.push_queued, { provider: res.provider }));
    } catch (e) {
      const msg = e instanceof Error ? e.message : "failed";
      toast.error(t(($) => $.sync.push_failed, { error: msg }));
    } finally {
      setLoading(false);
    }
  };

  return (
    <Button
      variant="outline"
      size="sm"
      className="h-6 gap-1 px-2 text-xs"
      onClick={handleSync}
      disabled={loading}
    >
      <RefreshCw className={`h-3 w-3 ${loading ? "animate-spin" : ""}`} />
      {loading ? t(($) => $.sync.syncing) : t(($) => $.sync.sync_to_provider)}
    </Button>
  );
}
