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
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vectors, err := m.embedder.Embed(texts)
	if err != nil {
		return err
	}
	return m.store.Upsert(ctx, workspaceID, chunks, vectors)
}
