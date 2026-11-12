package oidcclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

func TestUnknownKidsBoundRefreshAndAllowRotation(t *testing.T) {
	oldKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	newKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var published atomic.Pointer[ecdsa.PrivateKey]
	published.Store(oldKey)
	var fetches atomic.Int64
	issuer := ""
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"ES256"}})
			return
		}
		fetches.Add(1)
		key, kid := published.Load(), "old"
		if key == newKey {
			kid = "new"
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"}}})
	}))
	defer s.Close()
	issuer = s.URL
	c, err := New(t.Context(), &config.OIDC{Issuer: issuer})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	limiter := c.HTTP.Transport.(*jwksThrottle)
	limiter.now = func() time.Time { return time.Unix(0, clock.Load()) }
	v := c.Provider.Verifier(&oidc.Config{ClientID: "api"})
	sign := func(key *ecdsa.PrivateKey, kid string) string {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "user", "aud": "api", "exp": time.Now().Add(time.Hour).Unix()})
		signed, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := signed.CompactSerialize()
		return raw
	}
	old := sign(oldKey, "old")
	if _, err := v.Verify(t.Context(), old); err != nil {
		t.Fatal("initial key fetch", err)
	}
	var wg sync.WaitGroup
	for i := range 128 {
		raw := sign(newKey, fmt.Sprint(i))
		wg.Go(func() {
			if _, err := v.Verify(t.Context(), raw); err == nil {
				t.Error("unknown kid accepted")
			}
		})
	}
	wg.Wait()
	// Sequential attacks cannot bypass go-oidc's parallel-fetch coalescing.
	for i := range 64 {
		if _, err := v.Verify(t.Context(), sign(newKey, fmt.Sprint(i))); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if _, err := v.Verify(t.Context(), sign(newKey, "old")); err == nil {
		t.Fatal("forged signature with a known kid accepted")
	}
	if fetches.Load() != 1 {
		t.Fatal("unknown kids caused repeated network requests", fetches.Load())
	}
	published.Store(newKey)
	rotated := sign(newKey, "new")
	if _, err := v.Verify(t.Context(), rotated); err == nil {
		t.Fatal("rotation bypassed refresh interval")
	}
	if _, err := v.Verify(t.Context(), old); err != nil {
		t.Fatal("cooldown blocked a cached signing key", err)
	}
	clock.Add(int64(jwksRefreshInterval))
	if _, err := v.Verify(t.Context(), rotated); err != nil || fetches.Load() != 2 {
		t.Fatal("rotation did not recover at fixed interval", err, fetches.Load())
	}
}

func TestJWKSFailureCooldownAndSlowBody(t *testing.T) {
	now := time.Now()
	calls := 0
	tt := &jwksThrottle{endpoint: "https://idp.example/keys", now: func() time.Time { return now },
		next: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		})}
	req := httptest.NewRequest("GET", tt.endpoint, nil)
	resp, err := tt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(jwksRefreshInterval)
	if _, err := tt.RoundTrip(req); err == nil {
		t.Fatal("slow response body allowed parallel refresh")
	}
	_ = resp.Body.Close()
	resp, err = tt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for range 100 {
		if _, err := tt.RoundTrip(req); err == nil {
			t.Fatal("failed fetch bypassed cooldown")
		}
	}
	if calls != 2 {
		t.Fatal("failed fetches were unbounded", calls)
	}
	// Token exchanges and other IdP operations retain their normal behavior.
	other := httptest.NewRequest("POST", "https://idp.example/token", nil)
	resp, err = tt.RoundTrip(other)
	if err != nil {
		t.Fatal("JWKS throttle affected token endpoint", err)
	}
	_ = resp.Body.Close()
}
