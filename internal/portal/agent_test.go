package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
)

func TestAgentRegistryAndMCPPermissions(t *testing.T) {
	h := &Handler{}
	a := agentIdentity{Key: adminaccess.Key{Permissions: []string{"service:read"}}}
	seen := map[string]bool{}
	for _, op := range h.operations() {
		if seen[op.Name] {
			t.Fatal("duplicate operation", op.Name)
		}
		seen[op.Name] = true
		if op.Write && op.allowed(a) {
			t.Fatal("observer allowed write", op.Name)
		}
	}
	r := httptest.NewRequest("POST", "http://router/api/admin/v1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(agentTestContext(r.Context(), a))
	w := httptest.NewRecorder()
	h.agentMCP(w, r, a)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"name":"list_users"`) || !strings.Contains(w.Body.String(), `"name":"get_service_status"`) {
		t.Fatal("MCP permission discovery failed")
	}
}
func TestPreviewRedactionAndCanonicalBinding(t *testing.T) {
	raw := map[string]any{"body": map[string]any{"password": "private-password", "secrets": map[string]any{"client": "private-client"}, "nested": map[string]any{"bearer_token": "private-token"}}}
	b, _ := json.Marshal(sanitizeAgent(raw))
	if strings.Contains(string(b), "private-") {
		t.Fatal("preview exposed secrets")
	}
	a, _ := argumentHash(json.RawMessage(`{"body":{"enabled":false},"parameters":{"id":"x"}}`))
	b2, _ := argumentHash(json.RawMessage(`{"parameters":{"id":"x"},"body":{"enabled":false}}`))
	if string(a) != string(b2) {
		t.Fatal("object order changed digest")
	}
}

func TestMCPProtocolEnvelope(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name, body, version, contains string
		status                        int
	}{
		{"negotiated", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "", `"protocolVersion":"2025-06-18"`, 200},
		{"fallback", `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"future"}}`, "", `"protocolVersion":"2025-11-25"`, 200},
		{"unsupported header", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "1999-01-01", "unsupported MCP protocol version", 400},
		{"invalid id", `{"jsonrpc":"2.0","id":{},"method":"ping"}`, "", `"code":-32600`, 200},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, "", `"code":-32600`, 200},
		{"initialized", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "2025-11-25", "", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://router/api/admin/v1/mcp", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(agentTestContext(r.Context(), agentIdentity{}))
			if tc.version != "" {
				r.Header.Set("MCP-Protocol-Version", tc.version)
			}
			w := httptest.NewRecorder()
			h.agentMCP(w, r, agentIdentity{})
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
		})
	}
}

func agentTestContext(ctx context.Context, a agentIdentity) context.Context {
	return context.WithValue(ctx, agentContext{}, a)
}

func TestAgentAdmissionBudgets(t *testing.T) {
	l := &agentLimiter{}
	release, ok := l.acquire("key", 2, 1)
	if !ok {
		t.Fatal("first request refused")
	}
	if _, ok := l.acquire("key", 2, 1); ok {
		t.Fatal("concurrent limit bypassed")
	}
	release()
	release, ok = l.acquire("key", 2, 1)
	if !ok {
		t.Fatal("released concurrency slot not reusable")
	}
	release()
	if _, ok := l.acquire("key", 2, 1); ok {
		t.Fatal("per-minute budget bypassed")
	}
}
