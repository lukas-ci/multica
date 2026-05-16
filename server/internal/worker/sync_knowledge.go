package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/knowledge/sources"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
	"github.com/multica-ai/multica/server/internal/util"
)

type SyncKnowledgeArgs struct {
	SourceID          string `json:"source_id"`
	WorkspaceID       string `json:"workspace_id"`
	SyncKind          string `json:"sync_kind"`
	SyncRunID         string `json:"sync_run_id"`
	Cursor            string `json:"cursor"`
	ExpectedCheckpoint string `json:"expected_checkpoint"`
}

func (SyncKnowledgeArgs) Kind() string { return "sync_knowledge" }

type SyncKnowledgeWorker struct {
	river.WorkerDefaults[SyncKnowledgeArgs]
	pool     *pgxpool.Pool
	km       *knowledge.Manager
	enqueue  func(ctx context.Context, args SyncKnowledgeArgs) error
	insertTx func(ctx context.Context, tx pgx.Tx, args SyncKnowledgeArgs) error
}

func NewSyncKnowledgeWorker(pool *pgxpool.Pool, km *knowledge.Manager, enqueue func(ctx context.Context, args SyncKnowledgeArgs) error, insertTx func(ctx context.Context, tx pgx.Tx, args SyncKnowledgeArgs) error) *SyncKnowledgeWorker {
	return &SyncKnowledgeWorker{pool: pool, km: km, enqueue: enqueue, insertTx: insertTx}
}

func (w *SyncKnowledgeWorker) Work(ctx context.Context, job *river.Job[SyncKnowledgeArgs]) error {
	args := job.Args
	slog.Info("sync-knowledge worker starting", "source_id", args.SourceID, "sync_kind", args.SyncKind, "cursor", args.Cursor)

	srcUUID := parseUUID(args.SourceID)
	wsUUID := parseUUID(args.WorkspaceID)

	// 1. Load source row with run-isolation columns
	var sourceType string
	var configJSON json.RawMessage
	var checkpoint string
	var lastSyncedAt pgtype.Timestamptz
	var dbSyncRunID pgtype.UUID
	var activeIndexGeneration int
	var syncIndexGeneration pgtype.Int4
	var legacyIndexMode bool
	var syncWatermarkAt pgtype.Timestamptz

	err := w.pool.QueryRow(ctx, `
		SELECT source_type, config, checkpoint, last_synced_at,
		       sync_run_id, active_index_generation, sync_index_generation, legacy_index_mode,
		       sync_watermark_at
		FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &configJSON, &checkpoint, &lastSyncedAt,
		&dbSyncRunID, &activeIndexGeneration, &syncIndexGeneration, &legacyIndexMode, &syncWatermarkAt)
	if err != nil {
		slog.Warn("sync-knowledge worker: source not found", "source_id", args.SourceID, "workspace_id", args.WorkspaceID, "error", err)
		return nil
	}

	// 2. Stale job detection: if job carries a SyncRunID and DB has a different one, discard
	if args.SyncRunID != "" {
		dbRunID := util.UUIDToString(dbSyncRunID)
		if dbRunID != args.SyncRunID {
			slog.Info("sync-knowledge worker: stale job, discarding",
				"source_id", args.SourceID,
				"job_sync_run_id", args.SyncRunID,
				"db_sync_run_id", dbRunID)
			return nil
		}
	}

	// 3. Update heartbeat
	_, _ = w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_heartbeat_at = now() WHERE id = $1`, srcUUID)

	// 4. Get connector
	connector := sources.NewConnector(knowledge.SourceType(sourceType))
	if connector == nil {
		slog.Warn("sync-knowledge worker: unknown source type", "source_type", sourceType)
		_, _ = w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = 'unknown source type' WHERE id = $1`, srcUUID)
		return nil
	}

	// 5. Determine effective checkpoint (use args.Cursor for continuation jobs)
	effectiveCheckpoint := args.Cursor
	if effectiveCheckpoint == "" {
		effectiveCheckpoint = checkpoint
	}

	// 6. Determine since/until bounds
	var since *time.Time
	if args.SyncKind == string(knowledge.SyncIncremental) && lastSyncedAt.Valid {
		t := lastSyncedAt.Time
		since = &t
	}
	var until *time.Time
	if args.SyncKind == string(knowledge.SyncIncremental) && syncWatermarkAt.Valid {
		t := syncWatermarkAt.Time
		until = &t
	}

	// 7. Set sync_status = 'syncing' (idempotent)
	_, _ = w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'syncing', sync_error = NULL WHERE id = $1`, srcUUID)

	// 8. FetchPage — error means River retries, do NOT save checkpoint
	result, err := connector.FetchPage(ctx, args.WorkspaceID, string(configJSON), args.SourceID, sources.FetchOptions{
		Cursor: effectiveCheckpoint,
		Since:  since,
		Until:  until,
	})
	if err != nil {
		slog.Warn("sync-knowledge worker: fetch failed", "source_id", args.SourceID, "error", err)
		return err
	}

	// 9. Determine target generation for this sync run
	syncGen := activeIndexGeneration + 1
	if syncIndexGeneration.Valid {
		syncGen = int(syncIndexGeneration.Int32)
	}

	// 10. Stamp chunks with the target sync generation
	for i := range result.Chunks {
		result.Chunks[i].IndexGeneration = syncGen
	}

	// 11. Index chunks — error means River retries, do NOT save checkpoint
	if len(result.Chunks) > 0 && w.km != nil {
		if err := w.km.IndexChunks(ctx, args.WorkspaceID, result.Chunks); err != nil {
			slog.Warn("sync-knowledge worker: index failed", "source_id", args.SourceID, "error", err)
			return err
		}
	}

	// 12. Save checkpoint in transaction + optionally enqueue continuation
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx for checkpoint: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE knowledge_sources
		SET checkpoint = $1, pages_fetched = pages_fetched + $2, sync_watermark_at = now()
		WHERE id = $3
	`, result.NextCursor, result.PageCount, srcUUID)
	if err != nil {
		return fmt.Errorf("save checkpoint in tx: %w", err)
	}

	if result.NextCursor != "" && w.insertTx != nil {
		if err := w.insertTx(ctx, tx, SyncKnowledgeArgs{
			SourceID:          args.SourceID,
			WorkspaceID:       args.WorkspaceID,
			SyncKind:          args.SyncKind,
			SyncRunID:         args.SyncRunID,
			Cursor:            result.NextCursor,
			ExpectedCheckpoint: result.NextCursor,
		}); err != nil {
			return fmt.Errorf("enqueue continuation in tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkpoint tx: %w", err)
	}

	// Fallback: if no insertTx available, enqueue outside tx (best-effort)
	if result.NextCursor != "" && w.insertTx == nil && w.enqueue != nil {
		if err := w.enqueue(ctx, SyncKnowledgeArgs{
			SourceID:          args.SourceID,
			WorkspaceID:       args.WorkspaceID,
			SyncKind:          args.SyncKind,
			SyncRunID:         args.SyncRunID,
			Cursor:            result.NextCursor,
			ExpectedCheckpoint: result.NextCursor,
		}); err != nil {
			slog.Warn("sync-knowledge worker: re-enqueue failed", "source_id", args.SourceID, "error", err)
		}
	}
	if result.NextCursor != "" {
		return nil
	}

	// 13. Last batch — finalize in transaction with generation promotion
	oldActiveGen := activeIndexGeneration
	tx2, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx for finalize: %w", err)
	}
	defer tx2.Rollback(ctx)

	_, err = tx2.Exec(ctx, `
		UPDATE knowledge_sources
		SET sync_status = 'ready', sync_error = NULL,
		    last_synced_at = COALESCE(sync_watermark_at, now()),
		    total_pages = pages_fetched,
		    checkpoint = '',
		    sync_run_id = NULL,
		    sync_started_at = NULL,
		    sync_heartbeat_at = NULL,
		    active_index_generation = COALESCE(sync_index_generation, active_index_generation),
		    sync_index_generation = NULL,
		    sync_watermark_at = NULL,
		    legacy_index_mode = false
		WHERE id = $1
	`, srcUUID)
	if err != nil {
		return fmt.Errorf("finalize sync: %w", err)
	}

	if err := tx2.Commit(ctx); err != nil {
		return fmt.Errorf("commit finalize: %w", err)
	}

	// Best-effort async cleanup of old generation points after promotion
	if w.km != nil && oldActiveGen > 0 {
		go func(ctx context.Context, wsID, srcID string, gen int) {
			if err := w.km.DeleteSourcePointsByGeneration(ctx, wsID, srcID, gen); err != nil {
				slog.Warn("sync-knowledge worker: failed to cleanup old generation points",
					"source_id", srcID, "generation", gen, "error", err)
			}
		}(context.Background(), args.WorkspaceID, args.SourceID, oldActiveGen)
	}

	slog.Info("sync-knowledge worker: sync complete", "source_id", args.SourceID)
	return nil
}

func parseUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	u.Scan(s)
	return u
}

type Manager struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
}

func NewManager(pool *pgxpool.Pool, km *knowledge.Manager) (*Manager, error) {
	m := &Manager{pool: pool}

	// Run River's database migrations to create river_job, river_queue, etc.
	migrator, err := rivermigrate.New[pgx.Tx](riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, fmt.Errorf("river migrator: %w", err)
	}
	ctx := context.Background()
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return nil, fmt.Errorf("river migrate: %w", err)
	}

	enqueueFn := func(ctx context.Context, args SyncKnowledgeArgs) error {
		_, err := m.client.Insert(ctx, args, nil)
		return err
	}

	insertTxFn := func(ctx context.Context, tx pgx.Tx, args SyncKnowledgeArgs) error {
		_, err := m.client.InsertTx(ctx, tx, args, nil)
		return err
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, NewSyncKnowledgeWorker(pool, km, enqueueFn, insertTxFn))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:     workers,
		JobTimeout:  30 * time.Minute,
		MaxAttempts: 3,
	})
	if err != nil {
		return nil, err
	}
	m.client = client
	return m, nil
}

func (m *Manager) Start(ctx context.Context) error { return m.client.Start(ctx) }
func (m *Manager) Stop(ctx context.Context) error  { return m.client.Stop(ctx) }

func (m *Manager) Enqueue(ctx context.Context, args SyncKnowledgeArgs) error {
	_, err := m.client.Insert(ctx, args, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	})
	return err
}

func (m *Manager) EnqueueScheduled(ctx context.Context, args SyncKnowledgeArgs, at time.Time) error {
	_, err := m.client.Insert(ctx, args, &river.InsertOpts{
		ScheduledAt: at,
	})
	return err
}

func (m *Manager) RegisterPeriodicJobs() error {
	m.client.PeriodicJobs().Add(river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			rows, err := m.pool.Query(context.Background(), `
				SELECT id, workspace_id FROM knowledge_sources WHERE sync_status = 'ready'
			`)
			if err != nil {
				slog.Warn("sync-knowledge: periodic query failed", "error", err)
				return nil, nil
			}
			defer rows.Close()

			var firstID, firstWS string
			for rows.Next() {
				var id, wsID string
				if err := rows.Scan(&id, &wsID); err != nil {
					continue
				}
				if firstID == "" {
					firstID, firstWS = id, wsID
					continue
				}
				// Enqueue all but the first directly (constructor returns the first)
				if _, err := m.client.Insert(context.Background(), SyncKnowledgeArgs{
					SourceID:    id,
					WorkspaceID: wsID,
					SyncKind:    string(knowledge.SyncIncremental),
				}, nil); err != nil {
					slog.Warn("sync-knowledge: failed to enqueue periodic sync", "source_id", id, "error", err)
				}
			}
			if firstID != "" {
				return SyncKnowledgeArgs{
					SourceID:    firstID,
					WorkspaceID: firstWS,
					SyncKind:    string(knowledge.SyncIncremental),
				}, nil
			}
			return nil, nil
		},
		nil,
	))
	return nil
}
