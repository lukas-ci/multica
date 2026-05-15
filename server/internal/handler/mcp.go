package handler

import (
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/middleware"
)

type mcpRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	Result interface{} `json:"result,omitempty"`
	Error  *mcpError   `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

func (h *Handler) HandleMCP(w http.ResponseWriter, r *http.Request) {
	if h.KnowledgeManager == nil {
		writeError(w, http.StatusServiceUnavailable, "Knowledge manager not available")
		return
	}

	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32700, Message: "Parse error"}})
		return
	}

	switch req.Method {
	case "tools/list":
		h.mcpListTools(w, r)
	case "tools/call":
		h.mcpCallTool(w, r, req)
	default:
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32601, Message: "Method not found"}})
	}
}

func (h *Handler) mcpListTools(w http.ResponseWriter, r *http.Request) {
	tools := []mcpTool{
		{
			Name:        "knowledge_search",
			Description: "Search indexed knowledge sources (Confluence, GitHub, etc.) linked to the current workspace. Use this when you need context from documentation, runbooks, or code references.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "Search query"},
					"limit": map[string]interface{}{"type": "integer", "description": "Max results (default 10)"},
				},
				"required": []string{"query"},
			},
		},
	}
	writeJSON(w, http.StatusOK, mcpResponse{Result: map[string]interface{}{"tools": tools}})
}

func (h *Handler) mcpCallTool(w http.ResponseWriter, r *http.Request, req mcpRequest) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32602, Message: "Invalid params"}})
		return
	}

	if params.Name != "knowledge_search" {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32602, Message: "Unknown tool: " + params.Name}})
		return
	}

	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	query, _ := params.Arguments["query"].(string)
	limit := 10
	if l, ok := params.Arguments["limit"].(float64); ok {
		limit = int(l)
	}

	if query == "" {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32602, Message: "query is required"}})
		return
	}
	if status := h.reconcileReadyKnowledgeIndex(r.Context(), workspaceID); status.Unhealthy {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32603, Message: status.ErrorMessage}})
		return
	}

	results, err := h.KnowledgeManager.Search(r.Context(), knowledge.SearchRequest{
		WorkspaceID: workspaceID,
		Query:       query,
		Limit:       limit,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, mcpResponse{Error: &mcpError{Code: -32603, Message: err.Error()}})
		return
	}

	writeJSON(w, http.StatusOK, mcpResponse{Result: map[string]interface{}{"content": results}})
}
