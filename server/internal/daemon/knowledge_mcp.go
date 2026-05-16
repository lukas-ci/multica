package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// knowledgeCapability represents the result of probing a backend for
// Knowledge MCP support.
type knowledgeCapability int

const (
	knowledgeCapUnknown     knowledgeCapability = iota
	knowledgeCapSupported                      // backend supports knowledge_search
	knowledgeCapAuthFailure                     // 401 — token missing or invalid
	knowledgeCapUnsupported                     // 404/405 — backend has no knowledge service
	knowledgeCapTransient                       // 5xx/network error — retry later
)

func (c knowledgeCapability) String() string {
	switch c {
	case knowledgeCapSupported:
		return "supported"
	case knowledgeCapAuthFailure:
		return "auth_failure"
	case knowledgeCapUnsupported:
		return "unsupported"
	case knowledgeCapTransient:
		return "transient"
	default:
		return "unknown"
	}
}

// knowledgeProbeEntry holds a cached probe result with a bounded TTL.
type knowledgeProbeEntry struct {
	cap knowledgeCapability
	at  time.Time
	ttl time.Duration
}

const (
	// cacheTTLSupported keeps a supported result for the daemon process
	// lifetime — a supported backend does not become unsupported at the
	// same URL without a deploy, and the daemon restart will re-probe.
	cacheTTLSupported = 24 * time.Hour
	// cacheTTLUnsupported keeps an unsupported result for 5 minutes so the
	// daemon does not hammer a known-unsupported backend on every task. A
	// backend upgrade at the same URL becomes visible within one TTL window.
	cacheTTLUnsupported = 5 * time.Minute
	// cacheTTLTransient is deliberately zero: transient failures are not
	// cached so the next task attempt re-probes immediately.
	cacheTTLTransient = 0
)

// probeKnowledgeMCPCap probes the backend for Knowledge MCP capability.
//
// Resolution order:
//  1. GET <baseURL>/api/capabilities — typed endpoint; 200 is accepted as
//     supported without parsing the body.
//  2. POST <baseURL>/api/mcp with tools/list JSON-RPC — authenticates with the
//     given token. 200 + knowledge_search in tools → supported; 401 → auth
//     failure; 404/405 → unsupported; 5xx/network → transient.
func probeKnowledgeMCPCap(client *http.Client, baseURL, token string) knowledgeCapability {
	// Step 1: typed /api/capabilities endpoint.
	capURL := strings.TrimRight(baseURL, "/") + "/api/capabilities"
	req, err := http.NewRequest("GET", capURL, nil)
	if err == nil {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, doErr := client.Do(req)
		if doErr == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return knowledgeCapSupported
			}
			if resp.StatusCode >= 500 {
				return knowledgeCapTransient
			}
			// 401/403/404/405 → fall through to /api/mcp fallback
		}
	}

	// Step 2: fallback — POST /api/mcp with tools/list JSON-RPC.
	mcpURL := strings.TrimRight(baseURL, "/") + "/api/mcp"
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req, err = http.NewRequest("POST", mcpURL, body)
	if err != nil {
		return knowledgeCapTransient
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return knowledgeCapTransient
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 200:
		if containsKnowledgeSearch(resp.Body) {
			return knowledgeCapSupported
		}
		return knowledgeCapUnsupported
	case resp.StatusCode == 401:
		return knowledgeCapAuthFailure
	case resp.StatusCode == 404 || resp.StatusCode == 405:
		return knowledgeCapUnsupported
	case resp.StatusCode >= 500:
		return knowledgeCapTransient
	default:
		return knowledgeCapTransient
	}
}

// containsKnowledgeSearch decodes a JSON-RPC tools/list response and returns
// true when the tools array includes a tool named "knowledge_search".
func containsKnowledgeSearch(r io.Reader) bool {
	var mcpResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(r).Decode(&mcpResp); err != nil {
		return false
	}
	for _, tool := range mcpResp.Result.Tools {
		if tool.Name == "knowledge_search" {
			return true
		}
	}
	return false
}

// knowledgeProbeCache is a concurrency-safe cache for capability probe results.
type knowledgeProbeCache struct {
	mu    sync.Mutex
	store map[string]*knowledgeProbeEntry
}

func newKnowledgeProbeCache() *knowledgeProbeCache {
	return &knowledgeProbeCache{store: make(map[string]*knowledgeProbeEntry)}
}

// get returns a cached entry if it exists and has not expired.
func (c *knowledgeProbeCache) get(key string) (knowledgeCapability, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.store[key]
	if !ok {
		return knowledgeCapUnknown, false
	}
	if e.ttl <= 0 || time.Since(e.at) > e.ttl {
		delete(c.store, key)
		return knowledgeCapUnknown, false
	}
	return e.cap, true
}

// set stores a probe result with the appropriate TTL.
func (c *knowledgeProbeCache) set(key string, cap knowledgeCapability) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ttl time.Duration
	switch cap {
	case knowledgeCapSupported:
		ttl = cacheTTLSupported
	case knowledgeCapUnsupported:
		ttl = cacheTTLUnsupported
	default:
		ttl = cacheTTLTransient
	}
	c.store[key] = &knowledgeProbeEntry{cap: cap, at: time.Now(), ttl: ttl}
}

// probeCacheKey returns a stable cache key for (serverURL, token).
func probeCacheKey(serverURL, token string) string {
	return serverURL + "|" + shortTokenHash(token)
}

func shortTokenHash(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h[:4])
}

// knowledgeMCPCapability returns the backend's Knowledge MCP capability,
// consulting and updating the probe cache. Concurrent callers for the same
// (serverURL, token) pair collapse to a single probe.
func (d *Daemon) knowledgeMCPCapability(baseURL string) knowledgeCapability {
	token := readMulticaToken()
	key := probeCacheKey(baseURL, token)

	d.knowledgeProbeMu.Lock()
	if cap, ok := d.knowledgeProbeCache.get(key); ok {
		d.knowledgeProbeMu.Unlock()
		return cap
	}
	// Not cached — probe under the lock so concurrent callers for the same
	// key do not stampede the backend.
	client := &http.Client{Timeout: 10 * time.Second}
	cap := probeKnowledgeMCPCap(client, baseURL, token)
	d.knowledgeProbeCache.set(key, cap)
	d.knowledgeProbeMu.Unlock()
	return cap
}

// deriveKnowledgeMCPURL returns the Knowledge API base URL to use for MCP
// injection. Resolution order:
//  1. MULTICA_KNOWLEDGE_MCP_URL env var (explicit override, backward compat).
//     If MULTICA_KNOWLEDGE_MCP_DISABLE is also set alongside this, disable wins.
//  2. MULTICA_KNOWLEDGE_MCP_DISABLE env var (explicit disable).
//  3. Derived from serverBaseURL as <serverBaseURL>/api/mcp.
//
// Returns empty string when Knowledge MCP should be disabled.
func deriveKnowledgeMCPURL(serverBaseURL string) string {
	override := os.Getenv("MULTICA_KNOWLEDGE_MCP_URL")
	disabled := os.Getenv("MULTICA_KNOWLEDGE_MCP_DISABLE")

	if override != "" && disabled == "" {
		return override
	}
	if disabled != "" {
		return ""
	}
	if serverBaseURL == "" {
		return ""
	}
	return strings.TrimRight(serverBaseURL, "/") + "/api/mcp"
}

// mergeKnowledgeMCP merges the knowledge MCP server config into an existing
// agent-level MCP config using the stdio transport (Cursor/Claude expected format).
// The baseURL should come from deriveKnowledgeMCPURL so the resolution chain
// (override env → disable env → derivation from server URL) is applied first.
func mergeKnowledgeMCP(existing json.RawMessage, baseURL, workspaceID string) json.RawMessage {
	// Find the script: look alongside the multica binary directory
	scriptPath := resolveKnowledgeScript()

	// Read PAT from the daemon's Multica config for API auth
	pat := readMulticaToken()
	envVars := map[string]string{
		"MULTICA_API_URL":      strings.TrimSuffix(baseURL, "/api/mcp"),
		"MULTICA_WORKSPACE_ID": workspaceID,
	}
	if pat != "" {
		envVars["MULTICA_AUTH_TOKEN"] = pat
	}
	ks := map[string]interface{}{
		"command": "node",
		"args":    []string{scriptPath},
		"env":     envVars,
	}

	// Try to parse existing config as {"mcpServers": {...}}
	servers := map[string]interface{}{}
	if len(existing) > 0 {
		var shaped map[string]interface{}
		if err := json.Unmarshal(existing, &shaped); err == nil {
			if s, ok := shaped["mcpServers"]; ok {
				if sm, ok := s.(map[string]interface{}); ok {
					servers = sm
				}
			}
		}
	}

	servers["knowledge"] = ks

	result, err := json.Marshal(map[string]interface{}{"mcpServers": servers})
	if err != nil {
		return existing
	}
	return result
}

func readMulticaToken() string {
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".multica", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var config struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.Token
}

func resolveKnowledgeScript() string {
	// Look in the same directory as the multica binary, then in ~/dev
	paths := []string{
		filepath.Join(filepath.Dir(os.Args[0]), "tools", "mcp-knowledge", "index.mjs"),
		filepath.Join(os.Getenv("HOME"), "dev", "multica-source", "tools", "mcp-knowledge", "index.mjs"),
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fallback: relative to the cloned source
	return filepath.Join(os.Getenv("HOME"), "dev", "multica-source", "tools", "mcp-knowledge", "index.mjs")
}
