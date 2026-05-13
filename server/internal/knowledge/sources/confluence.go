package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

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
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

type confluenceSearchResult struct {
	Results []confluencePage `json:"results"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

func (c *ConfluenceConnector) Fetch(ctx context.Context, workspaceID, configJSON string) ([]knowledge.Chunk, error) {
	var cfg ConfluenceConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	unstructuredURL := os.Getenv("UNSTRUCTURED_URL")
	if unstructuredURL == "" {
		unstructuredURL = "http://unstructured:8000"
	}

	var allChunks []knowledge.Chunk
	nextURL := fmt.Sprintf("%s/wiki/rest/api/content?spaceKey=%s&expand=body.storage&limit=25", cfg.BaseURL, cfg.SpaceKey)

	for nextURL != "" {
		req, _ := http.NewRequestWithContext(ctx, "GET", nextURL, nil)
		email := cfg.Email
		if email == "" {
			email = "lukas.hu@instacart.com"
		}
		req.SetBasicAuth(email, cfg.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("confluence fetch: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result confluenceSearchResult
		json.Unmarshal(body, &result)

		for _, page := range result.Results {
			chunks, err := chunkWithUnstructured(unstructuredURL, page, workspaceID, cfg.SpaceKey)
			if err != nil {
				continue
			}
			allChunks = append(allChunks, chunks...)
		}

		nextURL = result.Links.Next
		if nextURL != "" && !strings.HasPrefix(nextURL, "http") {
			nextURL = cfg.BaseURL + "/wiki" + nextURL
		}
	}

	slog.Info("Confluence fetch complete", "space", cfg.SpaceKey, "chunks", len(allChunks))
	return allChunks, nil
}

type unstructuredRequest struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type unstructuredElement struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

func chunkWithUnstructured(baseURL string, page confluencePage, workspaceID, spaceKey string) ([]knowledge.Chunk, error) {
	// Strip HTML tags and chunk by word count directly.
	// No external Unstructured dependency needed for text extraction.
	text := stripHTML(page.Body.Storage.Value)
	if strings.TrimSpace(text) == "" {
		return nil, nil
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

	return chunks, nil
}

func stripHTML(html string) string {
	// Remove HTML tags, scripts, styles
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
			if len(b) > i+2 && b[i+1] == '/' {
				// Closing tag — skip
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

func countWords(s string) int {
	count := 0
	inWord := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			inWord = false
		} else if !inWord {
			count++
			inWord = true
		}
	}
	return count
}
