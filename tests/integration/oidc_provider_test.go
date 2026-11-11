package integration_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	// directory is what the admin API reports for each user ID (sub); a
	// missing entry is a deleted user.
	directory map[string]remoteUser
}

type remoteUser struct {
	enabled bool
	groups  []string
}

// sign returns a JWT signed with the provider's key.
func (p *identityProvider) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: p.key, KeyID: "test-key"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := signed.CompactSerialize()
	return compact
}

func (p *identityProvider) setUser(sub string, u remoteUser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.directory[sub] = u
}

func (p *identityProvider) removeUser(sub string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.directory, sub)
}

func (p *identityProvider) admin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sync-access-token" {
		w.WriteHeader(401)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/test/users/")
	sub, groups := strings.CutSuffix(rest, "/groups")
	p.mu.Lock()
	u, ok := p.directory[sub]
	p.mu.Unlock()
	if !ok {
		w.WriteHeader(404)
		return
	}
	if groups {
		list := []map[string]string{}
		for _, g := range u.groups {
			list = append(list, map[string]string{"id": "g-" + g, "name": g, "path": "/" + g})
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": sub, "enabled": u.enabled})
}

func newIdentityProvider(t *testing.T) *identityProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &identityProvider{key: key, codes: map[string]authorization{}, directory: map[string]remoteUser{}}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/realms/test/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.issuer, "authorization_endpoint": p.issuer + "/auth", "token_endpoint": p.issuer + "/token", "jwks_uri": p.issuer + "/jwks", "end_session_endpoint": p.issuer + "/logout", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/realms/test/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
		case "/realms/test/token":
			_ = r.ParseForm()
			client, secret, _ := r.BasicAuth()
			if client == "" {
				client, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
			}
			if r.Form.Get("grant_type") == "client_credentials" {
				if client != "olr-sync" || secret != "sync-secret" {
					http.Error(w, `{"error":"unauthorized_client"}`, http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "sync-access-token", "token_type": "Bearer", "expires_in": 300})
				return
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
			if strings.HasPrefix(r.URL.Path, "/admin/realms/test/users/") {
				p.admin(w, r)
				return
			}
			w.WriteHeader(404)
		}
	}))
	p.issuer = p.server.URL + "/realms/test"
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
