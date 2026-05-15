package knowledge

type SourceType string

const (
	SourceConfluence  SourceType = "confluence"
	SourceGitHub      SourceType = "github"
	SourceSlack       SourceType = "slack"
	SourceGoogleDrive SourceType = "google_drive"
)

type Chunk struct {
	Text        string            `json:"text"`
	SourceType  SourceType        `json:"source_type"`
	SourceID    string            `json:"source_id"`
	PageID      string            `json:"page_id"`
	WorkspaceID string            `json:"workspace_id"`
	URL         string            `json:"url"`
	Title       string            `json:"title"`
	ChunkIndex  int               `json:"chunk_index"`
	TotalChunks int               `json:"total_chunks"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type SearchResult struct {
	Score       float64    `json:"score"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	SourceType  SourceType `json:"source_type"`
	Snippet     string     `json:"snippet"`
	ChunkIndex  int        `json:"chunk_index"`
	TotalChunks int        `json:"total_chunks"`
}

type SearchRequest struct {
	WorkspaceID string       `json:"workspace_id"`
	Query       string       `json:"query"`
	SourceTypes []SourceType `json:"source_types,omitempty"`
	Limit       int          `json:"limit,omitempty"`
}

type SyncKind string

const (
	SyncFull        SyncKind = "full"
	SyncIncremental SyncKind = "incremental"
)
