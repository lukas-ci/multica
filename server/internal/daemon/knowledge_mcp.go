package daemon

import (
	"bufio"
	"bytes"
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

	"github.com/multica-ai/multica/server/internal/cli"
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

// injectKnowledgeMCP resolves the Knowledge MCP URL, probes the backend for
// capability, and injects the knowledge_search tool into mcpConfig when the
// backend supports it. Auth uses the daemon's active profile token.
// This is the exact composition used by runTask.
func (d *Daemon) injectKnowledgeMCP(mcpConfig json.RawMessage, workspaceID string) json.RawMessage {
	mcpURL := deriveKnowledgeMCPURL(d.cfg.ServerBaseURL)
	if mcpURL == "" {
		return mcpConfig
	}
	token := d.daemonProfileToken()
	switch cap := d.knowledgeMCPCapability(d.cfg.ServerBaseURL); cap {
	case knowledgeCapSupported:
		return mergeKnowledgeMCP(mcpConfig, mcpURL, token, workspaceID)
	case knowledgeCapAuthFailure:
		d.logger.Warn("knowledge MCP disabled: auth failure against backend; check login", "url", mcpURL)
	case knowledgeCapUnsupported:
		d.logger.Info("knowledge MCP disabled: backend does not support knowledge service", "url", mcpURL)
	case knowledgeCapTransient:
		d.logger.Warn("knowledge MCP disabled: backend unreachable", "url", mcpURL)
	}
	return mcpConfig
}

// daemonProfileToken returns the auth token for the daemon's active profile.
// When the daemon has a named profile (d.cfg.Profile != ""), it reads the
// profile-scoped config (~/.multica/profiles/<profile>/config.json). When
// unset, it falls back to the default config (~/.multica/config.json).
// Returns empty string when the config file is missing or has no token.
func (d *Daemon) daemonProfileToken() string {
	cfg, err := cli.LoadCLIConfigForProfile(d.cfg.Profile)
	if err != nil {
		return ""
	}
	return cfg.Token
}

// knowledgeMCPCapability probes the backend at serverBaseURL (the backend root,
// e.g. "http://localhost:8080") for Knowledge MCP support, consulting and
// updating the probe cache. Concurrent callers for the same (serverURL, token)
// pair collapse to a single probe.
func (d *Daemon) knowledgeMCPCapability(serverBaseURL string) knowledgeCapability {
	token := d.daemonProfileToken()
	key := probeCacheKey(serverBaseURL, token)

	d.knowledgeProbeMu.Lock()
	if cap, ok := d.knowledgeProbeCache.get(key); ok {
		d.knowledgeProbeMu.Unlock()
		return cap
	}
	// Not cached — probe under the lock so concurrent callers for the same
	// key do not stampede the backend.
	client := &http.Client{Timeout: 10 * time.Second}
	cap := probeKnowledgeMCPCap(client, serverBaseURL, token)
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
// The baseURL should come from deriveKnowledgeMCPURL. The token is the active
// profile's backend auth token — never the daemon-local IPC/health token.
//
// The MCP wrapper is the multica binary itself (daemon mcp-knowledge subcommand),
// so Knowledge MCP works without system Node or a developer checkout.
func mergeKnowledgeMCP(existing json.RawMessage, baseURL, token, workspaceID string) json.RawMessage {
	envVars := map[string]string{
		"MULTICA_API_URL":      strings.TrimSuffix(baseURL, "/api/mcp"),
		"MULTICA_WORKSPACE_ID": workspaceID,
	}
	if token != "" {
		envVars["MULTICA_AUTH_TOKEN"] = token
	}
	ks := map[string]interface{}{
		"command": multicaBinaryPath(),
		"args":    []string{"daemon", "mcp-knowledge"},
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
	// DEPRECATED: kept for backward compat with older Node-based wrapper.
	// Production uses multicaBinaryPath() + ["daemon", "mcp-knowledge"].
	paths := []string{
		filepath.Join(filepath.Dir(os.Args[0]), "tools", "mcp-knowledge", "index.mjs"),
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// multicaBinaryPath returns the path to the running multica binary. This is
// used as the command for the Knowledge MCP stdio proxy (daemon mcp-knowledge),
// so the tool works without system Node or a developer checkout.
func multicaBinaryPath() string {
	// Use os.Executable which resolves the symlink to the real binary path.
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	// Fallback to os.Args[0] (may be a relative path).
	return os.Args[0]
}

// ProxyMCPRequest sends a JSON-RPC request to the backend /api/mcp and returns
// the response body. Used by the daemon mcp-knowledge subcommand.
func ProxyMCPRequest(client *http.Client, mcpURL, authToken, workspaceID string, body []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// jsonrpcMessage is a minimal JSON-RPC message for MCP stdio parsing.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RunMCPStdioServerWithIO implements a stdio MCP server that handles the Knowledge
// tool lifecycle, using the given reader/writer instead of os.Stdin/os.Stdout.
// This allows testing without replacing global stdin/stdout.
func RunMCPStdioServerWithIO(r io.Reader, w io.Writer, apiURL, authToken, workspaceID string) error {
	mcpURL := strings.TrimRight(apiURL, "/") + "/api/mcp"
	client := &http.Client{Timeout: 60 * time.Second}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			writeErrorTo(w, nil, -32700, "Parse error: "+err.Error())
			continue
		}

		if msg.ID == nil {
			continue
		}

		var resp jsonrpcResponse
		resp.JSONRPC = "2.0"
		resp.ID = msg.ID

		switch msg.Method {
		case "initialize":
			resp.Result = map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "multica-knowledge",
					"version": "1.0.0",
				},
			}

		case "ping":
			resp.Result = map[string]interface{}{}

		case "tools/list":
			resp.Result = map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "knowledge_search",
						"description": "Search indexed knowledge sources (Confluence, GitHub, etc.) linked to the current workspace.",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"query": map[string]interface{}{
									"type":        "string",
									"description": "Search query",
								},
								"limit": map[string]interface{}{
									"type":        "integer",
									"description": "Max results (default 10)",
								},
							},
							"required": []string{"query"},
						},
					},
				},
			}

		case "tools/call":
			result, err := handleToolCall(client, mcpURL, authToken, workspaceID, msg.Params)
			if err != nil {
				resp.Error = &jsonrpcError{Code: -32603, Message: err.Error()}
			} else {
				resp.Result = result
			}

		default:
			resp.Error = &jsonrpcError{
				Code:    -32601,
				Message: fmt.Sprintf("Method not found: %s", msg.Method),
			}
		}

		data, _ := json.Marshal(resp)
		w.Write(data)
		w.Write([]byte("\n"))
	}

	return scanner.Err()
}

// RunMCPStdioServer is like RunMCPStdioServerWithIO but uses os.Stdin/os.Stdout.
func RunMCPStdioServer(apiURL, authToken, workspaceID string) error {
	return RunMCPStdioServerWithIO(os.Stdin, os.Stdout, apiURL, authToken, workspaceID)
}

// handleToolCall processes a tools/call request by forwarding it to the backend
// /api/mcp endpoint.
func handleToolCall(client *http.Client, mcpURL, authToken, workspaceID string, params json.RawMessage) (interface{}, error) {
	// Parse the tool call params
	var callParams struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &callParams); err != nil {
		return nil, fmt.Errorf("invalid tool call params: %w", err)
	}

	if callParams.Name != "knowledge_search" {
		return nil, fmt.Errorf("Unknown tool: %s", callParams.Name)
	}

	// Build the backend request
	backendBody := map[string]interface{}{
		"method": "tools/call",
		"params": map[string]interface{}{
			"name":      "knowledge_search",
			"arguments": callParams.Arguments,
		},
	}
	bodyData, _ := json.Marshal(backendBody)

	respBody, err := ProxyMCPRequest(client, mcpURL, authToken, workspaceID, bodyData)
	if err != nil {
		return nil, fmt.Errorf("backend request failed: %w", err)
	}

	// Parse the backend response
	var backendResp struct {
		Result *struct {
			Content []map[string]interface{} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &backendResp); err != nil {
		return nil, fmt.Errorf("parse backend response: %w", err)
	}

	if backendResp.Error != nil {
		return nil, fmt.Errorf("search failed: %s", backendResp.Error.Message)
	}

	// Format the results for the agent
	results := backendResp.Result.Content
	text := "No results found."
	if len(results) > 0 {
		var parts []string
		for _, r := range results {
			title, _ := r["title"].(string)
			url, _ := r["url"].(string)
			score, _ := r["score"].(float64)
			snippet, _ := r["snippet"].(string)
			parts = append(parts, fmt.Sprintf("[%.2f] %s\n%s\n%s", score, title, url, snippet))
		}
		text = strings.Join(parts, "\n\n")
	}

	return map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": text},
		},
	}, nil
}

func writeErrorTo(w io.Writer, id json.RawMessage, code int, message string) {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: message},
	}
	data, _ := json.Marshal(resp)
	w.Write(data)
	w.Write([]byte("\n"))
}
