package daemon

import (
	"os"
	"testing"
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
