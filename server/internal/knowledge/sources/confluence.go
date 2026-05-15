package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/knowledge"
)

type ConfluenceConfig struct {
	BaseURL  string `json:"base_url"`
	Token    string `json:"token"`
	Email    string `json:"email"`
	SpaceKey string `json:"space_key"`
}

type ConfluenceConnector struct{}

func (c *ConfluenceConnector) SourceType() knowledge.SourceType {
	return knowledge.SourceConfluence
}

type confluencePage struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Version struct {
		When time.Time `json:"when"`
	} `json:"version"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type confluenceSearchResult struct {
	Results []confluencePage `json:"results"`
	Size    int              `json:"size"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

func (c *ConfluenceConnector) Fetch(ctx context.Context, workspaceID, configJSON string) ([]knowledge.Chunk, error) {
	var cfg ConfluenceConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	var allChunks []knowledge.Chunk
	cursor := ""

	for {
		result, err := c.FetchPage(ctx, workspaceID, configJSON, cursor, nil)
		if err != nil {
			return nil, err
		}
		allChunks = append(allChunks, result.Chunks...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	slog.Info("Confluence fetch complete", "space", cfg.SpaceKey, "chunks", len(allChunks))
	return allChunks, nil
}

func (c *ConfluenceConnector) FetchPage(ctx context.Context, workspaceID, configJSON, cursor string, since *time.Time) (*PageResult, error) {
	var cfg ConfluenceConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	email := cfg.Email
	if email == "" {
		email = "lukas.hu@instacart.com"
	}

	reqURL := fmt.Sprintf("%s/wiki/rest/api/content?spaceKey=%s&expand=body.storage,version&limit=25", baseURL, cfg.SpaceKey)
	if cursor != "" {
		decoded, err := url.QueryUnescape(cursor)
		if err == nil {
			reqURL = decoded
		}
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	req.SetBasicAuth(email, cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence fetch: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result confluenceSearchResult
	json.Unmarshal(body, &result)

	var chunks []knowledge.Chunk
	for _, page := range result.Results {
		var lastModified time.Time
		if !page.Version.When.IsZero() {
			lastModified = page.Version.When
		}

		if since != nil && !since.IsZero() && !lastModified.IsZero() && !lastModified.After(*since) {
			continue
		}

		pageChunks := chunkPage(page, workspaceID, cfg.SpaceKey)
		chunks = append(chunks, pageChunks...)
	}

	var nextCursor string
	if result.Links.Next != "" {
		if strings.HasPrefix(result.Links.Next, "/") {
			nextCursor = url.QueryEscape(baseURL + "/wiki" + result.Links.Next)
		} else if strings.HasPrefix(result.Links.Next, "http") {
			nextCursor = url.QueryEscape(result.Links.Next)
		} else {
			nextCursor = url.QueryEscape(baseURL + "/wiki/" + result.Links.Next)
		}
	}

	return &PageResult{
		Chunks:     chunks,
		NextCursor: nextCursor,
		TotalCount: result.Size,
	}, nil
}

func chunkPage(page confluencePage, workspaceID, spaceKey string) []knowledge.Chunk {
	text := stripHTML(page.Body.Storage.Value)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	words := strings.Fields(text)
	const chunkWords = 500
	var chunks []knowledge.Chunk

	for i := 0; i < len(words); i += chunkWords {
		end := i + chunkWords
		if end > len(words) {
			end = len(words)
		}
		chunkText := strings.Join(words[i:end], " ")
		chunks = append(chunks, makeChunk(chunkText, page, workspaceID, spaceKey, len(chunks), 0))
	}

	for i := range chunks {
		chunks[i].TotalChunks = len(chunks)
	}

	return chunks
}

func stripHTML(html string) string {
	b := []byte(html)
	var out strings.Builder
	inTag := false
	inScript := false
	inStyle := false
	for i := 0; i < len(b); i++ {
		if !inTag && !inScript && !inStyle && b[i] == '<' {
			if len(b) > i+6 && bytes.EqualFold(b[i+1:i+7], []byte("script")) {
				inScript = true
				continue
			}
			if len(b) > i+5 && bytes.EqualFold(b[i+1:i+6], []byte("style")) {
				inStyle = true
				continue
			}
			inTag = true
			continue
		}
		if inTag && b[i] == '>' {
			inTag = false
			if !inScript && !inStyle {
				out.WriteByte(' ')
			}
			continue
		}
		if inScript && b[i] == '<' && len(b) > i+8 && bytes.EqualFold(b[i+1:i+9], []byte("/script>")) {
			inScript = false
			inTag = false
			out.WriteByte(' ')
			i += 8
			continue
		}
		if inStyle && b[i] == '<' && len(b) > i+7 && bytes.EqualFold(b[i+1:i+8], []byte("/style>")) {
			inStyle = false
			inTag = false
			out.WriteByte(' ')
			i += 7
			continue
		}
		if !inTag && !inScript && !inStyle {
			out.WriteByte(b[i])
		}
	}
	return out.String()
}

func makeChunk(text string, page confluencePage, workspaceID, spaceKey string, idx, total int) knowledge.Chunk {
	return knowledge.Chunk{
		Text:        text,
		SourceType:  knowledge.SourceConfluence,
		SourceID:    spaceKey,
		WorkspaceID: workspaceID,
		URL:         page.Links.WebUI,
		Title:       page.Title,
		ChunkIndex:  idx,
		TotalChunks: total,
	}
}
