package sources

import (
	"context"

	"github.com/multica-ai/multica/server/internal/knowledge"
)

type Connector interface {
	Fetch(ctx context.Context, workspaceID, configJSON string) ([]knowledge.Chunk, error)
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
