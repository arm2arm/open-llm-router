package oidcclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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

func TestProductionTransportRejectsAdvertisedHTTP(t *testing.T) {
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "http://identity.invalid/token", nil)
	if _, err := (httpsOnly{next: http.DefaultTransport}).RoundTrip(req); err == nil {
		t.Fatal("HTTP provider endpoint accepted")
	}
}
