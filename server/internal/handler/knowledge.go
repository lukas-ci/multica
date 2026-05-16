package handler

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/logger"
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

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Warn("CreateKnowledgeSource: begin tx failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create knowledge source")
		return
	}
	defer tx.Rollback(r.Context())

	var id pgtype.UUID
	err = tx.QueryRow(r.Context(), `
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

	if err := h.WorkerManager.InitAndEnqueueRunTx(r.Context(), tx, sourceID, workspaceID, string(knowledge.SyncFull)); err != nil {
		slog.Warn("CreateKnowledgeSource: failed to init and enqueue sync run", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to start initial sync")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Warn("CreateKnowledgeSource: commit failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create knowledge source")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": sourceID})
}

func (h *Handler) acquireLock(ctx context.Context, sourceID string) (*pgxpool.Conn, error) {
	pool, ok := h.TxStarter.(*pgxpool.Pool)
	if !ok {
		return nil, fmt.Errorf("TxStarter is not a *pgxpool.Pool")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	lockKey := hashSourceID(sourceID)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		conn.Release()
		return nil, err
	}
	return conn, nil
}

func (h *Handler) releaseLock(conn *pgxpool.Conn, sourceID string) {
	lockKey := hashSourceID(sourceID)
	conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	conn.Release()
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

	// Acquire session-level advisory lock (pinned connection, not xact-bound)
	conn, err := h.acquireLock(r.Context(), sourceID)
	if err != nil {
		slog.Warn("DeleteKnowledgeSource: acquire lock failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to acquire lock")
		return
	}
	defer h.releaseLock(conn, sourceID)

	// Short tx: load config for legacy space_key, then delete the row
	var configJSON json.RawMessage
	tx, err := conn.Begin(r.Context())
	if err != nil {
		slog.Warn("DeleteKnowledgeSource: begin tx failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to delete knowledge source")
		return
	}

	err = tx.QueryRow(r.Context(), `
		SELECT config FROM knowledge_sources WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&configJSON)
	if err != nil {
		if isNotFound(err) {
			tx.Rollback(r.Context())
			writeError(w, http.StatusNotFound, "knowledge source not found")
			return
		}
		tx.Rollback(r.Context())
		slog.Warn("DeleteKnowledgeSource lookup failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to read knowledge source")
		return
	}

	result, err := tx.Exec(r.Context(), `
		DELETE FROM knowledge_sources WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID)
	if err != nil {
		tx.Rollback(r.Context())
		slog.Warn("DeleteKnowledgeSource delete failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to delete knowledge source")
		return
	}
	if result.RowsAffected() == 0 {
		tx.Rollback(r.Context())
		writeError(w, http.StatusNotFound, "knowledge source not found")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Warn("DeleteKnowledgeSource: commit failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to delete knowledge source")
		return
	}

	// Qdrant cleanup (outside tx, still under session lock — best-effort)
	// Delete all generations for canonical source_id, then legacy space_key if different
	if h.KnowledgeManager != nil {
		if err := h.KnowledgeManager.DeleteAllSourcePoints(r.Context(), workspaceID, sourceID); err != nil {
			slog.Warn("DeleteKnowledgeSource: DeleteAllSourcePoints canonical failed",
				"workspace_id", workspaceID, "source_id", sourceID, "error", err)
		}
	}

	var cfg struct {
		SpaceKey string `json:"space_key"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err == nil && cfg.SpaceKey != "" && cfg.SpaceKey != sourceID {
		if h.KnowledgeManager != nil {
			if err := h.KnowledgeManager.DeleteAllSourcePoints(r.Context(), workspaceID, cfg.SpaceKey); err != nil {
				slog.Warn("DeleteKnowledgeSource: DeleteAllSourcePoints legacy failed",
					"workspace_id", workspaceID, "source_id", cfg.SpaceKey, "error", err)
			}
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

	// Init run + enqueue atomically (advisory lock + sync_run_id + River job within a single tx)
	if err := h.WorkerManager.InitAndEnqueueRun(r.Context(), sourceID, workspaceID, string(knowledge.SyncFull)); err != nil {
		slog.Warn("SyncKnowledgeSource: failed to init sync run", append(logger.RequestAttrs(r), "error", err)...)
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
