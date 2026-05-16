package knowledge

import (
	"context"
	"os"
	"testing"
)

func TestDeleteStalePageChunks_AgainstQdrant(t *testing.T) {
	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("QDRANT_URL not set, skipping integration test")
	}

	store, err := NewQdrantStore(url, 1536)
	if err != nil {
		t.Fatalf("NewQdrantStore: %v", err)
	}

	ctx := context.Background()
	wsID := "test-integration-stale-cleanup"
	sourceID := "test-source-uuid"
	gen := 1
	pageID := "test-page-123"

	store.DeleteBySourceID(ctx, wsID, sourceID)

	// 1. Upsert 5 chunks
	oldChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 5, IndexGeneration: gen, Text: "chunk0", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 5, IndexGeneration: gen, Text: "chunk1", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 2, TotalChunks: 5, IndexGeneration: gen, Text: "chunk2", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 3, TotalChunks: 5, IndexGeneration: gen, Text: "chunk3", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 4, TotalChunks: 5, IndexGeneration: gen, Text: "chunk4", SourceType: "test", Title: "test"},
	}
	vecs5 := make([][]float32, 5)
	for i := range vecs5 { vecs5[i] = make([]float32, 1536); vecs5[i][0] = 0.1 }
	if err := store.Upsert(ctx, wsID, oldChunks, vecs5); err != nil {
		t.Fatalf("Upsert (old) failed: %v", err)
	}
	t.Log("Upserted 5 old chunks")

	// 2. Upsert 2 replacement, then delete stale extras
	newChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 2, IndexGeneration: gen, Text: "replacement-0", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 2, IndexGeneration: gen, Text: "replacement-1", SourceType: "test", Title: "test"},
	}
	vecs2 := make([][]float32, 2)
	for i := range vecs2 { vecs2[i] = make([]float32, 1536); vecs2[i][0] = 0.2 }
	if err := store.Upsert(ctx, wsID, newChunks, vecs2); err != nil {
		t.Fatalf("Upsert (new) failed: %v", err)
	}
	if err := store.DeleteStalePageChunks(ctx, wsID, sourceID, gen, pageID, 2); err != nil {
		t.Fatalf("DeleteStalePageChunks failed: %v", err)
	}
	t.Log("Upserted 2 replacement + deleted stale extras (chunk_index >= 2)")

	// 3. Assert: total points for this collection should be < 5
	// (5 were inserted, 2 replaced, 3 stale deleted)
	count, err := store.CountPoints(ctx, wsID)
	if err != nil {
		t.Fatalf("CountPoints: %v", err)
	}
	t.Logf("Total collection count after cleanup: %d", count)

	if count >= 5 {
		t.Errorf("stale cleanup did not remove extras: count %d >= 5", count)
	} else {
		t.Logf("PASS: count %d < 5 — stale extras removed", count)
	}

	// 4. Cleanup
	store.DeleteBySourceID(ctx, wsID, sourceID)
}
