package sources

import "github.com/multica-ai/multica/server/internal/knowledge"

type Connector interface {
	Fetch(workspaceID, configJSON string) ([]knowledge.Chunk, error)
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
