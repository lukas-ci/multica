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

var embedHTTPClient = &http.Client{Timeout: 60 * time.Second}

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
	reqBody := openAIEmbedRequest{Model: e.Model, Input: texts}
	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", e.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := embedHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var embedResp openAIEmbedResponse
	json.Unmarshal(respBody, &embedResp)
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
	body, _ := json.Marshal(reqBody)
	resp, err := embedHTTPClient.Post(e.BaseURL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var embedResp ollamaEmbedResponse
	json.Unmarshal(respBody, &embedResp)
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
