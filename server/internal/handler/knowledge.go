package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/knowledge/sources"
	"github.com/multica-ai/multica/server/internal/logger"
)

type knowledgeSourceResponse struct {
	ID           string  `json:"id"`
	WorkspaceID  string  `json:"workspace_id"`
	SourceType   string  `json:"source_type"`
	DisplayName  string  `json:"display_name"`
	SyncStatus   string  `json:"sync_status"`
	SyncError    *string `json:"sync_error"`
	LastSyncedAt *string `json:"last_synced_at"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type createKnowledgeSourceRequest struct {
	SourceType  string          `json:"source_type"`
	DisplayName string          `json:"display_name"`
	Config      json.RawMessage `json:"config"`
}

func (h *Handler) ListKnowledgeSources(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT id, workspace_id, source_type, display_name, sync_status, sync_error, last_synced_at, created_at, updated_at
		FROM knowledge_sources
		WHERE workspace_id = $1
		ORDER BY created_at DESC
	`, parseUUID(workspaceID))
	if err != nil {
		slog.Warn("ListKnowledgeSources query failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list knowledge sources")
		return
	}
	defer rows.Close()

	var sources []knowledgeSourceResponse
	for rows.Next() {
		var s knowledgeSourceResponse
		var id, wsID pgtype.UUID
		var lastSyncedAt, createdAt, updatedAt pgtype.Timestamptz
		var syncError pgtype.Text
		if err := rows.Scan(&id, &wsID, &s.SourceType, &s.DisplayName, &s.SyncStatus, &syncError, &lastSyncedAt, &createdAt, &updatedAt); err != nil {
			slog.Warn("ListKnowledgeSources scan failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to read knowledge sources")
			return
		}
		s.ID = uuidToString(id)
		s.WorkspaceID = uuidToString(wsID)
		s.SyncError = textToPtr(syncError)
		s.LastSyncedAt = timestampToPtr(lastSyncedAt)
		s.CreatedAt = timestampToString(createdAt)
		s.UpdatedAt = timestampToString(updatedAt)
		sources = append(sources, s)
	}

	if sources == nil {
		sources = []knowledgeSourceResponse{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (h *Handler) CreateKnowledgeSource(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}

	var req createKnowledgeSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceType == "" {
		writeError(w, http.StatusBadRequest, "source_type is required")
		return
	}
	if req.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}

	var id pgtype.UUID
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO knowledge_sources (workspace_id, source_type, display_name, config)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, parseUUID(workspaceID), req.SourceType, req.DisplayName, req.Config).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a knowledge source with that name already exists")
			return
		}
		slog.Warn("CreateKnowledgeSource failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create knowledge source")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": uuidToString(id)})
}

func (h *Handler) DeleteKnowledgeSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	workspaceID := h.resolveWorkspaceID(r)

	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	srcUUID, ok := parseUUIDOrBadRequest(w, sourceID, "source id")
	if !ok {
		return
	}

	result, err := h.DB.Exec(r.Context(), `
		DELETE FROM knowledge_sources WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID)
	if err != nil {
		slog.Warn("DeleteKnowledgeSource failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to delete knowledge source")
		return
	}
	if result.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "knowledge source not found")
		return
	}

	if h.KnowledgeManager != nil {
		if err := h.KnowledgeManager.DropCollection(r.Context(), workspaceID); err != nil {
			slog.Warn("KnowledgeManager.DropCollection failed", "workspace_id", workspaceID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) SyncKnowledgeSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	workspaceID := h.resolveWorkspaceID(r)

	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	srcUUID, ok := parseUUIDOrBadRequest(w, sourceID, "source id")
	if !ok {
		return
	}

	var sourceType, configJSON string
	err := h.DB.QueryRow(r.Context(), `
		SELECT source_type, config::text FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &configJSON)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "knowledge source not found")
			return
		}
		slog.Warn("SyncKnowledgeSource lookup failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge source")
		return
	}

	if h.KnowledgeManager == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge search is not available (Qdrant not configured)")
		return
	}

	// Update status to syncing
	_, err = h.DB.Exec(r.Context(), `
		UPDATE knowledge_sources SET sync_status = 'syncing', sync_error = NULL WHERE id = $1
	`, srcUUID)
	if err != nil {
		slog.Warn("SyncKnowledgeSource status update failed", append(logger.RequestAttrs(r), "error", err)...)
	}

	// Run sync in background
	go func() {
		ctx := r.Context()
		connector := sources.NewConnector(knowledge.SourceType(sourceType))
		if connector == nil {
			slog.Warn("SyncKnowledgeSource: unknown source type", "source_type", sourceType, "source_id", sourceID)
			h.DB.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = 'unknown source type' WHERE id = $1`, srcUUID)
			return
		}
		chunks, err := connector.Fetch(workspaceID, configJSON)
		if err != nil {
			slog.Warn("SyncKnowledgeSource fetch failed", "source_id", sourceID, "error", err)
			h.DB.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = $1 WHERE id = $2`, err.Error(), srcUUID)
			return
		}
		if err := h.KnowledgeManager.IndexChunks(ctx, workspaceID, chunks); err != nil {
			slog.Warn("SyncKnowledgeSource index failed", "source_id", sourceID, "error", err)
			h.DB.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = $1 WHERE id = $2`, err.Error(), srcUUID)
			return
		}
		h.DB.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'ready', last_synced_at = now() WHERE id = $1`, srcUUID)
	}()

	writeJSON(w, http.StatusOK, map[string]string{"status": "syncing"})
}

func (h *Handler) SearchKnowledge(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}

	var req knowledge.SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.WorkspaceID = workspaceID
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	if h.KnowledgeManager == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge search is not available (Qdrant not configured)")
		return
	}

	results, err := h.KnowledgeManager.Search(r.Context(), req)
	if err != nil {
		slog.Warn("KnowledgeManager.Search failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	if results == nil {
		results = []knowledge.SearchResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
