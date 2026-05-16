package knowledge

import (
	"context"
	"fmt"
	"os"
)

type Manager struct {
	embedder Embedder
	store    *QdrantStore
}

func NewManager() (*Manager, error) {
	embedder := NewEmbedder()
	qdrantURL := os.Getenv("QDRANT_URL")
	if qdrantURL == "" {
		qdrantURL = "grpc://qdrant:6334"
	}
	store, err := NewQdrantStore(qdrantURL, embedder.Dimension())
	if err != nil {
		return nil, err
	}
	return &Manager{embedder: embedder, store: store}, nil
}

func (m *Manager) DropCollection(ctx context.Context, workspaceID string) error {
	return m.store.DropCollection(ctx, workspaceID)
}

func (m *Manager) CountIndexedChunks(ctx context.Context, workspaceID string) (uint64, error) {
	return m.store.CountPoints(ctx, workspaceID)
}

func (m *Manager) DeleteSourcePoints(ctx context.Context, workspaceID, sourceID string) error {
	return m.store.DeleteBySourceID(ctx, workspaceID, sourceID)
}

func (m *Manager) DeleteSourcePointsByGeneration(ctx context.Context, workspaceID, sourceID string, generation int) error {
	return m.store.DeleteBySourceIDAndGeneration(ctx, workspaceID, sourceID, generation)
}

func (m *Manager) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.Limit == 0 {
		req.Limit = 10
	}
	vecs, err := m.embedder.Embed([]string{req.Query})
	if err != nil {
		return nil, err
	}
	return m.store.Search(ctx, req.WorkspaceID, vecs[0], req.Limit, req.SourceTypes, req.IndexGenerations)
}

func (m *Manager) IndexChunks(ctx context.Context, workspaceID string, chunks []Chunk) error {
	const batchSize = 20
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]
		texts := make([]string, len(batch))
		for j, c := range batch {
			texts[j] = c.Text
		}
		vectors, err := m.embedder.Embed(texts)
		if err != nil {
			return err
		}
		if len(vectors) != len(batch) {
			return fmt.Errorf("IndexChunks: embedder returned %d vectors for %d chunks", len(vectors), len(batch))
		}
		if err := m.store.Upsert(ctx, workspaceID, batch, vectors); err != nil {
			return err
		}
	}
	return nil
}
