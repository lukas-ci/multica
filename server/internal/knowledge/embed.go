package knowledge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var embedHTTPClient = &http.Client{Timeout: 120 * time.Second}

type Embedder interface {
	Embed(texts []string) ([][]float32, error)
	Dimension() int
}

type AIGWEmbedder struct {
	BaseURL string
	APIKey  string
	Model   string
}

type openAIEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

func (e *AIGWEmbedder) Embed(texts []string) ([][]float32, error) {
	// Truncate each text to stay under the 8192 token limit
	// (~32000 chars for English, ~16000 for code-heavy content)
	const maxEmbedChars = 16000
	truncated := make([]string, len(texts))
	for i, t := range texts {
		if len(t) > maxEmbedChars {
			truncated[i] = t[:maxEmbedChars]
		} else {
			truncated[i] = t
		}
	}

	reqBody := openAIEmbedRequest{Model: e.Model, Input: truncated}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("embed marshal: %w", err)
	}
	req, err := http.NewRequest("POST", e.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := embedHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
	}
	var embedResp openAIEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, fmt.Errorf("embed json: %w", err)
	}
	if len(embedResp.Data) != len(texts) {
		return nil, fmt.Errorf("embed: expected %d embeddings, got %d", len(texts), len(embedResp.Data))
	}
	result := make([][]float32, len(embedResp.Data))
	for i, d := range embedResp.Data {
		vec := make([]float32, len(d.Embedding))
		for j, v := range d.Embedding {
			vec[j] = float32(v)
		}
		result[i] = vec
	}
	return result, nil
}

func (e *AIGWEmbedder) Dimension() int { return 1536 }

type OllamaEmbedder struct {
	BaseURL string
	Model   string
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

func (e *OllamaEmbedder) Embed(texts []string) ([][]float32, error) {
	reqBody := ollamaEmbedRequest{Model: e.Model, Input: texts}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ollama embed marshal: %w", err)
	}
	resp, err := embedHTTPClient.Post(e.BaseURL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama embed read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama embed: HTTP %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
	}
	var embedResp ollamaEmbedResponse
	if err := json.Unmarshal(respBody, &embedResp); err != nil {
		return nil, fmt.Errorf("ollama embed json: %w", err)
	}
	if len(embedResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: expected %d embeddings, got %d", len(texts), len(embedResp.Embeddings))
	}
	result := make([][]float32, len(embedResp.Embeddings))
	for i, emb := range embedResp.Embeddings {
		vec := make([]float32, len(emb))
		for j, v := range emb {
			vec[j] = float32(v)
		}
		result[i] = vec
	}
	return result, nil
}

func (e *OllamaEmbedder) Dimension() int { return 768 }

func NewEmbedder() Embedder {
	provider := os.Getenv("KNOWLEDGE_EMBEDDING_PROVIDER")
	if provider == "ollama" {
		return &OllamaEmbedder{
			BaseURL: os.Getenv("OLLAMA_BASE_URL"),
			Model:   os.Getenv("OLLAMA_EMBEDDING_MODEL"),
		}
	}
	return &AIGWEmbedder{
		BaseURL: os.Getenv("AIGW_BASE_URL"),
		APIKey:  os.Getenv("AIGW_API_KEY"),
		Model:   os.Getenv("AIGW_EMBEDDING_MODEL"),
	}
}
