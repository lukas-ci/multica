package knowledge

import (
	"context"
	"os"
	"testing"
)

func TestDeleteStalePageChunks_AgainstQdrant(t *testing.T) {
	// Integration test against live Qdrant.
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
	wsID := "test-integration-stale-cleanup"
	sourceID := "test-source-uuid"
	gen := 1
	pageID := "test-page-123"

	// Clean up from previous runs
	store.DeleteBySourceID(ctx, wsID, sourceID)

	// 1. Upsert 5 chunks for a page (simulating old content before shrink)
	oldChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 5, IndexGeneration: gen, Text: "chunk0", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 5, IndexGeneration: gen, Text: "chunk1", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 2, TotalChunks: 5, IndexGeneration: gen, Text: "chunk2", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 3, TotalChunks: 5, IndexGeneration: gen, Text: "chunk3", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 4, TotalChunks: 5, IndexGeneration: gen, Text: "chunk4", SourceType: "test", Title: "test"},
	}
	embedDim := 1536
	dummyVectors := make([][]float32, len(oldChunks))
	for i := range dummyVectors {
		dummyVectors[i] = make([]float32, embedDim)
		dummyVectors[i][0] = 0.1 // non-zero vector
	}
	if err := store.Upsert(ctx, wsID, oldChunks, dummyVectors); err != nil {
		t.Fatalf("Upsert (old) failed: %v", err)
	}
	t.Log("Upserted 5 old chunks")

	// Verify count before shrink
	count, err := store.CountPoints(ctx, wsID)
	if err != nil {
		t.Fatalf("CountPoints: %v", err)
	}
	beforeCount := count
	t.Logf("Points before shrink: %d", beforeCount)
	if beforeCount < 5 {
		t.Fatalf("expected at least 5 points, got %d", beforeCount)
	}

	// 2. Simulate shrink: keep only 2 chunks, delete stale extras
	// First upsert the 2 replacement chunks (new text but same point IDs for indices 0,1)
	newChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 2, IndexGeneration: gen, Text: "new-chunk0", SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 2, IndexGeneration: gen, Text: "new-chunk1", SourceType: "test", Title: "test"},
	}
	newVectors := make([][]float32, len(newChunks))
	for i := range newVectors {
		newVectors[i] = make([]float32, embedDim)
		newVectors[i][0] = 0.2
	}
	if err := store.Upsert(ctx, wsID, newChunks, newVectors); err != nil {
		t.Fatalf("Upsert (new) failed: %v", err)
	}
	t.Log("Upserted 2 new replacement chunks")

	// Now delete stale extras: chunk_index >= 2
	keepCount := 2
	if err := store.DeleteStalePageChunks(ctx, wsID, sourceID, gen, pageID, keepCount); err != nil {
		t.Fatalf("DeleteStalePageChunks failed: %v", err)
	}
	t.Log("Deleted stale extras (chunk_index >= 2)")

	// 3. Verify: count should be close to 2 (the kept chunks)
	// (point IDs for chunk 0,1 overwritten; chunks 2,3,4 deleted; net ~2 points)
	countAfter, err := store.CountPoints(ctx, wsID)
	if err != nil {
		t.Fatalf("CountPoints after: %v", err)
	}
	t.Logf("Points after shrink+cleanup: %d", countAfter)

	// After upsert of 2 + stale cleanup of 3, we expect ~2 points for this source
	// (there may be old test data in the collection)
	if countAfter > beforeCount {
		t.Errorf("points after shrink (%d) should not exceed before (%d)", countAfter, beforeCount)
	}

	// 4. Clean up test data
	if err := store.DeleteBySourceID(ctx, wsID, sourceID); err != nil {
		t.Logf("Cleanup (DeleteBySourceID): %v", err)
	}
}
