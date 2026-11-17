package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBrowserCORSRetainsRealKeyAndModelAuthorization(t *testing.T) {
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), usageOptions{group: &config.Group{Models: []string{}}, configure: func(_ *config.Config, _ *proxy.Handler, s *server.Server, _ *pgxpool.Pool) {
		s.SetCORS(config.CORS{AllowedOrigins: []string{"https://ai.aip.de"}})
	}})
	for _, tc := range []struct {
		method, path, key string
		want              int
	}{
		{"OPTIONS", "/v1/models", "", 204},
		{"GET", "/v1/models", "", 401},
		{"GET", "/v1/models", "synthetic-invalid-key", 401},
		{"GET", "/v1/models", f.key, 200},
		{"POST", "/v1/chat/completions", f.key, 403},
		{"GET", "/v1/models/forbidden", f.key, 404},
	} {
		var body io.Reader
		if tc.method == "POST" {
			body = strings.NewReader(`{"model":"chat-model"}`)
		}
		r, err := http.NewRequest(tc.method, f.router.URL+tc.path, body)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", "https://ai.aip.de")
		if tc.method == "OPTIONS" {
			r.Header.Set("Access-Control-Request-Method", "GET")
			r.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
		}
		if tc.key != "" {
			r.Header.Set("Authorization", "Bearer "+tc.key)
		}
		response, err := f.router.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != tc.want || response.Header.Get("Access-Control-Allow-Origin") != "https://ai.aip.de" {
			t.Fatalf("%s %s: status=%d, CORS origin=%q", tc.method, tc.path, response.StatusCode, response.Header.Get("Access-Control-Allow-Origin"))
		}
	}
	// Disable this real key's owner: CORS permission must not grant model access.
	if err := (auth.Admin{Pool: f.pool}).DisableUser(t.Context(), f.owner); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", f.router.URL+"/v1/models", nil)
	r.Header.Set("Origin", "https://ai.aip.de")
	r.Header.Set("Authorization", "Bearer "+f.key)
	response, err := f.router.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 401 || response.Header.Get("Access-Control-Allow-Origin") != "https://ai.aip.de" {
		t.Fatalf("status=%d", response.StatusCode)
	}
}
