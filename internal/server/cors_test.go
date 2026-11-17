package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

const browserOrigin = "https://ai.aip.de"

func corsTestServer(enabled bool) (*Server, *atomic.Int32) {
	s, _ := newTestServer()
	if enabled {
		s.SetCORS(config.CORS{AllowedOrigins: []string{browserOrigin}, ExtraAllowedHeaders: []string{"X-Client-Name"}})
	}
	calls := new(atomic.Int32)
	s.SetAPIMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer browser-test-key" {
					w.Header().Set("Retry-After", "1")
					http.Error(w, "a valid API key is required", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	s.Handle("GET /v1/models", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	return s, calls
}

func TestCORSPreflightAndAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, origin, requestedMethod, requestedHeaders, key string
		enabled                                                            bool
		status, authCalls                                                  int
		allowOrigin                                                        bool
	}{
		{"Open WebUI models", "OPTIONS", "/v1/models", browserOrigin, "GET", "authorization,content-type", "", true, 204, 0, true},
		{"Open WebUI chat", "OPTIONS", "/v1/chat/completions", browserOrigin, "POST", "Authorization, Content-Type, X-Client-Name", "", true, 204, 0, true},
		{"Anthropic", "OPTIONS", "/v1/messages", browserOrigin, "POST", "x-api-key,anthropic-version,anthropic-beta", "", true, 204, 0, true},
		{"missing key", "GET", "/v1/models", browserOrigin, "", "", "", true, 401, 1, true},
		{"valid key", "GET", "/v1/models", browserOrigin, "", "", "browser-test-key", true, 200, 1, true},
		{"CLI unchanged", "GET", "/v1/models", "", "", "", "browser-test-key", true, 200, 1, false},
		{"CLI missing key", "GET", "/v1/models", "", "", "", "", true, 401, 1, false},
		{"disabled by default", "OPTIONS", "/v1/models", browserOrigin, "GET", "authorization", "", false, 401, 1, false},
		{"ordinary OPTIONS", "OPTIONS", "/v1/models", browserOrigin, "", "", "", true, 401, 1, true},
		{"no origin preflight", "OPTIONS", "/v1/models", "", "GET", "authorization", "", true, 401, 1, false},
		{"unapproved origin", "OPTIONS", "/v1/models", "https://evil.test", "GET", "authorization", "", true, 403, 0, false},
		{"suffix attack", "GET", "/v1/models", browserOrigin + ".evil.test", "", "", "browser-test-key", true, 403, 0, false},
		{"opaque origin", "OPTIONS", "/v1/models", "null", "GET", "", "", true, 403, 0, false},
		{"bad method", "OPTIONS", "/v1/models", browserOrigin, "DELETE", "authorization", "", true, 403, 0, true},
		{"empty method", "OPTIONS", "/v1/models", browserOrigin, " ", "authorization", "", true, 403, 0, true},
		{"bad header", "OPTIONS", "/v1/models", browserOrigin, "POST", "authorization,x-not-approved", "", true, 403, 0, true},
		{"empty header token", "OPTIONS", "/v1/models", browserOrigin, "POST", "authorization,", "", true, 403, 0, true},
		{"portal excluded", "OPTIONS", "/portal/api/me", browserOrigin, "GET", "authorization", "", true, 404, 0, false},
		{"admin excluded", "OPTIONS", "/api/admin/v1/users", browserOrigin, "GET", "authorization", "", true, 404, 0, false},
		{"health excluded", "GET", "/healthz", browserOrigin, "", "", "", true, 200, 0, false},
		{"prefix excluded", "OPTIONS", "/v10/models", browserOrigin, "GET", "authorization", "", true, 404, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, calls := corsTestServer(tc.enabled)
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.requestedMethod != "" {
				r.Header.Set("Access-Control-Request-Method", tc.requestedMethod)
			}
			if tc.requestedHeaders != "" {
				r.Header.Set("Access-Control-Request-Headers", tc.requestedHeaders)
			}
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || int(calls.Load()) != tc.authCalls {
				t.Fatalf("status %d, auth calls %d; body %s", w.Code, calls.Load(), w.Body.String())
			}
			wantOrigin := ""
			if tc.allowOrigin {
				wantOrigin = browserOrigin
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != wantOrigin {
				t.Fatalf("origin %q", got)
			}
			if w.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("enabled cookie credentials")
			}
			if w.Header().Get("X-Request-Id") == "" {
				t.Fatal("missing request ID")
			}
			if tc.status == 204 {
				if w.Body.Len() != 0 || w.Header().Get("Access-Control-Max-Age") != "600" || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
					t.Fatal("invalid preflight response")
				}
				vary := strings.Join(w.Header().Values("Vary"), ",")
				for _, field := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
					if !strings.Contains(vary, field) {
						t.Fatalf("missing Vary %s", field)
					}
				}
			}
			if tc.allowOrigin && !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
				t.Fatal("retry diagnostics not exposed")
			}
		})
	}
}

func TestCORSRepeatedHeaders(t *testing.T) {
	for _, field := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		s, calls := corsTestServer(true)
		r := httptest.NewRequest("OPTIONS", "/v1/models", nil)
		r.Header.Set("Origin", browserOrigin)
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "authorization")
		extra := map[string]string{"Origin": browserOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "x-unapproved"}[field]
		r.Header.Add(field, extra)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 403 || calls.Load() != 0 {
			t.Fatalf("%s: %d", field, w.Code)
		}
	}
}

func TestCORSStreamingAndCancellation(t *testing.T) {
	s, _ := corsTestServer(true)
	cancelled := make(chan struct{})
	s.Handle("POST /v1/chat/completions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		<-r.Context().Done()
		close(cancelled)
	}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	r, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", nil)
	r.Header.Set("Origin", browserOrigin)
	r.Header.Set("Authorization", "Bearer browser-test-key")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Access-Control-Allow-Origin") != browserOrigin {
		t.Fatal("stream has no CORS headers")
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("buffered stream: %q %v", line, err)
	}
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(wait):
		t.Fatal("browser disconnect did not cancel stream")
	}
}
