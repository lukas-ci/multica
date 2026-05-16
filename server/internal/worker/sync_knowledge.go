package worker

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/knowledge"
	"github.com/multica-ai/multica/server/internal/knowledge/sources"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
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

	// 1. Acquire dedicated connection for session-level lock
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	lockKey := hashSourceID(args.SourceID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	// 2. Short tx: load source row + stale validation + set syncing status
	var sourceType string
	var configJSON json.RawMessage
	var checkpoint string
	var lastSyncedAt pgtype.Timestamptz
	var dbSyncRunID pgtype.Text
	var activeIndexGeneration int
	var syncIndexGeneration pgtype.Int4
	var legacyIndexMode bool
	var syncWatermarkAt pgtype.Timestamptz

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT source_type, config, checkpoint, last_synced_at,
		       sync_run_id::text, active_index_generation, sync_index_generation, legacy_index_mode,
		       sync_watermark_at
		FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &configJSON, &checkpoint, &lastSyncedAt,
		&dbSyncRunID, &activeIndexGeneration, &syncIndexGeneration, &legacyIndexMode, &syncWatermarkAt)
	if err != nil {
		tx.Rollback(ctx)
		slog.Warn("sync-knowledge worker: source not found", "source_id", args.SourceID, "workspace_id", args.WorkspaceID, "error", err)
		return nil
	}

	if args.SyncRunID != "" && (dbSyncRunID.String != args.SyncRunID || checkpoint != args.ExpectedCheckpoint) {
		tx.Rollback(ctx)
		slog.Info("sync-knowledge worker: stale job, discarding",
			"source_id", args.SourceID,
			"job_sync_run_id", args.SyncRunID,
			"db_sync_run_id", dbSyncRunID.String,
			"expected_checkpoint", args.ExpectedCheckpoint,
			"db_checkpoint", checkpoint)
		return nil
	}

	if _, err := tx.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'syncing', sync_error = NULL, sync_heartbeat_at = now() WHERE id = $1`, srcUUID); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("update sync status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("commit load tx: %w", err)
	}

	// 3. Get connector
	connector := sources.NewConnector(knowledge.SourceType(sourceType))
	if connector == nil {
		slog.Warn("sync-knowledge worker: unknown source type", "source_type", sourceType)
		if etx, cerr := conn.Begin(ctx); cerr == nil {
			etx.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = 'unknown source type' WHERE id = $1`, srcUUID)
			etx.Commit(ctx)
		}
		return nil
	}

	// 4. Determine effective checkpoint (use args.Cursor for continuation jobs)
	effectiveCheckpoint := args.Cursor
	if effectiveCheckpoint == "" {
		effectiveCheckpoint = checkpoint
	}

	// 5. Determine since/until bounds
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

	// 6. FetchPage — error means River retries; session lock is held across retries
	result, err := connector.FetchPage(ctx, args.WorkspaceID, string(configJSON), args.SourceID, sources.FetchOptions{
		Cursor: effectiveCheckpoint,
		Since:  since,
		Until:  until,
	})
	if err != nil {
		slog.Warn("sync-knowledge worker: fetch failed", "source_id", args.SourceID, "error", err)
		return err
	}

	// 7. Determine target generation for this sync run
	syncGen := activeIndexGeneration + 1
	if syncIndexGeneration.Valid {
		syncGen = int(syncIndexGeneration.Int32)
	}

	// 8. Stamp chunks with the target sync generation
	for i := range result.Chunks {
		result.Chunks[i].IndexGeneration = syncGen
	}

	// 9. Index chunks — error means River retries
	if len(result.Chunks) > 0 && w.km != nil {
		if err := w.km.IndexChunks(ctx, args.WorkspaceID, result.Chunks); err != nil {
			slog.Warn("sync-knowledge worker: index failed", "source_id", args.SourceID, "error", err)
			return err
		}
	}

	// 10. Short tx: save checkpoint conditionally on sync_run_id + expected_checkpoint
	tx, err = conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin checkpoint tx: %w", err)
	}

	ct, err := tx.Exec(ctx, `
		UPDATE knowledge_sources
		SET checkpoint = $1, pages_fetched = pages_fetched + $2
		WHERE id = $3 AND workspace_id = $4
		  AND ($5 = '' AND sync_run_id IS NULL OR sync_run_id::text = $5)
		  AND checkpoint = $6
	`, result.NextCursor, result.PageCount, srcUUID, wsUUID, args.SyncRunID, effectiveCheckpoint)
	if err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("save checkpoint in tx: %w", err)
	}
	if ct.RowsAffected() == 0 {
		tx.Rollback(ctx)
		slog.Warn("sync-knowledge worker: stale job, checkpoint update affected 0 rows",
			"source_id", args.SourceID, "sync_run_id", args.SyncRunID)
		return nil
	}

	// 11. Enqueue continuation within the same transaction
	if result.NextCursor != "" && w.insertTx != nil {
		if err := w.insertTx(ctx, tx, SyncKnowledgeArgs{
			SourceID:          args.SourceID,
			WorkspaceID:       args.WorkspaceID,
			SyncKind:          args.SyncKind,
			SyncRunID:         args.SyncRunID,
			Cursor:            result.NextCursor,
			ExpectedCheckpoint: result.NextCursor,
		}); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("enqueue continuation in tx: %w", err)
		}
	}

	if result.NextCursor != "" {
		if err := tx.Commit(ctx); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("commit checkpoint tx: %w", err)
		}
		if w.insertTx == nil && w.enqueue != nil {
			w.enqueue(ctx, SyncKnowledgeArgs{
				SourceID:          args.SourceID,
				WorkspaceID:       args.WorkspaceID,
				SyncKind:          args.SyncKind,
				SyncRunID:         args.SyncRunID,
				Cursor:            result.NextCursor,
				ExpectedCheckpoint: result.NextCursor,
			})
		}
		return nil
	}

	if err := tx.Commit(ctx); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("commit checkpoint tx: %w", err)
	}

	// 12. Short tx: last batch — finalize conditionally on sync_kind
	oldActiveGen := activeIndexGeneration

	tx, err = conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finalize tx: %w", err)
	}

	if args.SyncKind == string(knowledge.SyncIncremental) {
		ct, err = tx.Exec(ctx, `
			UPDATE knowledge_sources
			SET sync_status = 'ready', sync_error = NULL,
			    last_synced_at = COALESCE(sync_watermark_at, now()),
			    checkpoint = '',
			    sync_run_id = NULL,
			    sync_started_at = NULL,
			    sync_heartbeat_at = NULL,
			    sync_index_generation = NULL,
			    sync_watermark_at = NULL
			WHERE id = $1 AND workspace_id = $2
			  AND ($3 = '' AND sync_run_id IS NULL OR sync_run_id::text = $3)
			  AND checkpoint = $4
		`, srcUUID, wsUUID, args.SyncRunID, result.NextCursor)
	} else {
		ct, err = tx.Exec(ctx, `
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
			WHERE id = $1 AND workspace_id = $2
			  AND ($3 = '' AND sync_run_id IS NULL OR sync_run_id::text = $3)
			  AND checkpoint = $4
		`, srcUUID, wsUUID, args.SyncRunID, result.NextCursor)
	}
	if err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("finalize sync: %w", err)
	}
	if ct.RowsAffected() == 0 {
		tx.Rollback(ctx)
		slog.Warn("sync-knowledge worker: stale job, finalize update affected 0 rows",
			"source_id", args.SourceID, "sync_run_id", args.SyncRunID)
		return nil
	}

	if err := tx.Commit(ctx); err != nil {
		tx.Rollback(ctx)
		return fmt.Errorf("commit finalize: %w", err)
	}

	// Best-effort async cleanup of old generation points after full sync promotion
	if args.SyncKind != string(knowledge.SyncIncremental) && w.km != nil && oldActiveGen > 0 {
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

func hashSourceID(id string) int64 {
	h := sha1.New()
	h.Write([]byte(id))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

type Manager struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
}

func (m *Manager) InsertTx(ctx context.Context, tx pgx.Tx, args SyncKnowledgeArgs) error {
	_, err := m.client.InsertTx(ctx, tx, args, nil)
	return err
}

func (m *Manager) InitAndEnqueueRun(ctx context.Context, sourceID, workspaceID, syncKind string) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	lockKey := hashSourceID(sourceID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}

	syncRunID := uuid.NewString()
	srcUUID := parseUUID(sourceID)

	genExpr := "active_index_generation + 1"
	if syncKind == string(knowledge.SyncIncremental) {
		genExpr = "active_index_generation"
	}
	ct, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE knowledge_sources
		SET checkpoint = '', pages_fetched = 0, total_pages = NULL,
		    sync_kind = $3,
		    sync_run_id = $1, sync_started_at = now(), sync_heartbeat_at = now(),
		    sync_watermark_at = now(),
		    sync_index_generation = %s
		WHERE id = $2 AND sync_status != 'syncing'
	`, genExpr), syncRunID, srcUUID, syncKind)
	if err != nil {
		return fmt.Errorf("init sync run: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("source not found or already syncing")
	}

	if _, err := m.client.InsertTx(ctx, tx, SyncKnowledgeArgs{
		SourceID:    sourceID,
		WorkspaceID: workspaceID,
		SyncKind:    syncKind,
		SyncRunID:   syncRunID,
	}, nil); err != nil {
		return fmt.Errorf("enqueue sync job: %w", err)
	}

	return tx.Commit(ctx)
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
			ctx := context.Background()
			rows, err := m.pool.Query(ctx, `
				SELECT id, workspace_id FROM knowledge_sources WHERE sync_status = 'ready' AND NOT legacy_index_mode
			`)
			if err != nil {
				slog.Warn("sync-knowledge: periodic query failed", "error", err)
				return nil, nil
			}
			defer rows.Close()

			for rows.Next() {
				var id, wsID string
				if err := rows.Scan(&id, &wsID); err != nil {
					continue
				}
				if err := m.InitAndEnqueueRun(ctx, id, wsID, string(knowledge.SyncIncremental)); err != nil {
					slog.Warn("sync-knowledge: periodic init failed", "source_id", id, "error", err)
				}
			}
			return nil, nil
		},
		nil,
	))
	return nil
}
