package sources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/multica-ai/multica/server/internal/knowledge"
)

type ConfluenceConfig struct {
	BaseURL  string `json:"base_url"`
	Token    string `json:"token"`
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

func (c *ConfluenceConnector) Fetch(workspaceID, configJSON string) ([]knowledge.Chunk, error) {
	var cfg ConfluenceConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	unstructuredURL := os.Getenv("UNSTRUCTURED_URL")
	if unstructuredURL == "" {
		unstructuredURL = "http://unstructured:8000"
	}

	var allChunks []knowledge.Chunk
	nextURL := fmt.Sprintf("%s/rest/api/content?spaceKey=%s&expand=body.storage&limit=25", cfg.BaseURL, cfg.SpaceKey)

	for nextURL != "" {
		req, _ := http.NewRequest("GET", nextURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
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
		if nextURL != "" && !startsWithHTTP(nextURL) {
			nextURL = cfg.BaseURL + nextURL
		}
	}

	return allChunks, nil
}

func startsWithHTTP(s string) bool {
	return len(s) >= 4 && s[:4] == "http"
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
	reqBody, _ := json.Marshal(unstructuredRequest{
		Filename: page.ID + ".html",
		Content:  page.Body.Storage.Value,
	})
	resp, err := http.Post(baseURL+"/general/v0/general", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var elements []unstructuredElement
	json.Unmarshal(respBody, &elements)

	var chunks []knowledge.Chunk
	var buf string
	wordCount := 0
	chunkIdx := 0

	for _, el := range elements {
		if el.Text == "" {
			continue
		}
		buf += el.Text + "\n"
		wordCount += countWords(el.Text)
		if wordCount >= 500 {
			chunks = append(chunks, makeChunk(buf, page, workspaceID, spaceKey, chunkIdx, 0))
			buf = ""
			wordCount = 0
			chunkIdx++
		}
	}
	if buf != "" {
		chunks = append(chunks, makeChunk(buf, page, workspaceID, spaceKey, chunkIdx, 0))
	}
	for i := range chunks {
		chunks[i].TotalChunks = len(chunks)
	}

	return chunks, nil
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
