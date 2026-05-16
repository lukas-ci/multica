package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "some-token")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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

	cap := probeKnowledgeMCPCap(srv.Client(), srv.URL, "")
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
	cap := probeKnowledgeMCPCap(client, "http://127.0.0.1:1", "")
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

	probeKnowledgeMCPCap(srv.Client(), srv.URL, "test-token-123")
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
	cap := d.knowledgeMCPCapability(srv.URL)
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
	cap = d.knowledgeMCPCapability(srv.URL)
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

	cap := d.knowledgeMCPCapability(srv.URL)
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

	cap := d.knowledgeMCPCapability(srv.URL)
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
	cap := d.knowledgeMCPCapability("http://127.0.0.1:1")
	if cap != knowledgeCapTransient {
		t.Fatalf("expected transient, got %v", cap)
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
