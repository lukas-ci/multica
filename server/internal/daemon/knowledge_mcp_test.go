package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestDeriveKnowledgeMCPURL_deriveFromServerURL(t *testing.T) {
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://localhost:8080")
	want := "http://localhost:8080/api/mcp"
	if got != want {
		t.Fatalf("deriveKnowledgeMCPURL(%q) = %q, want %q", "http://localhost:8080", got, want)
	}
}

func TestDeriveKnowledgeMCPURL_deriveFromWSURL(t *testing.T) {
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://192.168.3.172:18080")
	want := "http://192.168.3.172:18080/api/mcp"
	if got != want {
		t.Fatalf("deriveKnowledgeMCPURL(%q) = %q, want %q", "http://192.168.3.172:18080", got, want)
	}
}

func TestDeriveKnowledgeMCPURL_overrideEnv(t *testing.T) {
	os.Setenv("MULTICA_KNOWLEDGE_MCP_URL", "http://override:9999/api/mcp")
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://localhost:8080")
	want := "http://override:9999/api/mcp"
	if got != want {
		t.Fatalf("deriveKnowledgeMCPURL with override = %q, want %q", got, want)
	}
}

func TestDeriveKnowledgeMCPURL_overrideWithDisable(t *testing.T) {
	os.Setenv("MULTICA_KNOWLEDGE_MCP_URL", "http://override:9999/api/mcp")
	os.Setenv("MULTICA_KNOWLEDGE_MCP_DISABLE", "1")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://localhost:8080")
	if got != "" {
		t.Fatalf("deriveKnowledgeMCPURL with override+disable = %q, want empty", got)
	}
}

func TestDeriveKnowledgeMCPURL_disableOnly(t *testing.T) {
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
	os.Setenv("MULTICA_KNOWLEDGE_MCP_DISABLE", "1")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://localhost:8080")
	if got != "" {
		t.Fatalf("deriveKnowledgeMCPURL with disable = %q, want empty", got)
	}
}

func TestDeriveKnowledgeMCPURL_emptyServerURL(t *testing.T) {
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("")
	if got != "" {
		t.Fatalf("deriveKnowledgeMCPURL(\"\") = %q, want empty", got)
	}
}

// ----- Capability probe tests -----

func TestProbeKnowledgeMCPCap_capabilities200(t *testing.T) {
	// /api/capabilities returns 200 → supported without hitting /api/mcp
	var mcpCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(200)
			w.Write([]byte(`{"knowledge":true}`))
			return
		}
		mcpCalled.Store(true)
		w.WriteHeader(404)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapSupported {
		t.Fatalf("expected supported, got %v", cap)
	}
	if mcpCalled.Load() {
		t.Fatal("unexpected /api/mcp call — capabilities 200 should short-circuit")
	}
}

func TestProbeKnowledgeMCPCap_capabilities404_fallbackToMCP_supported(t *testing.T) {
	// /api/capabilities 404 → falls through to /api/mcp which returns knowledge_search
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search","description":"..."}]}}`))
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapSupported {
		t.Fatalf("expected supported, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_mcp200_noKnowledgeSearch(t *testing.T) {
	// /api/mcp returns 200 but no knowledge_search tool → unsupported
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"result":{"tools":[{"name":"some_other_tool","description":"..."}]}}`))
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapUnsupported {
		t.Fatalf("expected unsupported, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_mcp401(t *testing.T) {
	// /api/mcp returns 401 → auth failure
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "some-token", "")
	if cap != knowledgeCapAuthFailure {
		t.Fatalf("expected auth_failure, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_mcp404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapUnsupported {
		t.Fatalf("expected unsupported, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_mcp405(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(405)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapUnsupported {
		t.Fatalf("expected unsupported, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_mcp5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapTransient {
		t.Fatalf("expected transient, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_capabilities5xx(t *testing.T) {
	// /api/capabilities 503 → transient without touching /api/mcp
	var mcpCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(503)
			return
		}
		mcpCalled.Store(true)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "", "")
	if cap != knowledgeCapTransient {
		t.Fatalf("expected transient, got %v", cap)
	}
	if mcpCalled.Load() {
		t.Fatal("unexpected /api/mcp call — capabilities 503 should short-circuit")
	}
}

func TestProbeKnowledgeMCPCap_networkError(t *testing.T) {
	// Point at a server that does not exist → transient
	client := &http.Client{Timeout: time.Second}
	cap := probeKnowledgeMCPCap(client, "http://127.0.0.1:1", "", "")
	if cap != knowledgeCapTransient {
		t.Fatalf("expected transient, got %v", cap)
	}
}

func TestProbeKnowledgeMCPCap_authorizationHeader(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()

	probeKnowledgeMCPCap(srv.Client(), srv.URL, "test-token-123", "")
	if gotToken != "Bearer test-token-123" {
		t.Fatalf("expected Authorization: Bearer test-token-123, got %q", gotToken)
	}
}

// ----- Cache tests -----

func TestKnowledgeProbeCache_supported(t *testing.T) {
	cache := newKnowledgeProbeCache()
	key := "http://test:8080|abc1"

	cache.set(key, knowledgeCapSupported)
	cap, ok := cache.get(key)
	if !ok || cap != knowledgeCapSupported {
		t.Fatalf("expected supported from cache, got %v (ok=%v)", cap, ok)
	}
}

func TestKnowledgeProbeCache_unsupported_expires(t *testing.T) {
	cache := newKnowledgeProbeCache()
	key := "http://test:8080|abc1"

	cache.set(key, knowledgeCapUnsupported)
	cap, ok := cache.get(key)
	if !ok || cap != knowledgeCapUnsupported {
		t.Fatalf("expected unsupported from cache, got %v (ok=%v)", cap, ok)
	}

	// Fake the entry age past the TTL
	cache.mu.Lock()
	cache.store[key].at = time.Now().Add(-(cacheTTLUnsupported + time.Second))
	cache.mu.Unlock()

	cap, ok = cache.get(key)
	if ok {
		t.Fatalf("expected expired entry to be removed, got %v", cap)
	}
}

func TestKnowledgeProbeCache_transient_notCached(t *testing.T) {
	cache := newKnowledgeProbeCache()
	key := "http://test:8080|abc1"

	cache.set(key, knowledgeCapTransient)
	_, ok := cache.get(key)
	if ok {
		t.Fatal("expected transient result to not be cached")
	}
}

func TestKnowledgeProbeCache_keyUniqueness(t *testing.T) {
	cache := newKnowledgeProbeCache()
	key1 := probeCacheKey("http://a:1", "tok1")
	key2 := probeCacheKey("http://a:1", "tok2")

	if key1 == key2 {
		t.Fatal("expected different tokens to produce different cache keys")
	}

	cache.set(key1, knowledgeCapSupported)
	cache.set(key2, knowledgeCapUnsupported)

	cap1, ok1 := cache.get(key1)
	cap2, ok2 := cache.get(key2)
	if !ok1 || cap1 != knowledgeCapSupported {
		t.Fatalf("key1: expected supported, got %v (ok=%v)", cap1, ok1)
	}
	if !ok2 || cap2 != knowledgeCapUnsupported {
		t.Fatalf("key2: expected unsupported, got %v (ok=%v)", cap2, ok2)
	}
}

func TestKnowledgeProbeCache_emptyTokenKey(t *testing.T) {
	key1 := probeCacheKey("http://test:8080", "")
	key2 := probeCacheKey("http://test:8080", "")
	if key1 != key2 {
		t.Fatalf("empty token keys differ: %q vs %q", key1, key2)
	}
	if key1 != "http://test:8080|" {
		t.Fatalf("expected 'http://test:8080|', got %q", key1)
	}
}

func TestContainsKnowledgeSearch(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		expect bool
	}{
		{"has knowledge_search", `{"result":{"tools":[{"name":"knowledge_search","description":"search"}]}}`, true},
		{"no knowledge_search", `{"result":{"tools":[{"name":"other_tool","description":"other"}]}}`, false},
		{"empty tools", `{"result":{"tools":[]}}`, false},
		{"malformed", `not-json`, false},
		{"empty body", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := containsKnowledgeSearch(strings.NewReader(tc.json))
			if got != tc.expect {
				t.Fatalf("containsKnowledgeSearch = %v, want %v", got, tc.expect)
			}
		})
	}
}

// Wiring-level test: prove that knowledgeMCPCapability with a configured
// server root URL probes /api/capabilities and /api/mcp correctly (not
// /api/mcp/api/capabilities), then returns supported when the backend
// advertises knowledge_search.
func TestKnowledgeMCPCapability_wiring(t *testing.T) {
	var capabilitiesCalls atomic.Int32
	var mcpCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			capabilitiesCalls.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"knowledge":true}`))
		case "/api/mcp":
			mcpCalls.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search","description":"search"}]}}`))
		default:
			t.Logf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Daemon{
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}
	d.cfg.ServerBaseURL = srv.URL

	// First call: probe, cache miss
	cap := d.knowledgeMCPCapability(srv.URL, "")
	if cap != knowledgeCapSupported {
		t.Fatalf("first call: expected supported, got %v", cap)
	}
	if capabilitiesCalls.Load() != 1 {
		t.Fatalf("expected 1 /api/capabilities call, got %d", capabilitiesCalls.Load())
	}
	if mcpCalls.Load() != 0 {
		t.Fatalf("expected 0 /api/mcp calls (capabilities 200 shortcut), got %d", mcpCalls.Load())
	}

	// Second call: should use cache, no new requests
	cap = d.knowledgeMCPCapability(srv.URL, "")
	if cap != knowledgeCapSupported {
		t.Fatalf("second call (cached): expected supported, got %v", cap)
	}
	if capabilitiesCalls.Load() != 1 {
		t.Fatalf("expected still 1 /api/capabilities call (cached), got %d", capabilitiesCalls.Load())
	}
}

// Wiring test: capabilities 404 → falls through to /api/mcp, result = supported.
func TestKnowledgeMCPCapability_wiring_fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			w.WriteHeader(404)
		case "/api/mcp":
			w.WriteHeader(200)
			w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search","description":"search"}]}}`))
		default:
			t.Logf("unexpected: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Daemon{
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	cap := d.knowledgeMCPCapability(srv.URL, "")
	if cap != knowledgeCapSupported {
		t.Fatalf("expected supported via /api/mcp fallback, got %v", cap)
	}
}

// Wiring test: unsupported backend (MCP returns 404) → unsupported result.
func TestKnowledgeMCPCapability_wiring_unsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			w.WriteHeader(404)
		case "/api/mcp":
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Daemon{
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	cap := d.knowledgeMCPCapability(srv.URL, "")
	if cap != knowledgeCapUnsupported {
		t.Fatalf("expected unsupported, got %v", cap)
	}
}

// Wiring test: backend unreachable → transient.
func TestKnowledgeMCPCapability_wiring_transient(t *testing.T) {
	d := &Daemon{
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	// Point at a port that will refuse/not connect
	cap := d.knowledgeMCPCapability("http://127.0.0.1:1", "")
	if cap != knowledgeCapTransient {
		t.Fatalf("expected transient, got %v", cap)
	}
}

// Regression test: injectKnowledgeMCP exercises the exact composition path
// used by runTask. It must probe /api/capabilities (not /api/mcp/api/...).
func TestInjectKnowledgeMCP_regression(t *testing.T) {
	var capabilitiesCalls atomic.Int32
	var mcpCalls atomic.Int32
	var wrongPathCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			capabilitiesCalls.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"knowledge":true}`))
		case "/api/mcp":
			mcpCalls.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search","description":"search"}]}}`))
		default:
			// If the probe hits /api/mcp/api/capabilities or similar, flag it
			if strings.Contains(r.URL.Path, "/api/mcp/api/") {
				wrongPathCalls.Add(1)
			}
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Daemon{
		cfg: Config{
			ServerBaseURL: srv.URL,
		},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	result := d.injectKnowledgeMCP(nil, "test-ws")

	if capabilitiesCalls.Load() != 1 {
		t.Fatalf("expected 1 /api/capabilities call, got %d; probes went to: %s",
			capabilitiesCalls.Load(), srv.URL)
	}
	if wrongPathCalls.Load() > 0 {
		t.Fatalf("probe hit double-appended path (e.g. /api/mcp/api/capabilities) %d times",
			wrongPathCalls.Load())
	}
	if mcpCalls.Load() != 0 {
		t.Fatalf("expected 0 /api/mcp calls (capabilities 200 shortcut), got %d", mcpCalls.Load())
	}
	if result == nil {
		t.Fatal("injectKnowledgeMCP returned nil, expected knowledge MCP config")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]interface{})
	if !ok {
		t.Fatalf("result missing mcpServers key: %v", parsed)
	}
	if _, ok := servers["knowledge"]; !ok {
		t.Fatalf("result missing knowledge tool in mcpServers: %v", servers)
	}
}

func TestInjectKnowledgeMCP_fallbackRegression(t *testing.T) {
	// /api/capabilities 404 → fallback to /api/mcp → supported
	var capabilitiesCalls atomic.Int32
	var mcpCalls atomic.Int32
	var wrongPathCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/capabilities":
			capabilitiesCalls.Add(1)
			w.WriteHeader(404)
		case "/api/mcp":
			mcpCalls.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search","description":"search"}]}}`))
		default:
			if strings.Contains(r.URL.Path, "/api/mcp/api/") {
				wrongPathCalls.Add(1)
			}
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &Daemon{
		cfg: Config{
			ServerBaseURL: srv.URL,
		},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	result := d.injectKnowledgeMCP(nil, "test-ws")

	if capabilitiesCalls.Load() != 1 {
		t.Fatalf("expected 1 /api/capabilities call, got %d", capabilitiesCalls.Load())
	}
	if mcpCalls.Load() != 1 {
		t.Fatalf("expected 1 /api/mcp call (fallback), got %d", mcpCalls.Load())
	}
	if wrongPathCalls.Load() > 0 {
		t.Fatalf("probe hit double-appended path %d times", wrongPathCalls.Load())
	}
	if result == nil {
		t.Fatal("injectKnowledgeMCP returned nil, expected knowledge MCP config")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]interface{})
	if !ok || servers["knowledge"] == nil {
		t.Fatalf("result missing knowledge tool: %v", parsed)
	}
}

func TestInjectKnowledgeMCP_unsupportedBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	d := &Daemon{
		cfg: Config{
			ServerBaseURL: srv.URL,
		},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	result := d.injectKnowledgeMCP(json.RawMessage(`{"mcpServers":{"existing":{"command":"echo","args":["hi"]}}}`), "test-ws")
	if result == nil {
		t.Fatal("expected unchanged config, got nil")
	}
	if !strings.Contains(string(result), "existing") {
		t.Fatal("expected existing MCP config preserved when knowledge disabled")
	}
	if strings.Contains(string(result), "knowledge_search") {
		t.Fatal("knowledge_search should not be injected for unsupported backend")
	}
}

func TestInjectKnowledgeMCP_derivedURLDisable(t *testing.T) {
	// When deriveKnowledgeMCPURL returns empty (disabled), injectKnowledgeMCP
	// must return mcpConfig unchanged without probing.
	os.Setenv("MULTICA_KNOWLEDGE_MCP_DISABLE", "1")
	defer os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")

	d := &Daemon{
		cfg: Config{
			ServerBaseURL: "http://localhost:8080",
		},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	original := json.RawMessage(`{"mcpServers":{"existing":{"command":"echo"}}}`)
	result := d.injectKnowledgeMCP(original, "test-ws")
	if string(result) != string(original) {
		t.Fatalf("expected unchanged config when disabled, got %s", result)
	}
}

// ----- Profile-scoped token tests -----

// setupTestMulticaHome creates a temporary ~/.multica directory with the given
// profile configs and returns the home dir path. Caller restores HOME in Cleanup.
func setupTestMulticaHome(t *testing.T, defaultToken string, profileTokens map[string]string) string {
	t.Helper()
	home := t.TempDir()
	multicaDir := filepath.Join(home, ".multica")
	if err := os.MkdirAll(multicaDir, 0o755); err != nil {
		t.Fatalf("mkdir .multica: %v", err)
	}
	// Write default config
	if defaultToken != "" {
		cfg := cli.CLIConfig{Token: defaultToken}
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(multicaDir, "config.json"), data, 0o644); err != nil {
			t.Fatalf("write default config: %v", err)
		}
	}
	// Write profile configs
	for name, token := range profileTokens {
		profDir := filepath.Join(multicaDir, "profiles", name)
		if err := os.MkdirAll(profDir, 0o755); err != nil {
			t.Fatalf("mkdir profile %s: %v", name, err)
		}
		cfg := cli.CLIConfig{Token: token}
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(profDir, "config.json"), data, 0o644); err != nil {
			t.Fatalf("write profile config %s: %v", name, err)
		}
	}
	return home
}

func TestDaemonProfileToken_defaultProfile(t *testing.T) {
	home := setupTestMulticaHome(t, "default-token-abc", nil)
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	d := &Daemon{cfg: Config{Profile: ""}}
	got := d.daemonProfileToken()
	if got != "default-token-abc" {
		t.Fatalf("expected default-token-abc, got %q", got)
	}
}

func TestDaemonProfileToken_namedProfile(t *testing.T) {
	home := setupTestMulticaHome(t, "default-token-abc", map[string]string{
		"desktop-staging": "profile-token-xyz",
	})
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	d := &Daemon{cfg: Config{Profile: "desktop-staging"}}
	got := d.daemonProfileToken()
	if got != "profile-token-xyz" {
		t.Fatalf("expected profile-token-xyz, got %q", got)
	}

	// Confirm NOT falling back to default token
	if got == "default-token-abc" {
		t.Fatal("daemonProfileToken returned default token instead of profile-scoped token")
	}
}

func TestDaemonProfileToken_missingFile(t *testing.T) {
	home := t.TempDir() // no .multica at all
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	d := &Daemon{cfg: Config{Profile: "nonexistent"}}
	got := d.daemonProfileToken()
	if got != "" {
		t.Fatalf("expected empty token for missing profile, got %q", got)
	}
}

func TestDaemonProfileToken_noTokenInConfig(t *testing.T) {
	home := setupTestMulticaHome(t, "", map[string]string{"test": ""})
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	d := &Daemon{cfg: Config{Profile: "test"}}
	got := d.daemonProfileToken()
	if got != "" {
		t.Fatalf("expected empty token when profile has no token, got %q", got)
	}
}

func TestDaemonProfileToken_defaultVsProfileIsolation(t *testing.T) {
	// Both have different tokens — verify isolation
	home := setupTestMulticaHome(t, "default-token", map[string]string{
		"prod": "prod-token",
		"stg":  "stg-token",
	})
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	d := &Daemon{cfg: Config{Profile: "stg"}}
	got := d.daemonProfileToken()
	if got != "stg-token" {
		t.Fatalf("expected stg-token, got %q", got)
	}
}

func TestInjectKnowledgeMCP_usesProfileToken(t *testing.T) {
	// Verify the profile-scoped token flows through to mergeKnowledgeMCP
	// by creating a live injectKnowledgeMCP call and checking the output.
	home := setupTestMulticaHome(t, "", map[string]string{
		"eng2-test": "eng2-profile-token",
	})
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", origHome)

	var receivedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedToken = r.Header.Get("Authorization")
		if r.URL.Path == "/api/capabilities" {
			w.WriteHeader(200)
			w.Write([]byte(`{"knowledge":true}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	d := &Daemon{
		cfg: Config{
			ServerBaseURL: srv.URL,
			Profile:       "eng2-test",
		},
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		knowledgeProbeCache: newKnowledgeProbeCache(),
	}

	result := d.injectKnowledgeMCP(nil, "ws-1")
	if result == nil {
		t.Fatal("injectKnowledgeMCP returned nil")
	}

	// Token was sent in Authorization header to probe
	if receivedToken != "Bearer eng2-profile-token" {
		t.Fatalf("expected Bearer eng2-profile-token in probe, got %q", receivedToken)
	}

	// Token is also in the MCP config env
	var parsed struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	ks, ok := parsed.McpServers["knowledge"]
	if !ok {
		t.Fatal("result missing knowledge MCP server")
	}
	if ks.Env["MULTICA_AUTH_TOKEN"] != "eng2-profile-token" {
		t.Fatalf("expected MULTICA_AUTH_TOKEN=eng2-profile-token in env, got %q", ks.Env["MULTICA_AUTH_TOKEN"])
	}
}

func TestDeriveKnowledgeMCPURL_trailingSlash(t *testing.T) {
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
	os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	t.Cleanup(func() {
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_URL")
		os.Unsetenv("MULTICA_KNOWLEDGE_MCP_DISABLE")
	})

	got := deriveKnowledgeMCPURL("http://localhost:8080/")
	want := "http://localhost:8080/api/mcp"
	if got != want {
		t.Fatalf("deriveKnowledgeMCPURL with trailing slash = %q, want %q", got, want)
	}
}

// ----- Task 5: MCP packaging tests -----

func TestMulticaBinaryPath_returnsNonEmpty(t *testing.T) {
	path := multicaBinaryPath()
	if path == "" {
		t.Fatal("multicaBinaryPath returned empty string")
	}
}

func TestMergeKnowledgeMCP_usesMulticaBinary(t *testing.T) {
	result := mergeKnowledgeMCP(nil, "http://test:8080", "test-token", "ws-1")
	if result == nil {
		t.Fatal("mergeKnowledgeMCP returned nil")
	}

	var parsed struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	ks, ok := parsed.McpServers["knowledge"]
	if !ok {
		t.Fatal("result missing knowledge MCP server")
	}

	if ks.Command == "node" {
		t.Fatal("injected config uses 'node' command — should use multica binary")
	}
	if len(ks.Args) < 2 || ks.Args[0] != "daemon" || ks.Args[1] != "mcp-knowledge" {
		t.Fatalf("expected args [daemon mcp-knowledge], got %v", ks.Args)
	}
	if ks.Env["MULTICA_AUTH_TOKEN"] != "test-token" {
		t.Fatalf("expected MULTICA_AUTH_TOKEN=test-token, got %q", ks.Env["MULTICA_AUTH_TOKEN"])
	}
	if ks.Env["MULTICA_API_URL"] != "http://test:8080" {
		t.Fatalf("expected MULTICA_API_URL=http://test:8080, got %q", ks.Env["MULTICA_API_URL"])
	}
	if ks.Env["MULTICA_WORKSPACE_ID"] != "ws-1" {
		t.Fatalf("expected MULTICA_WORKSPACE_ID=ws-1, got %q", ks.Env["MULTICA_WORKSPACE_ID"])
	}
}

func TestProxyMCPRequest_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("X-Workspace-ID") != "ws-1" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"result":{"tools":[{"name":"knowledge_search"}]}}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	resp, err := ProxyMCPRequest(client, srv.URL+"/api/mcp", "test-token", "ws-1", body)
	if err != nil {
		t.Fatalf("proxyMCPRequest returned error: %v", err)
	}
	if !strings.Contains(string(resp), "knowledge_search") {
		t.Fatalf("response missing knowledge_search: %s", resp)
	}
}

func TestProxyMCPRequest_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_, err := ProxyMCPRequest(client, srv.URL+"/api/mcp", "bad-token", "ws-1", body)
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected error mentioning 401, got: %v", err)
	}
}

func TestProxyMCPRequest_networkError(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_, err := ProxyMCPRequest(client, "http://127.0.0.1:1/api/mcp", "", "", body)
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

// ----- MCP protocol tests -----

func TestRunMCPStdioServer_initialize(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
`
	output, err := runMCPWithIO(input, "")
	if err != nil {
		t.Fatalf("runMCPWithIO failed: %v", err)
	}

	// Should get one response (initialize), notification produces none
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d: %q", len(lines), output)
	}

	var resp struct {
		ID     float64                `json:"id"`
		Result map[string]interface{} `json:"result"`
		Error  interface{}            `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.ID != 1 {
		t.Fatalf("expected id 1, got %v", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected result for initialize")
	}
}

func TestRunMCPStdioServer_toolsList(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`
	output, err := runMCPWithIO(input, "")
	if err != nil {
		t.Fatalf("runMCPWithIO failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d", len(lines))
	}

	var resp struct {
		ID     float64 `json:"id"`
		Result *struct {
			Tools []map[string]interface{} `json:"tools"`
		} `json:"result"`
		Error interface{} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil || len(resp.Result.Tools) == 0 {
		t.Fatal("expected tools in result")
	}
	found := false
	for _, tool := range resp.Result.Tools {
		if tool["name"] == "knowledge_search" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected knowledge_search in tools list")
	}
}

func TestRunMCPStdioServer_unknownMethod(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":5,"method":"unknown_method"}
`
	output, err := runMCPWithIO(input, "")
	if err != nil {
		t.Fatalf("runMCPWithIO failed: %v", err)
	}

	var resp struct {
		ID    float64      `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("expected error code -32601, got %d", resp.Error.Code)
	}
	if resp.ID != 5 {
		t.Fatalf("expected id 5 preserved in error, got %v", resp.ID)
	}
}

func TestRunMCPStdioServer_toolCall(t *testing.T) {
	// Start a backend server that returns knowledge results
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"result":{"content":[{"title":"Test Result","url":"http://example.com","score":0.95,"snippet":"A test result"}]}}`))
	}))
	defer backendSrv.Close()

	input := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"test query"}}}
`
	output, err := runMCPWithIO(input, backendSrv.URL)
	if err != nil {
		t.Fatalf("runMCPWithIO failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d: %s", len(lines), output)
	}

	var resp struct {
		ID     float64 `json:"id"`
		Result *struct {
			Content []map[string]interface{} `json:"content"`
		} `json:"result"`
		Error interface{} `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		t.Fatal("expected content in tool call result")
	}
	if resp.ID != 3 {
		t.Fatalf("expected id 3, got %v", resp.ID)
	}
}

func TestRunMCPStdioServer_invalidJSON(t *testing.T) {
	input := `not valid json
`
	output, err := runMCPWithIO(input, "")
	if err != nil {
		t.Fatalf("runMCPWithIO failed: %v", err)
	}

	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if resp.Error.Code != -32700 {
		t.Fatalf("expected parse error code -32700, got %d", resp.Error.Code)
	}
}

// runMCPWithIO is a test helper that runs RunMCPStdioServerWithIO with the given
// stdin input and returns the stdout output.
func runMCPWithIO(input, apiURL string) (string, error) {
	if apiURL == "" {
		apiURL = "http://test-backend:8080"
	}

	var buf bytes.Buffer
	err := RunMCPStdioServerWithIO(
		strings.NewReader(input),
		&buf,
		apiURL,
		"test-token",
		"test-ws",
	)
	return buf.String(), err
}
