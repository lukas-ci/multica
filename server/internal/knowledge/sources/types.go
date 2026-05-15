package sources

import (
	"context"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/knowledge"
)

type PageResult struct {
	Chunks     []knowledge.Chunk
	NextCursor string
	PageCount  int
}

type Connector interface {
	FetchPage(ctx context.Context, workspaceID, configJSON, cursor string, since *time.Time) (*PageResult, error)
	SourceType() knowledge.SourceType
}

func NewConnector(st knowledge.SourceType) Connector {
	switch st {
	case knowledge.SourceConfluence:
		return &ConfluenceConnector{
			httpClient: &http.Client{Timeout: 5 * time.Minute},
		}
	default:
		return nil
	}
}
