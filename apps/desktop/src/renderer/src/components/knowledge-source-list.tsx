import { BookOpen, Globe, MessageSquare, HardDrive, Trash2, RefreshCw } from "lucide-react";

interface KnowledgeSource {
  id: string;
  source_type: string;
  display_name: string;
  sync_status: string;
  last_synced_at: string | null;
}

const sourceIcons: Record<string, typeof BookOpen> = {
  confluence: BookOpen,
  github: Globe,
  slack: MessageSquare,
  google_drive: HardDrive,
};

const sourceLabels: Record<string, string> = {
  confluence: "Confluence",
  github: "GitHub",
  slack: "Slack",
  google_drive: "Google Drive",
};

export function KnowledgeSourceList({ sources, onRemove }: { sources: KnowledgeSource[]; onRemove: () => void }) {
  if (sources.length === 0) {
    return (
      <div className="mt-12 text-center">
        <BookOpen className="w-12 h-12 mx-auto text-gray-300 mb-4" />
        <p className="text-lg text-gray-500">No sources linked yet.</p>
        <p className="text-sm text-gray-400 mt-1">
          Link your first knowledge source to get started.
        </p>
      </div>
    );
  }

  return (
    <div className="mt-6 space-y-2">
      {sources.map(source => {
        const Icon = sourceIcons[source.source_type] || BookOpen;
        return (
          <div key={source.id} className="flex items-center justify-between p-4 border rounded-lg hover:bg-gray-50 transition-colors">
            <div className="flex items-center gap-3">
              <Icon className="w-5 h-5 text-gray-500" />
              <div>
                <span className="font-medium">{source.display_name}</span>
                <span className="ml-2 text-xs px-2 py-0.5 bg-gray-100 rounded-full text-gray-600">
                  {sourceLabels[source.source_type] || source.source_type}
                </span>
              </div>
              <div className="flex items-center gap-2 ml-4">
                {source.sync_status === "syncing" && (
                  <span className="flex items-center gap-1 text-xs text-blue-600">
                    <RefreshCw className="w-3 h-3 animate-spin" />
                    Syncing...
                  </span>
                )}
                {source.sync_status === "error" && (
                  <span className="text-xs text-red-600">Error</span>
                )}
                {source.sync_status === "ready" && source.last_synced_at && (
                  <span className="text-xs text-gray-400">
                    Synced {new Date(source.last_synced_at).toLocaleDateString()}
                  </span>
                )}
              </div>
            </div>
            <button
              onClick={async () => {
                await fetch(`/api/knowledge/sources/${source.id}`, { method: "DELETE" });
                onRemove();
              }}
              className="p-2 text-gray-400 hover:text-red-600 transition-colors"
            >
              <Trash2 className="w-4 h-4" />
            </button>
          </div>
        );
      })}
    </div>
  );
}
