package knowledge

import (
	"context"
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

func (m *Manager) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.Limit == 0 {
		req.Limit = 10
	}
	vecs, err := m.embedder.Embed([]string{req.Query})
	if err != nil {
		return nil, err
	}
	return m.store.Search(ctx, req.WorkspaceID, vecs[0], req.Limit, req.SourceTypes)
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
		if err := m.store.Upsert(ctx, workspaceID, batch, vectors, i); err != nil {
			return err
		}
	}
	return nil
}
