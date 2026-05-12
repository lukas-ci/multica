import { useState } from "react";
import { X } from "lucide-react";

interface Props {
  onClose: () => void;
  onLinked: () => void;
}

export function KnowledgeLinkModal({ onClose, onLinked }: Props) {
  const [sourceType, setSourceType] = useState("confluence");
  const [baseURL, setBaseURL] = useState("");
  const [token, setToken] = useState("");
  const [spaceKey, setSpaceKey] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleLink = async () => {
    setError("");
    setLoading(true);
    try {
      const res = await fetch("/api/knowledge/sources", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          source_type: sourceType,
          display_name: spaceKey || baseURL,
          config: { base_url: baseURL, token, space_key: spaceKey },
        }),
      });
      if (!res.ok) {
        const err = await res.json();
        setError(err.error || "Failed to link source");
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
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" onClick={onClose}>
      <div className="bg-white rounded-xl p-6 w-[480px] shadow-xl" onClick={e => e.stopPropagation()}>
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-lg font-bold">Link Knowledge Source</h2>
          <button onClick={onClose} className="p-1 text-gray-400 hover:text-gray-600">
            <X className="w-5 h-5" />
          </button>
        </div>

        <label className="block text-sm font-medium mb-1 text-gray-700">Type</label>
        <select
          value={sourceType}
          onChange={e => setSourceType(e.target.value)}
          className="w-full border rounded-lg px-3 py-2 mb-4 text-sm"
        >
          <option value="confluence">Confluence</option>
          <option value="github" disabled>GitHub (coming soon)</option>
          <option value="slack" disabled>Slack (coming soon)</option>
        </select>

        <label className="block text-sm font-medium mb-1 text-gray-700">Base URL</label>
        <input
          value={baseURL}
          onChange={e => setBaseURL(e.target.value)}
          placeholder="https://your-domain.atlassian.net"
          className="w-full border rounded-lg px-3 py-2 mb-4 text-sm"
        />

        <label className="block text-sm font-medium mb-1 text-gray-700">API Token</label>
        <input
          value={token}
          onChange={e => setToken(e.target.value)}
          type="password"
          placeholder="Atlassian API token"
          className="w-full border rounded-lg px-3 py-2 mb-4 text-sm"
        />

        <label className="block text-sm font-medium mb-1 text-gray-700">Space Key</label>
        <input
          value={spaceKey}
          onChange={e => setSpaceKey(e.target.value)}
          placeholder="e.g. DOC, PROD"
          className="w-full border rounded-lg px-3 py-2 mb-4 text-sm"
        />

        {error && <p className="text-red-600 text-sm mb-4">{error}</p>}

        <div className="flex justify-end gap-3">
          <button onClick={onClose} className="px-4 py-2 text-sm text-gray-600 hover:text-gray-800">
            Cancel
          </button>
          <button
            onClick={handleLink}
            disabled={loading}
            className="px-4 py-2 text-sm bg-black text-white rounded-lg hover:bg-gray-800 disabled:opacity-50 transition-colors"
          >
            {loading ? "Verifying..." : "Verify & Link"}
          </button>
        </div>
      </div>
    </div>
  );
}
