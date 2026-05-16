package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	knowledgeSyncCleanupInterval = 5 * time.Minute
)

func runKnowledgeSyncCleanup(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(knowledgeSyncCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := pool.Exec(ctx, `
				UPDATE knowledge_sources ks
				SET sync_status = 'error',
				    sync_error = 'sync failed after max attempts; re-sync required',
				    sync_run_id = NULL,
				    sync_started_at = NULL,
				    sync_heartbeat_at = NULL
				WHERE sync_status = 'syncing'
				AND NOT EXISTS (
				    SELECT 1 FROM river_job
				    WHERE kind = 'sync_knowledge'
				    AND args->>'source_id' = ks.id::text
				    AND state IN ('available', 'running', 'retryable', 'scheduled')
				)
				AND EXISTS (
				    SELECT 1 FROM river_job
				    WHERE kind = 'sync_knowledge'
				    AND args->>'source_id' = ks.id::text
				    AND state = 'discarded'
				)
			`)
			if err != nil {
				slog.Warn("knowledge sync cleanup failed", "error", err)
			}
		}
	}
}
