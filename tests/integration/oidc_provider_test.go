package integration_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

type authorization struct {
	claims              map[string]any
	challenge, redirect string
}
type identityProvider struct {
	server *httptest.Server
	issuer string
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]authorization
}

func newIdentityProvider(t *testing.T) *identityProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &identityProvider{key: key, codes: map[string]authorization{}}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/issuer/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.issuer, "authorization_endpoint": p.issuer + "/auth", "token_endpoint": p.issuer + "/token", "jwks_uri": p.issuer + "/jwks", "end_session_endpoint": p.issuer + "/logout", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/issuer/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
		case "/issuer/token":
			_ = r.ParseForm()
			client, secret, _ := r.BasicAuth()
			if client == "" {
				client, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
			}
			p.mu.Lock()
			a, ok := p.codes[r.Form.Get("code")]
			delete(p.codes, r.Form.Get("code"))
			p.mu.Unlock()
			if !ok || client != "olr-portal" || secret != "test-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != a.redirect || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != a.challenge {
				http.Error(w, "invalid exchange", 400)
				return
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "test-key"}}, nil)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			payload, _ := json.Marshal(a.claims)
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			compact, _ := signed.CompactSerialize()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "private-provider-token-marker", "token_type": "Bearer", "expires_in": 300, "id_token": compact})
		default:
			w.WriteHeader(404)
		}
	}))
	p.issuer = p.server.URL + "/issuer"
	t.Cleanup(p.server.Close)
	return p
}

func (p *identityProvider) authorize(t *testing.T, q url.Values, subject string, groups []string, alter func(map[string]any)) string {
	t.Helper()
	if q.Get("response_type") != "code" || q.Get("client_id") != "olr-portal" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("authorization request is missing code, state, nonce or PKCE")
	}
	claims := map[string]any{"iss": p.issuer, "sub": subject, "aud": "olr-portal", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce"), "preferred_username": subject, "email": "shared@example.test", "groups": groups}
	if alter != nil {
		alter(claims)
	}
	code := rand.Text()
	p.mu.Lock()
	p.codes[code] = authorization{claims: claims, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	p.mu.Unlock()
	return code
}
