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
)

type SyncKnowledgeArgs struct {
	SourceID    string `json:"source_id"`
	WorkspaceID string `json:"workspace_id"`
	SyncKind    string `json:"sync_kind"`
}

func (SyncKnowledgeArgs) Kind() string { return "sync_knowledge" }

type SyncKnowledgeWorker struct {
	river.WorkerDefaults[SyncKnowledgeArgs]
	pool    *pgxpool.Pool
	km      *knowledge.Manager
	enqueue func(ctx context.Context, args SyncKnowledgeArgs) error
}

func NewSyncKnowledgeWorker(pool *pgxpool.Pool, km *knowledge.Manager, enqueue func(ctx context.Context, args SyncKnowledgeArgs) error) *SyncKnowledgeWorker {
	return &SyncKnowledgeWorker{pool: pool, km: km, enqueue: enqueue}
}

func (w *SyncKnowledgeWorker) Work(ctx context.Context, job *river.Job[SyncKnowledgeArgs]) error {
	args := job.Args
	slog.Info("sync-knowledge worker starting", "source_id", args.SourceID, "sync_kind", args.SyncKind)

	srcUUID := parseUUID(args.SourceID)
	wsUUID := parseUUID(args.WorkspaceID)

	// 1. Load source row
	var sourceType string
	var configJSON json.RawMessage
	var checkpoint string
	var lastSyncedAt pgtype.Timestamptz

	err := w.pool.QueryRow(ctx, `
		SELECT source_type, config, checkpoint, last_synced_at
		FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &configJSON, &checkpoint, &lastSyncedAt)
	if err != nil {
		slog.Warn("sync-knowledge worker: source not found", "source_id", args.SourceID, "workspace_id", args.WorkspaceID, "error", err)
		return nil
	}

	// 2. Get connector
	connector := sources.NewConnector(knowledge.SourceType(sourceType))
	if connector == nil {
		slog.Warn("sync-knowledge worker: unknown source type", "source_type", sourceType)
		_, _ = w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = 'unknown source type' WHERE id = $1`, srcUUID)
		return nil
	}

	// 4. Determine since
	var since *time.Time
	if args.SyncKind == string(knowledge.SyncIncremental) && lastSyncedAt.Valid {
		t := lastSyncedAt.Time
		since = &t
	}

	// 5. Set sync_status = 'syncing'
	_, _ = w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'syncing' WHERE id = $1`, srcUUID)

	// 6. FetchPage — error means River retries, do NOT save checkpoint
	result, err := connector.FetchPage(ctx, args.WorkspaceID, string(configJSON), checkpoint, since)
	if err != nil {
		slog.Warn("sync-knowledge worker: fetch failed", "source_id", args.SourceID, "error", err)
		return err
	}

	// 7. Delete old points on first batch of full sync (non-fatal)
	if checkpoint == "" && w.km != nil {
		if err := w.km.DeleteSourcePoints(ctx, args.WorkspaceID, args.SourceID); err != nil {
			slog.Warn("sync-knowledge worker: failed to delete old points", "source_id", args.SourceID, "error", err)
		}
	}

	// 8. Index chunks — error means River retries, do NOT save checkpoint
	if len(result.Chunks) > 0 && w.km != nil {
		if err := w.km.IndexChunks(ctx, args.WorkspaceID, result.Chunks); err != nil {
			slog.Warn("sync-knowledge worker: index failed", "source_id", args.SourceID, "error", err)
			return err
		}
	}

	// 9. Save checkpoint (always advances after successful IndexChunks)
	_, err = w.pool.Exec(ctx, `
		UPDATE knowledge_sources
		SET checkpoint = $1, pages_fetched = pages_fetched + $2
		WHERE id = $3
	`, result.NextCursor, result.PageCount, srcUUID)
	if err != nil {
		slog.Warn("sync-knowledge worker: failed to save checkpoint", "source_id", args.SourceID, "error", err,
			"sql_checkpoint", fmt.Sprintf("%#v", result.NextCursor),
			"sql_pages_fetched", fmt.Sprintf("%#v", result.PageCount))
		return err
	}

	// 10. More pages — re-enqueue (checkpoint is saved, re-enqueue is best-effort)
	if result.NextCursor != "" {
		if w.enqueue != nil {
			if err := w.enqueue(ctx, SyncKnowledgeArgs{
				SourceID:    args.SourceID,
				WorkspaceID: args.WorkspaceID,
				SyncKind:    args.SyncKind,
			}); err != nil {
				slog.Warn("sync-knowledge worker: re-enqueue failed", "source_id", args.SourceID, "error", err)
			}
		}
		return nil
	}

	// 11. Last batch — finalize
	_, err = w.pool.Exec(ctx, `
		UPDATE knowledge_sources
		SET sync_status = 'ready', last_synced_at = now(), total_pages = pages_fetched, checkpoint = ''
		WHERE id = $1
	`, srcUUID)
	if err != nil {
		slog.Warn("sync-knowledge worker: failed to finalize sync", "source_id", args.SourceID, "error", err)
		return err
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

	enqueueFn := func(ctx context.Context, args SyncKnowledgeArgs) error {
		_, err := m.client.Insert(ctx, args, nil)
		return err
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, NewSyncKnowledgeWorker(pool, km, enqueueFn))

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
		UniqueOpts: river.UniqueOpts{ByArgs: true},
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
				m.client.Insert(context.Background(), SyncKnowledgeArgs{
					SourceID:    id,
					WorkspaceID: wsID,
					SyncKind:    string(knowledge.SyncIncremental),
				}, nil)
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
