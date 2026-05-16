package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"net/http"
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
	httpURL     string
}

func NewQdrantStore(url string, dimension int) (*QdrantStore, error) {
	host := strings.TrimPrefix(url, "grpc://")
	host = strings.SplitN(host, ":", 2)[0]
	httpURL := "http://" + host + ":6333"

	url = strings.TrimPrefix(url, "grpc://")
	conn, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("qdrant connect: %w", err)
	}
	return &QdrantStore{
		client:      qdrantpb.NewPointsClient(conn),
		collections: qdrantpb.NewCollectionsClient(conn),
		dimension:   dimension,
		httpURL:     httpURL,
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
	if len(chunks) == 0 {
		return nil
	}
	if err := s.ensureCollection(ctx, workspaceID); err != nil {
		return err
	}

	// Use REST API for upsert (gRPC Upsert silently fails on Qdrant 1.18.0)
	type restPoint struct {
		ID      uint64         `json:"id"`
		Vector  []float32      `json:"vector"`
		Payload map[string]any `json:"payload"`
	}
	type restUpsertReq struct {
		Points []restPoint `json:"points"`
	}

	points := make([]restPoint, len(chunks))
	for i, c := range chunks {
		payload := map[string]any{
			"text":         c.Text,
			"source_type":  string(c.SourceType),
			"source_id":    c.SourceID,
			"workspace_id": c.WorkspaceID,
			"url":          c.URL,
			"title":        c.Title,
			"chunk_index":  c.ChunkIndex,
			"total_chunks": c.TotalChunks,
			"synced_at":    time.Now().UTC().Format(time.RFC3339),
		}
		points[i] = restPoint{
			ID:      stablePointID(c.SourceID, c.PageID, c.ChunkIndex),
			Vector:  vectors[i],
			Payload: payload,
		}
	}

	reqBody, err := json.Marshal(restUpsertReq{Points: points})
	if err != nil {
		return fmt.Errorf("qdrant upsert marshal: %w", err)
	}

	reqURL := fmt.Sprintf("%s/collections/%s/points?wait=true", s.httpURL, collectionName(workspaceID))
	req, err := http.NewRequestWithContext(ctx, "PUT", reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("qdrant upsert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("qdrant upsert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant upsert: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *QdrantStore) CountPoints(ctx context.Context, workspaceID string) (uint64, error) {
	if err := s.ensureCollection(ctx, workspaceID); err != nil {
		return 0, err
	}
	exact := true
	resp, err := s.client.Count(ctx, &qdrantpb.CountPoints{
		CollectionName: collectionName(workspaceID),
		Exact:          &exact,
	})
	if err != nil {
		return 0, err
	}
	if resp.GetResult() == nil {
		return 0, nil
	}
	return resp.GetResult().Count, nil
}

var pointIDTable = crc64.MakeTable(crc64.ECMA)

func stablePointID(sourceID, pageID string, chunkIndex int) uint64 {
	key := sourceID + ":" + pageID + ":" + strconv.Itoa(chunkIndex)
	return crc64.Checksum([]byte(key), pointIDTable)
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

func (s *QdrantStore) DeleteBySourceID(ctx context.Context, workspaceID, sourceID string) error {
	if err := s.ensureCollection(ctx, workspaceID); err != nil {
		return err
	}
	_, err := s.client.Delete(ctx, &qdrantpb.DeletePoints{
		CollectionName: collectionName(workspaceID),
		Points: &qdrantpb.PointsSelector{
			PointsSelectorOneOf: &qdrantpb.PointsSelector_Filter{
				Filter: &qdrantpb.Filter{
					Must: []*qdrantpb.Condition{
						{
							ConditionOneOf: &qdrantpb.Condition_Field{
								Field: &qdrantpb.FieldCondition{
									Key: "source_id",
									Match: &qdrantpb.Match{
										MatchValue: &qdrantpb.Match_Keyword{Keyword: sourceID},
									},
								},
							},
						},
					},
				},
			},
		},
	})
	return err
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
