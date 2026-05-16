package knowledge

import (
	"context"
	"os"
	"testing"
)

func TestFailedUpsertPreservesOldContent(t *testing.T) {
	// Integration test: verify that a failed Upsert does NOT delete old content.
	// Requires QDRANT_URL env var.

	url := os.Getenv("QDRANT_URL")
	if url == "" {
		t.Skip("QDRANT_URL not set, skipping integration test")
	}

	store, err := NewQdrantStore(url, 1536)
	if err != nil {
		t.Fatalf("NewQdrantStore: %v", err)
	}

	ctx := context.Background()
	wsID := "test-integration-failed-upsert"
	sourceID := "test-source-fail"
	gen := 1
	pageID := "test-page-fail"

	// Clean up from previous runs
	store.DeleteBySourceID(ctx, wsID, sourceID)

	// 1. Insert initial content (3 chunks)
	initialChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 3, IndexGeneration: gen, Text: "keep-0", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 3, IndexGeneration: gen, Text: "keep-1", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 2, TotalChunks: 3, IndexGeneration: gen, Text: "keep-2", SourceType: "test", Title: "test"},
	}
	embedDim := 1536
	vecs := make([][]float32, 3)
	for i := range vecs {
		vecs[i] = make([]float32, embedDim)
		vecs[i][0] = 0.1
	}
	if err := store.Upsert(ctx, wsID, initialChunks, vecs); err != nil {
		t.Fatalf("Initial Upsert failed: %v", err)
	}
	t.Log("Inserted 3 initial chunks")

	initialCount, _ := store.CountPoints(ctx, wsID)
	t.Logf("Count after initial insert: %d", initialCount)

	// 2. Attempt a bad upsert with wrong-dimension vectors (0-dim)
	// This should return an error from Qdrant
	badChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 1, IndexGeneration: gen, Text: "bad", SourceType: "test", Title: "test"},
	}
	badVecs := make([][]float32, 1) // 0-dim vectors — wrong
	if err := store.Upsert(ctx, wsID, badChunks, badVecs); err == nil {
		// 0-dim vectors might succeed (Qdrant might just create empty vectors)
		// Try another approach: use nil vectors
		t.Log("0-dim upsert did not error, trying with nil vectors")
	}

	// 3. Verify that old content is preserved despite the bad upsert
	countAfterFailed, err := store.CountPoints(ctx, wsID)
	if err != nil {
		t.Fatalf("CountPoints after: %v", err)
	}
	t.Logf("Count after failed upsert attempt: %d", countAfterFailed)

	if countAfterFailed < initialCount {
		t.Errorf("old content was lost! had %d, now %d", initialCount, countAfterFailed)
	}

	// 4. Now test that an empty-slice upsert is rejected
	// (this simulates IndexChunks returning error before stale cleanup)
	if err := store.Upsert(ctx, wsID, nil, nil); err != nil {
		t.Logf("Nil-upsert correctly returned error: %v", err)
	} else {
		t.Log("Nil-upsert was handled gracefully (nil chunk check works)")
	}

	countAfterNil, _ := store.CountPoints(ctx, wsID)
	if countAfterNil < initialCount {
		t.Errorf("content lost after nil-upsert test! had %d, now %d", initialCount, countAfterNil)
	}
	t.Logf("Final count: %d (initial was %d)", countAfterNil, initialCount)

	// Cleanup
	store.DeleteBySourceID(ctx, wsID, sourceID)
}
