package handler

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/worker"
)

const knowledgeEmptyIndexError = "knowledge source marked ready but index collection is empty; re-sync required"

type knowledgeIndexHealth struct {
	Unhealthy    bool
	ErrorMessage string
}

type knowledgeSourceResponse struct {
	ID              string  `json:"id"`
	WorkspaceID     string  `json:"workspace_id"`
	SourceType      string  `json:"source_type"`
	DisplayName     string  `json:"display_name"`
	SyncStatus      string  `json:"sync_status"`
	SyncError       *string `json:"sync_error"`
	LastSyncedAt    *string `json:"last_synced_at"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	PagesFetched    int     `json:"pages_fetched"`
	TotalPages      *int    `json:"total_pages"`
	ResourcesFetched int    `json:"resources_fetched"`
	SyncKind        string  `json:"sync_kind"`
	Checkpoint      string  `json:"checkpoint"`
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
	h.reconcileReadyKnowledgeIndex(r.Context(), workspaceID)

	rows, err := h.DB.Query(r.Context(), `
		SELECT id, workspace_id, source_type, display_name, sync_status, sync_error, last_synced_at,
		       created_at, updated_at, pages_fetched, total_pages, resources_fetched, sync_kind, checkpoint
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
		var totalPages pgtype.Int4
		if err := rows.Scan(&id, &wsID, &s.SourceType, &s.DisplayName, &s.SyncStatus, &syncError, &lastSyncedAt,
			&createdAt, &updatedAt, &s.PagesFetched, &totalPages, &s.ResourcesFetched, &s.SyncKind, &s.Checkpoint); err != nil {
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
		if totalPages.Valid {
			v := int(totalPages.Int32)
			s.TotalPages = &v
		}
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

	sourceID := uuidToString(id)

	if h.WorkerManager != nil {
		if err := h.WorkerManager.Enqueue(context.Background(), worker.SyncKnowledgeArgs{
			SourceID:    sourceID,
			WorkspaceID: workspaceID,
			SyncKind:    string(knowledge.SyncFull),
		}); err != nil {
			slog.Warn("CreateKnowledgeSource: failed to enqueue sync job", "error", err)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": sourceID})
}

func (h *Handler) DeleteKnowledgeSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	workspaceID := h.resolveWorkspaceID(r)

	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}

	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	srcUUID, ok := parseUUIDOrBadRequest(w, sourceID, "source id")
	if !ok {
		return
	}

	// Acquire advisory lock for this source
	lockKey := hashSourceID(sourceID)
	if _, err := h.DB.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		slog.Warn("DeleteKnowledgeSource: advisory lock failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to acquire lock")
		return
	}

	// Load source row with config and active generation
	var configJSON json.RawMessage
	var activeIndexGeneration int
	var syncRunID pgtype.UUID
	err := h.DB.QueryRow(r.Context(), `
		SELECT config, active_index_generation, sync_run_id
		FROM knowledge_sources WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&configJSON, &activeIndexGeneration, &syncRunID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "knowledge source not found")
			return
		}
		slog.Warn("DeleteKnowledgeSource lookup failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge source")
		return
	}

	// Parse config to extract source_id (space_key for Confluence)
	var sourceIDInQdrant string
	var cfg struct {
		SpaceKey string `json:"space_key"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err == nil && cfg.SpaceKey != "" {
		sourceIDInQdrant = cfg.SpaceKey
	}

	// Delete DB row
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

	// Delete Qdrant points for this source — generation-specific then catch-all
	if h.KnowledgeManager != nil && sourceIDInQdrant != "" {
		if err := h.KnowledgeManager.DeleteSourcePointsByGeneration(r.Context(), workspaceID, sourceIDInQdrant, activeIndexGeneration); err != nil {
			slog.Warn("DeleteKnowledgeSource: DeleteSourcePointsByGeneration failed",
				"workspace_id", workspaceID, "source_id", sourceIDInQdrant, "generation", activeIndexGeneration, "error", err)
		}
		// Catch-all: delete any remaining points from earlier generations
		if err := h.KnowledgeManager.DeleteSourcePoints(r.Context(), workspaceID, sourceIDInQdrant); err != nil {
			slog.Warn("DeleteKnowledgeSource: DeleteSourcePoints catch-all failed",
				"workspace_id", workspaceID, "source_id", sourceIDInQdrant, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) SyncKnowledgeSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "id")
	workspaceID := h.resolveWorkspaceID(r)

	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}

	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	srcUUID, ok := parseUUIDOrBadRequest(w, sourceID, "source id")
	if !ok {
		return
	}

	if h.KnowledgeManager == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge search is not available (Qdrant not configured)")
		return
	}
	if h.WorkerManager == nil {
		writeError(w, http.StatusInternalServerError, "sync queue is not available")
		return
	}

	var sourceType string
	var syncStatus string
	err := h.DB.QueryRow(r.Context(), `
		SELECT source_type, sync_status FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &syncStatus)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "knowledge source not found")
			return
		}
		slog.Warn("SyncKnowledgeSource lookup failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge source")
		return
	}

	if syncStatus == "syncing" {
		writeError(w, http.StatusConflict, "sync already in progress")
		return
	}

	// Acquire advisory lock for this source to prevent concurrent sync/delete
	lockKey := hashSourceID(sourceID)
	if _, err := h.DB.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		slog.Warn("SyncKnowledgeSource: advisory lock failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to acquire sync lock")
		return
	}

	// Create a new sync run (reset state + bump generation + set run ID)
	syncRunID := uuid.NewString()
	if _, err := h.DB.Exec(r.Context(), `
		UPDATE knowledge_sources
		SET checkpoint = '', pages_fetched = 0, total_pages = NULL, sync_kind = 'full',
		    sync_run_id = $1, sync_started_at = now(), sync_heartbeat_at = now(),
		    sync_index_generation = active_index_generation + 1
		WHERE id = $2
	`, syncRunID, srcUUID); err != nil {
		slog.Warn("SyncKnowledgeSource: failed to initialize sync run", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to start sync")
		return
	}

	if err := h.WorkerManager.Enqueue(context.Background(), worker.SyncKnowledgeArgs{
		SourceID:    sourceID,
		WorkspaceID: workspaceID,
		SyncKind:    string(knowledge.SyncFull),
		SyncRunID:   syncRunID,
	}); err != nil {
		slog.Warn("SyncKnowledgeSource: failed to enqueue sync job", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start sync")
		return
	}

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
	if status := h.reconcileReadyKnowledgeIndex(r.Context(), workspaceID); status.Unhealthy {
		writeError(w, http.StatusServiceUnavailable, status.ErrorMessage)
		return
	}

	// Build generation filter from ready non-legacy sources
	genRows, err := h.DB.Query(r.Context(), `
		SELECT id, active_index_generation, legacy_index_mode
		FROM knowledge_sources
		WHERE workspace_id = $1 AND sync_status = 'ready'
	`, parseUUID(workspaceID))
	if err == nil {
		defer genRows.Close()
		for genRows.Next() {
			var id pgtype.UUID
			var gen int
			var legacy bool
			if err := genRows.Scan(&id, &gen, &legacy); err == nil && !legacy && id.Valid {
				if req.IndexGenerations == nil {
					req.IndexGenerations = make(map[string]int)
				}
				req.IndexGenerations[uuidToString(id)] = gen
			}
		}
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

func knowledgeIndexHealthStatus(readySourceCount int, indexedPointCount uint64, countErr error) knowledgeIndexHealth {
	if countErr != nil || readySourceCount == 0 || indexedPointCount > 0 {
		return knowledgeIndexHealth{}
	}
	return knowledgeIndexHealth{Unhealthy: true, ErrorMessage: knowledgeEmptyIndexError}
}

func hashSourceID(id string) int64 {
	h := sha1.New()
	h.Write([]byte(id))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func (h *Handler) reconcileReadyKnowledgeIndex(ctx context.Context, workspaceID string) knowledgeIndexHealth {
	if h.KnowledgeManager == nil || workspaceID == "" {
		return knowledgeIndexHealth{}
	}
	var readyCount int
	if err := h.DB.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_sources
		WHERE workspace_id = $1 AND sync_status = 'ready' AND legacy_index_mode = false
	`, parseUUID(workspaceID)).Scan(&readyCount); err != nil {
		slog.Warn("knowledge ready health query failed", "workspace_id", workspaceID, "error", err)
		return knowledgeIndexHealth{}
	}
	count, err := h.KnowledgeManager.CountIndexedChunks(ctx, workspaceID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("knowledge index count failed", "workspace_id", workspaceID, "error", err)
		}
		return knowledgeIndexHealthStatus(readyCount, 0, err)
	}
	status := knowledgeIndexHealthStatus(readyCount, count, nil)
	if !status.Unhealthy {
		return status
	}
	if _, err := h.DB.Exec(ctx, `
		UPDATE knowledge_sources
		SET sync_status = 'error', sync_error = $2
		WHERE workspace_id = $1 AND sync_status = 'ready'
	`, parseUUID(workspaceID), status.ErrorMessage); err != nil {
		slog.Warn("knowledge ready health update failed", "workspace_id", workspaceID, "error", err)
	}
	return status
}
