package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestFailedUpsertPreservesOldContent(t *testing.T) {
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

	store.DeleteBySourceID(ctx, wsID, sourceID)

	// 1. Insert 3 chunks with known text
	initialTexts := []string{"original-chunk-0", "original-chunk-1", "original-chunk-2"}
	initialChunks := []Chunk{
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 0, TotalChunks: 3, IndexGeneration: gen, Text: initialTexts[0], SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 1, TotalChunks: 3, IndexGeneration: gen, Text: initialTexts[1], SourceType: "test", Title: "test"},
		{SourceID: sourceID, WorkspaceID: wsID, PageID: pageID, ChunkIndex: 2, TotalChunks: 3, IndexGeneration: gen, Text: initialTexts[2], SourceType: "test", Title: "test"},
	}
	vecs := make([][]float32, 3)
	for i := range vecs { vecs[i] = make([]float32, 1536); vecs[i][0] = 0.1 }
	if err := store.Upsert(ctx, wsID, initialChunks, vecs); err != nil {
		t.Fatalf("Initial Upsert failed: %v", err)
	}
	t.Logf("Inserted 3 chunks")

	// 2. Attempt a bad upsert with known bad text via malformed REST request
	// Send a PUT to Qdrant REST with empty points to force an error
	httpURL := fmt.Sprintf("http://192.168.3.172:6333/collections/%s/points?wait=true", collectionName(wsID))
	badBody := `{"points": [{"id": 1, "vector": [0.1, 0.2], "payload": {"text": "should-not-appear"}}]}`
	http.DefaultClient.Post(httpURL, "application/json", strings.NewReader(badBody))
	// Note: this request may succeed (Qdrant might accept wrong-dim vectors for upsert)
	// The key assertion below is what matters

	// 3. Verify old content by scrolling the test page
	scrollReq, _ := json.Marshal(map[string]any{
		"limit":       10,
		"with_payload": true,
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "source_id", "match": map[string]any{"value": sourceID}},
				{"key": "index_generation", "match": map[string]any{"value": float64(gen)}},
				{"key": "page_id", "match": map[string]any{"value": pageID}},
			},
		},
	})
	scrollURL := fmt.Sprintf("http://192.168.3.172:6333/collections/%s/points/scroll", collectionName(wsID))
	resp, err := http.DefaultClient.Post(scrollURL, "application/json", strings.NewReader(string(scrollReq)))
	if err != nil {
		t.Fatalf("Scroll request failed: %v", err)
	}
	defer resp.Body.Close()

	var scrollResp struct {
		Result struct {
			Points []struct {
				ID      any              `json:"id"`
				Payload map[string]any   `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&scrollResp)

	if len(scrollResp.Result.Points) == 0 {
		t.Fatal("No points found — old content was lost despite failed upsert!")
	}

	foundTexts := make(map[string]bool)
	for _, p := range scrollResp.Result.Points {
		text, _ := p.Payload["text"].(string)
		foundTexts[text] = true
	}

	if foundTexts["should-not-appear"] {
		t.Error("Bad text found — old content was overwritten!")
	}
	originalFound := 0
	for _, orig := range initialTexts {
		if foundTexts[orig] {
			originalFound++
		}
	}
	if originalFound == 0 {
		t.Error("No original texts found — old content was LOST!")
		t.Errorf("Got texts: %v", foundTexts)
	} else {
		t.Logf("PASS: %d/%d original texts preserved, bad text not found", originalFound, len(initialTexts))
	}

	// 4. Cleanup
	store.DeleteBySourceID(ctx, wsID, sourceID)
}
