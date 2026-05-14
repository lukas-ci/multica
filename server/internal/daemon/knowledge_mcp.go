package daemon

import (
	"encoding/json"
)

// mergeKnowledgeMCP merges the knowledge MCP server config into an existing
// agent-level MCP config. If the existing config is empty or invalid, it
// creates a fresh one with just the knowledge server.
func mergeKnowledgeMCP(existing json.RawMessage, baseURL, workspaceID string) json.RawMessage {
	ks := map[string]interface{}{
		"url": baseURL,
		"headers": map[string]string{
			"X-Workspace-Slug": workspaceID,
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
