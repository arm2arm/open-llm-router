package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestBackendCannotOverrideRouterCORS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if backendProbe(r) {
			return
		}
		for _, key := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded browser-only header %s", key)
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "X-Backend-Secret")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"backend busy"}}`)
	}))
	defer backend.Close()
	cfg := &config.Config{
		CORS: config.CORS{AllowedOrigins: []string{"https://ai.aip.de"}},
		Models: map[string]config.Model{"chat": {
			UpstreamModel: "model", Protocols: []string{config.ProtocolOpenAI}, TimeoutS: 30, IdleTimeoutS: 30,
			Pool: []config.Replica{{URL: backend.URL, HealthPath: "/health", Weight: 1, MaxInflight: 8}},
		}},
	}
	_, router := configured(t, cfg)
	response, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat"}`, http.Header{
		"Origin": {"https://ai.aip.de"}, "Access-Control-Request-Method": {"POST"}, "Access-Control-Request-Headers": {"authorization"},
	})
	if response.StatusCode != 429 || response.Header.Get("Access-Control-Allow-Origin") != "https://ai.aip.de" || response.Header.Get("Access-Control-Allow-Credentials") != "" || strings.Contains(response.Header.Get("Access-Control-Expose-Headers"), "Secret") {
		t.Fatalf("backend changed CORS: status %d, headers %v", response.StatusCode, response.Header)
	}
	vary := strings.Join(response.Header.Values("Vary"), ",")
	if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("lost Vary: %s", vary)
	}
}
