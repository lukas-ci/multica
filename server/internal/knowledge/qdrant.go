package knowledge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	qdrantpb "github.com/qdrant/go-client/qdrant"
)

type QdrantStore struct {
	client      qdrantpb.PointsClient
	collections qdrantpb.CollectionsClient
	dimension   int
}

func NewQdrantStore(url string, dimension int) (*QdrantStore, error) {
	url = strings.TrimPrefix(url, "grpc://")
	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("qdrant connect: %w", err)
	}
	return &QdrantStore{
		client:      qdrantpb.NewPointsClient(conn),
		collections: qdrantpb.NewCollectionsClient(conn),
		dimension:   dimension,
	}, nil
}

func collectionName(workspaceID string) string {
	// Qdrant collection names must be valid UTF-8, no hyphens in UUIDs
	id := strings.ReplaceAll(workspaceID, "-", "")
	return "ws_" + id
}

func (s *QdrantStore) ensureCollection(ctx context.Context, workspaceID string) error {
	name := collectionName(workspaceID)
	list, err := s.collections.List(ctx, &qdrantpb.ListCollectionsRequest{})
	if err != nil {
		return err
	}
	for _, c := range list.Collections {
		if c.Name == name {
			return nil
		}
	}
	_, err = s.collections.Create(ctx, &qdrantpb.CreateCollection{
		CollectionName: name,
		VectorsConfig: &qdrantpb.VectorsConfig{
			Config: &qdrantpb.VectorsConfig_Params{
				Params: &qdrantpb.VectorParams{
					Size:     uint64(s.dimension),
					Distance: qdrantpb.Distance_Cosine,
				},
			},
		},
	})
	return err
}

func (s *QdrantStore) Upsert(ctx context.Context, workspaceID string, chunks []Chunk, vectors [][]float32) error {
	if err := s.ensureCollection(ctx, workspaceID); err != nil {
		return err
	}
	points := make([]*qdrantpb.PointStruct, len(chunks))
	for i, c := range chunks {
		payload := map[string]*qdrantpb.Value{
			"text":         {Kind: &qdrantpb.Value_StringValue{StringValue: c.Text}},
			"source_type":  {Kind: &qdrantpb.Value_StringValue{StringValue: string(c.SourceType)}},
			"source_id":    {Kind: &qdrantpb.Value_StringValue{StringValue: c.SourceID}},
			"workspace_id": {Kind: &qdrantpb.Value_StringValue{StringValue: c.WorkspaceID}},
			"url":          {Kind: &qdrantpb.Value_StringValue{StringValue: c.URL}},
			"title":        {Kind: &qdrantpb.Value_StringValue{StringValue: c.Title}},
			"chunk_index":  {Kind: &qdrantpb.Value_IntegerValue{IntegerValue: int64(c.ChunkIndex)}},
			"total_chunks": {Kind: &qdrantpb.Value_IntegerValue{IntegerValue: int64(c.TotalChunks)}},
			"synced_at":    {Kind: &qdrantpb.Value_StringValue{StringValue: time.Now().UTC().Format(time.RFC3339)}},
		}
		points[i] = &qdrantpb.PointStruct{
			Id:      &qdrantpb.PointId{PointIdOptions: &qdrantpb.PointId_Num{Num: uint64(i)}},
			Vectors: &qdrantpb.Vectors{VectorsOptions: &qdrantpb.Vectors_Vector{Vector: &qdrantpb.Vector{Data: vectors[i]}}},
			Payload: payload,
		}
	}
	_, err := s.client.Upsert(ctx, &qdrantpb.UpsertPoints{
		CollectionName: collectionName(workspaceID),
		Points:         points,
	})
	return err
}

func generateChunkID(c Chunk) string {
	return c.WorkspaceID + "-" + c.SourceID + "-" + strconv.Itoa(c.ChunkIndex)
}

func (s *QdrantStore) Search(ctx context.Context, workspaceID string, queryVector []float32, limit int, sourceTypes []SourceType) ([]SearchResult, error) {
	if err := s.ensureCollection(ctx, workspaceID); err != nil {
		return nil, err
	}
	var filter *qdrantpb.Filter
	if len(sourceTypes) > 0 {
		var conditions []*qdrantpb.Condition
		for _, st := range sourceTypes {
			conditions = append(conditions, &qdrantpb.Condition{
				ConditionOneOf: &qdrantpb.Condition_Field{
					Field: &qdrantpb.FieldCondition{
						Key: "source_type",
						Match: &qdrantpb.Match{
							MatchValue: &qdrantpb.Match_Keyword{Keyword: string(st)},
						},
					},
				},
			})
		}
		filter = &qdrantpb.Filter{Should: conditions}
	}
	results, err := s.client.Search(ctx, &qdrantpb.SearchPoints{
		CollectionName: collectionName(workspaceID),
		Vector:         queryVector,
		Limit:          uint64(limit),
		WithPayload:    &qdrantpb.WithPayloadSelector{SelectorOptions: &qdrantpb.WithPayloadSelector_Enable{Enable: true}},
		Filter:         filter,
	})
	if err != nil {
		return nil, err
	}
	var out []SearchResult
	for _, r := range results.Result {
		out = append(out, SearchResult{
			Score:      float64(r.Score),
			Title:      getPayloadString(r.Payload, "title"),
			URL:        getPayloadString(r.Payload, "url"),
			SourceType: SourceType(getPayloadString(r.Payload, "source_type")),
			Snippet:    truncate(getPayloadString(r.Payload, "text"), 300),
		})
	}
	return out, nil
}

func getPayloadString(payload map[string]*qdrantpb.Value, key string) string {
	if v, ok := payload[key]; ok {
		if s, ok := v.Kind.(*qdrantpb.Value_StringValue); ok {
			return s.StringValue
		}
	}
	return ""
}

func (s *QdrantStore) DropCollection(ctx context.Context, workspaceID string) error {
	name := collectionName(workspaceID)
	_, err := s.collections.Delete(ctx, &qdrantpb.DeleteCollection{
		CollectionName: name,
	})
	return err
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
