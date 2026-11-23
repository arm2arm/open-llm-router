package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamProvenancePassthrough(t *testing.T) {
	// An opaque test marker exercises preservation without prescribing a
	// provenance format or relying on a particular model provider.
	const marker = "synthetic-provider-marker"
	for _, tc := range []struct {
		name, contentType, request, upstream string
	}{
		{
			name:        "JSON",
			contentType: "application/json",
			request:     `{"model":"chat-model","messages":[]}`,
			upstream: `{
  "choices": [{"message":{"role":"assistant","content":"Flux λ.\nprint('λ')\n"}}],
  "provider_provenance": {"value":"` + marker + `"}
}
`,
		},
		{
			name:        "SSE",
			contentType: "text/event-stream",
			request:     `{"model":"chat-model","messages":[],"stream":true}`,
			upstream: "data: " + `{"choices":[{"delta":{"content":"Flux λ.\n"}}],"provider_provenance":{"value":"` + marker + `"}}` + "\n\n" +
				"data: " + `{"choices":[{"delta":{"content":"print('λ')\n"},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("X-Test-Upstream-Provenance", marker)
				_, _ = io.WriteString(w, tc.upstream)
			}))
			defer backend.Close()
			_, router := setup(t, backend.URL)
			resp, body := send(t, router.URL, "/v1/chat/completions", tc.request, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("unexpected status %d: %s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("X-Test-Upstream-Provenance"); got != marker {
				t.Errorf("upstream provenance header = %q, want %q", got, marker)
			}
			if string(body) != tc.upstream {
				t.Errorf("response bytes changed\ngot:  %q\nwant: %q", body, tc.upstream)
			}
		})
	}
}
