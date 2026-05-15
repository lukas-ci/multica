package sources

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/knowledge"
)

type PageResult struct {
	Chunks     []knowledge.Chunk
	NextCursor string
	TotalCount int
}

type Connector interface {
	Fetch(ctx context.Context, workspaceID, configJSON string) ([]knowledge.Chunk, error)
	FetchPage(ctx context.Context, workspaceID, configJSON, cursor string, since *time.Time) (*PageResult, error)
	SourceType() knowledge.SourceType
}

func NewConnector(st knowledge.SourceType) Connector {
	switch st {
	case knowledge.SourceConfluence:
		return &ConfluenceConnector{}
	default:
		return nil
	}
}
