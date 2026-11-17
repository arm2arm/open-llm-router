package oidcclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestBackchannelKeepsIssuerAndLimitsRewrite(t *testing.T) {
	public, _ := url.Parse("https://login.example/realms/demo")
	internal, _ := url.Parse("http://keycloak:8080/realms/demo")
	for _, tc := range []struct{ input, want string }{
		{"https://login.example/realms/demo/.well-known/openid-configuration", "http://keycloak:8080/realms/demo/.well-known/openid-configuration"},
		{"https://login.example/realms/demo/token?x=1", "http://keycloak:8080/realms/demo/token?x=1"},
		{"https://login.example/realms/demo-evil/token", "https://login.example/realms/demo-evil/token"},
		{"https://elsewhere.example/realms/demo/token", "https://elsewhere.example/realms/demo/token"},
	} {
		r := httptest.NewRequest("GET", tc.input, nil)
		b := backchannel{public: public, internal: internal, next: roundTripFunc(func(forwarded *http.Request) (*http.Response, error) {
			if forwarded.URL.String() != tc.want {
				t.Fatal(forwarded.URL.String(), tc.want)
			}
			return &http.Response{StatusCode: 200}, nil
		})}
		if _, err := b.RoundTrip(r); err != nil {
			t.Fatal(err)
		}
		if r.URL.String() != tc.input || public.String() != "https://login.example/realms/demo" {
			t.Fatal("rewrite mutated expected issuer or input")
		}
	}
}

type countingReader struct{ read int }

func (r *countingReader) Read(p []byte) (int, error) {
	clear(p)
	r.read += len(p)
	return len(p), nil // An unbounded provider response without a Content-Length.
}

func TestIdentityResponseSizeBound(t *testing.T) {
	for _, length := range []int64{-1, maxResponseBytes + 1} {
		body := &countingReader{}
		transport := responseLimit{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: length, Body: io.NopCloser(body)}, nil
		})}
		resp, err := transport.RoundTrip(httptest.NewRequest("GET", "https://idp.example/keys", nil))
		if err == nil {
			var data []byte
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(data) > maxResponseBytes {
				t.Fatal("body buffered beyond limit")
			}
		}
		if !errors.Is(err, errResponseTooLarge) || body.read > maxResponseBytes+1 {
			t.Fatalf("unbounded response: read=%d error=%v", body.read, err)
		}
	}
	// A response exactly at the cap remains readable.
	b := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat(" ", maxResponseBytes))), remaining: maxResponseBytes}
	if data, err := io.ReadAll(b); err != nil || len(data) != maxResponseBytes {
		t.Fatal("exact-limit body refused", len(data), err)
	}
}

func TestOversizedDiscoveryFailsWithGenericError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.(http.Flusher).Flush() // Exercise a chunked body without Content-Length.
		_, _ = io.WriteString(w, strings.Repeat(" ", maxResponseBytes+1))
	}))
	defer s.Close()
	if _, err := New(t.Context(), &config.OIDC{Issuer: s.URL}); err == nil || err.Error() != "OIDC discovery failed" {
		t.Fatal("oversized discovery did not fail safely", err)
	}
}

func TestProductionTransportRejectsAdvertisedHTTP(t *testing.T) {
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://identity.invalid/token", nil)
	if _, err := (httpsOnly{next: http.DefaultTransport}).RoundTrip(req); err == nil {
		t.Fatal("HTTP provider endpoint accepted")
	}
}
