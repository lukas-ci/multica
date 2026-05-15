package worker

import (
	"context"
	"encoding/json"
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

func (SyncKnowledgeArgs) Kind() string {
	return "sync_knowledge"
}

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

	var sourceType string
	var configJSON json.RawMessage
	var checkpoint string
	var lastSyncedAt pgtype.Timestamptz

	srcUUID := parseUUID(args.SourceID)
	wsUUID := parseUUID(args.WorkspaceID)

	err := w.pool.QueryRow(ctx, `
		SELECT source_type, config, checkpoint, last_synced_at
		FROM knowledge_sources
		WHERE id = $1 AND workspace_id = $2
	`, srcUUID, wsUUID).Scan(&sourceType, &configJSON, &checkpoint, &lastSyncedAt)
	if err != nil {
		slog.Warn("sync-knowledge worker: source not found", "source_id", args.SourceID, "error", err)
		return nil
	}

	connector := sources.NewConnector(knowledge.SourceType(sourceType))
	if connector == nil {
		slog.Warn("sync-knowledge worker: unknown source type", "source_type", sourceType)
		w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = 'unknown source type' WHERE id = $1`, srcUUID)
		return nil
	}

	w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'syncing', sync_error = NULL WHERE id = $1`, srcUUID)

	var since *time.Time
	if args.SyncKind == string(knowledge.SyncIncremental) && lastSyncedAt.Valid {
		t := lastSyncedAt.Time
		since = &t
	}

	result, err := connector.FetchPage(ctx, string(args.WorkspaceID), string(configJSON), checkpoint, since)
	if err != nil {
		slog.Warn("sync-knowledge worker: fetch failed", "error", err)
		w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = $1 WHERE id = $2`, err.Error(), srcUUID)
		return err
	}

	if len(result.Chunks) > 0 && w.km != nil {
		if err := w.km.IndexChunks(ctx, string(args.WorkspaceID), result.Chunks); err != nil {
			slog.Warn("sync-knowledge worker: index failed", "error", err)
			w.pool.Exec(ctx, `UPDATE knowledge_sources SET sync_status = 'error', sync_error = $1 WHERE id = $2`, err.Error(), srcUUID)
			return err
		}

		newPagesFetched := result.PageCount
		if result.NextCursor == "" {
			newPagesFetched = result.PageCount
		} else {
			var currentFetched int
			w.pool.QueryRow(ctx, `SELECT pages_fetched FROM knowledge_sources WHERE id = $1`, srcUUID).Scan(&currentFetched)
			newPagesFetched = currentFetched + len(result.Chunks)
		}

		w.pool.Exec(ctx, `
			UPDATE knowledge_sources
			SET checkpoint = $1, pages_fetched = $2, total_pages = $3
			WHERE id = $4
		`, result.NextCursor, newPagesFetched, result.PageCount, srcUUID)
	}

	if result.NextCursor != "" && w.enqueue != nil {
		slog.Info("sync-knowledge worker: more pages, re-enqueuing", "source_id", args.SourceID, "next_cursor", result.NextCursor)
		if err := w.enqueue(ctx, SyncKnowledgeArgs{
			SourceID:    args.SourceID,
			WorkspaceID: args.WorkspaceID,
			SyncKind:    args.SyncKind,
		}); err != nil {
			slog.Warn("sync-knowledge worker: re-enqueue failed", "error", err)
		}
		return nil
	}

	w.pool.Exec(ctx, `
		UPDATE knowledge_sources
		SET sync_status = 'ready', last_synced_at = now(), checkpoint = ''
		WHERE id = $1
	`, srcUUID)

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
		_, err := m.client.Insert(ctx, args, &river.InsertOpts{
			UniqueOpts: river.UniqueOpts{
				ByArgs: true,
			},
		})
		return err
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, NewSyncKnowledgeWorker(pool, km, enqueueFn))

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, err
	}

	m.client = client
	return m, nil
}

func (m *Manager) Start(ctx context.Context) error {
	return m.client.Start(ctx)
}

func (m *Manager) Stop(ctx context.Context) error {
	return m.client.Stop(ctx)
}

func (m *Manager) Enqueue(ctx context.Context, args SyncKnowledgeArgs) error {
	_, err := m.client.Insert(ctx, args, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
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
