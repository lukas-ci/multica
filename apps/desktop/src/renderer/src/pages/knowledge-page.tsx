"use client";

import { useState, useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { BookOpen, Plus, Search, Trash2, AlertCircle } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { PageHeader } from "@multica/views/layout";
import { api } from "@multica/core/api";

interface KnowledgeSource {
  id: string;
  source_type: string;
  display_name: string;
  sync_status: string;
  last_synced_at: string | null;
}

const sourceLabels: Record<string, string> = {
  confluence: "Confluence",
  github: "GitHub",
  slack: "Slack",
  google_drive: "Google Drive",
};

function EmptyState({ onLink }: { onLink: () => void }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center px-6 py-16 text-center">
      <div className="flex h-12 w-12 items-center justify-center rounded-full bg-muted">
        <BookOpen className="h-6 w-6 text-muted-foreground" />
      </div>
      <h2 className="mt-4 text-base font-semibold">No sources linked yet</h2>
      <p className="mt-1 max-w-md text-sm text-muted-foreground">
        Link your first knowledge source to get started. Confluence is
        supported now; GitHub and Slack are coming soon.
      </p>
      <Button type="button" onClick={onLink} size="sm" className="mt-5">
        <Plus className="h-3 w-3" />
        Link Source
      </Button>
    </div>
  );
}

function SourceRow({
  source,
  onRemove,
}: {
  source: KnowledgeSource;
  onRemove: (id: string) => void;
}) {
  return (
    <div className="flex items-center justify-between rounded-lg border bg-background px-4 py-3">
      <div className="flex items-center gap-3 min-w-0">
        <BookOpen className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0">
          <span className="text-sm font-medium">{source.display_name}</span>
          <span className="ml-2 inline-flex items-center rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground">
            {sourceLabels[source.source_type] || source.source_type}
          </span>
        </div>
        <div className="hidden sm:flex items-center gap-2">
          {source.sync_status === "syncing" && (
            <span className="text-xs text-blue-600">Syncing...</span>
          )}
          {source.sync_status === "error" && (
            <span className="text-xs text-destructive">Error</span>
          )}
          {source.sync_status === "ready" && source.last_synced_at && (
            <span className="text-xs text-muted-foreground">
              Synced {new Date(source.last_synced_at).toLocaleDateString()}
            </span>
          )}
        </div>
      </div>
      <button
        onClick={() => onRemove(source.id)}
        className="ml-2 shrink-0 rounded-md p-1.5 text-muted-foreground/60 transition-colors hover:bg-accent hover:text-foreground"
      >
        <Trash2 className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}

function LinkSourceModal({
  onClose,
  onLinked,
}: {
  onClose: () => void;
  onLinked: () => void;
}) {
  const [baseURL, setBaseURL] = useState("");
  const [token, setToken] = useState("");
  const [spaceKey, setSpaceKey] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleLink = async () => {
    setError("");
    setLoading(true);
    try {
      try {
        await api.request("/api/knowledge/sources", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            source_type: "confluence",
            display_name: spaceKey || baseURL,
            config: { base_url: baseURL, token, space_key: spaceKey },
          }),
        });
      } catch (e) {
        setError(e instanceof Error ? e.message : "Failed to link source");
        return;
      }
      onLinked();
    } catch {
      setError("Network error");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={onClose}
    >
      <div
        className="w-[480px] rounded-xl bg-background p-6 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="mb-4 text-lg font-semibold">Link Confluence Space</h2>

        <label className="mb-1 block text-sm font-medium text-foreground">
          Base URL
        </label>
        <input
          value={baseURL}
          onChange={(e) => setBaseURL(e.target.value)}
          placeholder="https://your-domain.atlassian.net"
          className="mb-4 w-full rounded-lg border bg-background px-3 py-2 text-sm"
        />

        <label className="mb-1 block text-sm font-medium text-foreground">
          API Token
        </label>
        <input
          value={token}
          onChange={(e) => setToken(e.target.value)}
          type="password"
          placeholder="Atlassian API token"
          className="mb-4 w-full rounded-lg border bg-background px-3 py-2 text-sm"
        />

        <label className="mb-1 block text-sm font-medium text-foreground">
          Space Key
        </label>
        <input
          value={spaceKey}
          onChange={(e) => setSpaceKey(e.target.value)}
          placeholder="e.g. DOC, PROD"
          className="mb-4 w-full rounded-lg border bg-background px-3 py-2 text-sm"
        />

        {error && (
          <p className="mb-4 text-sm text-destructive">{error}</p>
        )}

        <div className="flex justify-end gap-3">
          <Button variant="outline" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button size="sm" onClick={handleLink} disabled={loading}>
            {loading ? "Verifying..." : "Verify & Link"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function KnowledgePage() {
  const [sources, setSources] = useState<KnowledgeSource[]>([]);
  const [showModal, setShowModal] = useState(false);
  const [search, setSearch] = useState("");
  const [loading, setLoading] = useState(true);

  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchSources = (isPolling = false) => {
    if (!isPolling) setLoading(true);
    api.request<{sources: KnowledgeSource[]}>("/api/knowledge/sources")
      .then((data) => {
        const list = Array.isArray(data.sources) ? data.sources : [];
        setSources(list);
        const hasPending = list.some(s => s.sync_status === "pending" || s.sync_status === "syncing");
        if (hasPending && !intervalRef.current) {
          intervalRef.current = setInterval(() => fetchSources(true), 3000);
        } else if (!hasPending && intervalRef.current) {
          clearInterval(intervalRef.current);
          intervalRef.current = null;
        }
      })
      .catch(() => {
        if (!isPolling) setSources([]);
      })
      .finally(() => { if (!isPolling) setLoading(false); });
  };

  useEffect(() => {
    fetchSources();
    return () => { if (intervalRef.current) clearInterval(intervalRef.current); };
  }, []);

  const handleRemove = async (id: string) => {
    await api.request(`/api/knowledge/sources/${id}`, { method: "DELETE" });
    fetchSources();
  };

  const filtered = search.trim()
    ? sources.filter((s) =>
        s.display_name.toLowerCase().includes(search.toLowerCase()),
      )
    : sources;

  if (loading) {
    return (
      <div className="flex flex-1 min-h-0 flex-col">
        <PageHeader className="justify-between px-5">
          <div className="flex items-center gap-2">
            <BookOpen className="h-4 w-4 text-muted-foreground" />
            <h1 className="text-sm font-medium">Knowledge</h1>
          </div>
        </PageHeader>
        <div className="flex flex-1 min-h-0 flex-col gap-4 p-6">
          <Skeleton className="h-8 w-full max-w-xs rounded-md" />
          <Skeleton className="h-14 w-full rounded-md" />
          <Skeleton className="h-14 w-full rounded-md" />
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-1 min-h-0 flex-col">
      <PageHeader className="justify-between px-5">
        <div className="flex items-center gap-2">
          <BookOpen className="h-4 w-4 text-muted-foreground" />
          <h1 className="text-sm font-medium">Knowledge</h1>
          {sources.length > 0 && (
            <span className="font-mono text-xs tabular-nums text-muted-foreground/70">
              {sources.length}
            </span>
          )}
        </div>
        <Button type="button" size="sm" onClick={() => setShowModal(true)}>
          <Plus className="h-3 w-3" />
          Link Source
        </Button>
      </PageHeader>

      <div className="flex flex-1 min-h-0 flex-col gap-4 p-6">
        {sources.length > 0 && (
          <div className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
            <div className="relative">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Filter sources..."
                className="h-8 w-64 pl-8 text-sm"
              />
            </div>
          </div>
        )}

        {sources.length === 0 ? (
          <div className="flex flex-1 items-center justify-center">
            <EmptyState onLink={() => setShowModal(true)} />
          </div>
        ) : filtered.length === 0 ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 px-4 py-16 text-center text-muted-foreground">
            <Search className="h-8 w-8 text-muted-foreground/40" />
            <p className="text-sm">No matching sources</p>
          </div>
        ) : (
          <div className="flex flex-1 min-h-0 flex-col gap-2">
            {filtered.map((source) => (
              <SourceRow
                key={source.id}
                source={source}
                onRemove={handleRemove}
              />
            ))}
          </div>
        )}
      </div>

      {showModal && (
        <LinkSourceModal
          onClose={() => setShowModal(false)}
          onLinked={() => {
            fetchSources();
            setShowModal(false);
          }}
        />
      )}
    </div>
  );
}
