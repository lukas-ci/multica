import { useState, useEffect } from "react";
import { KnowledgeSourceList } from "../components/knowledge-source-list";
import { KnowledgeLinkModal } from "../components/knowledge-link-modal";
import { KnowledgeSearchBar } from "../components/knowledge-search-bar";
import { Plus } from "lucide-react";

interface KnowledgeSource {
  id: string;
  source_type: string;
  display_name: string;
  sync_status: string;
  last_synced_at: string | null;
}

export function KnowledgePage() {
  const [sources, setSources] = useState<KnowledgeSource[]>([]);
  const [showModal, setShowModal] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");

  useEffect(() => {
    fetch("/api/knowledge/sources")
      .then(r => r.json())
      .then(setSources)
      .catch(() => setSources([]));
  }, []);

  const refreshSources = () => {
    fetch("/api/knowledge/sources")
      .then(r => r.json())
      .then(setSources)
      .catch(() => {});
  };

  return (
    <div className="p-6 max-w-4xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold">Knowledge</h1>
        <button
          onClick={() => setShowModal(true)}
          className="flex items-center gap-2 px-4 py-2 bg-black text-white rounded-lg hover:bg-gray-800 transition-colors"
        >
          <Plus className="w-4 h-4" />
          Link Source
        </button>
      </div>

      <KnowledgeSearchBar onSearch={setSearchQuery} />

      <KnowledgeSourceList sources={sources} onRemove={() => refreshSources()} />

      {showModal && (
        <KnowledgeLinkModal
          onClose={() => setShowModal(false)}
          onLinked={() => {
            refreshSources();
            setShowModal(false);
          }}
        />
      )}
    </div>
  );
}
