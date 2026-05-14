package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// mergeKnowledgeMCP merges the knowledge MCP server config into an existing
// agent-level MCP config using the stdio transport (Cursor/CLaude expected format).
// The MULTICA_KNOWLEDGE_MCP_URL env var is used as a feature toggle — when set,
// the knowledge tool is injected. The value is the backend API base URL.
func mergeKnowledgeMCP(existing json.RawMessage, baseURL, workspaceID string) json.RawMessage {
	// Find the script: look alongside the multica binary directory
	scriptPath := resolveKnowledgeScript()

	ks := map[string]interface{}{
		"command": "node",
		"args":    []string{scriptPath},
		"env": map[string]string{
			"MULTICA_API_URL":        baseURL,
			"MULTICA_WORKSPACE_SLUG": workspaceID,
		},
	}

	// Try to parse existing config as {"mcpServers": {...}}
	servers := map[string]interface{}{}
	if len(existing) > 0 {
		var shaped map[string]interface{}
		if err := json.Unmarshal(existing, &shaped); err == nil {
			if s, ok := shaped["mcpServers"]; ok {
				if sm, ok := s.(map[string]interface{}); ok {
					servers = sm
				}
			}
		}
	}

	servers["multica-knowledge"] = ks

	result, err := json.Marshal(map[string]interface{}{"mcpServers": servers})
	if err != nil {
		return existing
	}
	return result
}

func resolveKnowledgeScript() string {
	// Look in the same directory as the multica binary, then in ~/dev
	paths := []string{
		filepath.Join(filepath.Dir(os.Args[0]), "tools", "mcp-knowledge", "index.mjs"),
		filepath.Join(os.Getenv("HOME"), "dev", "multica-source", "tools", "mcp-knowledge", "index.mjs"),
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fallback: relative to the cloned source
	return filepath.Join(os.Getenv("HOME"), "dev", "multica-source", "tools", "mcp-knowledge", "index.mjs")
}
