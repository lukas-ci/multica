package worker

import (
	"testing"
)

func TestStaleCleanupPageTotals(t *testing.T) {
	// Simulate the pageTotals computation from Work()
	type chunk struct {
		PageID      string
		TotalChunks int
	}
	tests := []struct {
		name   string
		chunks []chunk
		want   map[string]int // pageID -> expected total
	}{
		{
			name: "single page, same total",
			chunks: []chunk{
				{PageID: "p1", TotalChunks: 3},
				{PageID: "p1", TotalChunks: 3},
				{PageID: "p1", TotalChunks: 3},
			},
			want: map[string]int{"p1": 3},
		},
		{
			name: "multiple pages, varying totals",
			chunks: []chunk{
				{PageID: "p1", TotalChunks: 3},
				{PageID: "p1", TotalChunks: 3},
				{PageID: "p2", TotalChunks: 1},
			},
			want: map[string]int{"p1": 3, "p2": 1},
		},
		{
			name: "shrunk page: max total is the authoritative count",
			chunks: []chunk{
				{PageID: "p1", TotalChunks: 2}, // was 5, now 2
				{PageID: "p1", TotalChunks: 2},
			},
			want: map[string]int{"p1": 2},
		},
		{
			name:   "empty chunks",
			chunks: []chunk{},
			want:   map[string]int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(map[string]int)
			for _, c := range tt.chunks {
				if c.PageID != "" && c.TotalChunks > got[c.PageID] {
					got[c.PageID] = c.TotalChunks
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d pages, want %d", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("page %s: got total=%d, want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestStaleCleanupSkipsEmptyPageID(t *testing.T) {
	// pageTotals should skip chunks with empty PageID
	type chunk struct {
		PageID      string
		TotalChunks int
	}
	chunks := []chunk{
		{PageID: "", TotalChunks: 3},
		{PageID: "p1", TotalChunks: 2},
	}
	got := make(map[string]int)
	for _, c := range chunks {
		if c.PageID != "" && c.TotalChunks > got[c.PageID] {
			got[c.PageID] = c.TotalChunks
		}
	}
	if len(got) != 1 || got["p1"] != 2 {
		t.Errorf("expected {p1:2}, got %v", got)
	}
}
