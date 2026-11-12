package idpcheck

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/go-jose/go-jose/v4"
)

// fakeKeycloak serves discovery, keys, client-credentials tokens,
// introspection and the admin users list for realm "test".
type fakeKeycloak struct {
	server    *httptest.Server
	key       *rsa.PrivateKey
	clients   map[string]string   // client ID -> secret
	audiences map[string][]string // client ID -> token audience
	canList   map[string]bool     // client ID -> may list users
	issuer    string              // advertised issuer; default the realm URL
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKeycloak{key: key, clients: map[string]string{"olr-portal": "portal-secret", "olr-sync": "sync-secret", "olr-service": "service-secret"},
		audiences: map[string][]string{"olr-service": {"olr-api"}, "olr-sync": {"realm-management"}}, canList: map[string]bool{"olr-sync": true}}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	realm := f.server.URL + "/realms/test"
	mux.HandleFunc("GET /realms/test/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := realm
		if f.issuer != "" {
			issuer = f.issuer
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": realm + "/auth", "token_endpoint": realm + "/token",
			"jwks_uri": realm + "/certs", "end_session_endpoint": realm + "/logout", "introspection_endpoint": realm + "/introspect",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /realms/test/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"}}})
	})
	authenticated := func(r *http.Request) (string, bool) {
		id, secret, ok := r.BasicAuth()
		if !ok {
			_ = r.ParseForm()
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		return id, id != "" && f.clients[id] == secret
	}
	mux.HandleFunc("POST /realms/test/introspect", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authenticated(r); !ok {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"active":false}`))
	})
	mux.HandleFunc("POST /realms/test/token", func(w http.ResponseWriter, r *http.Request) {
		id, ok := authenticated(r)
		if !ok || id == "olr-portal" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"unauthorized_client"}`))
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k"}}, nil)
		claims, _ := json.Marshal(map[string]any{"iss": realm, "sub": id, "azp": id, "aud": f.audiences[id], "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
		signed, _ := signer.Sign(claims)
		token, _ := signed.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 60})
	})
	mux.HandleFunc("GET /admin/realms/test/users", func(w http.ResponseWriter, r *http.Request) {
		// The fake accepts the sub of the bearer token without verifying it.
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parsed, err := jose.ParseSignedCompact(bearer, []jose.SignatureAlgorithm{jose.RS256})
		var claims struct{ Sub string }
		if err != nil || json.Unmarshal(parsed.UnsafePayloadWithoutVerification(), &claims) != nil {
			w.WriteHeader(401)
			return
		}
		if !f.canList[claims.Sub] {
			w.WriteHeader(403)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	return f
}

func (f *fakeKeycloak) input() Input {
	return Input{OIDC: &config.OIDC{Issuer: f.server.URL + "/realms/test", PortalClientID: "olr-portal", Audience: "olr-api", SyncClientID: "olr-sync"},
		PortalSecret: "portal-secret", SyncSecret: "sync-secret", TestClientID: "olr-service", TestClientSecret: "service-secret"}
}

func statuses(results []Result) map[string]string {
	out := map[string]string{}
	for _, r := range results {
		out[r.Name] = r.Status
	}
	return out
}

func TestChecksPassAndFailSpecifically(t *testing.T) {
	f := newFakeKeycloak(t)
	all := statuses(Run(t.Context(), f.input()))
	for _, name := range []string{Discovery, Logout, Portal, TokenAudience, SyncAccess} {
		if all[name] != Passed {
			t.Fatalf("%s: %v", name, all)
		}
	}
	secrets := []string{"portal-secret", "sync-secret", "service-secret", "wrong-secret-value"}
	for _, tc := range []struct {
		name   string
		mutate func(*Input, *fakeKeycloak)
		check  string
		status string
	}{
		{"wrong portal secret", func(in *Input, _ *fakeKeycloak) { in.PortalSecret = "wrong-secret-value" }, Portal, Failed},
		{"no portal secret", func(in *Input, _ *fakeKeycloak) { in.PortalSecret = "" }, Portal, Skipped},
		{"wrong sync secret", func(in *Input, _ *fakeKeycloak) { in.SyncSecret = "wrong-secret-value" }, SyncAccess, Failed},
		{"sync without view-users", func(_ *Input, f *fakeKeycloak) { f.canList["olr-sync"] = false }, SyncAccess, Failed},
		{"no sync secret", func(in *Input, _ *fakeKeycloak) { in.SyncSecret = "" }, SyncAccess, Skipped},
		{"token without audience", func(_ *Input, f *fakeKeycloak) { f.audiences["olr-service"] = []string{"account"} }, TokenAudience, Failed},
		{"wrong test client secret", func(in *Input, _ *fakeKeycloak) { in.TestClientSecret = "wrong-secret-value" }, TokenAudience, Failed},
		{"no test client", func(in *Input, _ *fakeKeycloak) { in.TestClientID = "" }, TokenAudience, Skipped},
		{"no audience", func(in *Input, _ *fakeKeycloak) { in.OIDC.Audience = "" }, TokenAudience, Skipped},
		{"not a Keycloak issuer path", func(in *Input, f *fakeKeycloak) { in.OIDC.BackchannelURL = f.server.URL + "/oidc" }, Discovery, Failed},
		{"issuer mismatch", func(_ *Input, f *fakeKeycloak) { f.issuer = "https://elsewhere.example/realms/test" }, Discovery, Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKeycloak(t)
			in := f.input()
			tc.mutate(&in, f)
			results := Run(t.Context(), in)
			if got := statuses(results)[tc.check]; got != tc.status {
				t.Fatalf("%s = %s, want %s: %v", tc.check, got, tc.status, results)
			}
			encoded, _ := json.Marshal(results)
			for _, secret := range secrets {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("a check result exposed a secret")
				}
			}
			if tc.name == "issuer mismatch" && !strings.Contains(string(encoded), "different issuer") {
				t.Fatal("issuer mismatch is not specific", string(encoded))
			}
		})
	}
}

func TestDestinationsAreRestrictedAtDialTime(t *testing.T) {
	for _, addr := range []string{"169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1", "fd00:ec2::254"} {
		if Allow(netip.MustParseAddr(addr)) == nil {
			t.Fatal("allowed", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.10", "203.0.113.7", "::1"} {
		if Allow(netip.MustParseAddr(addr)) != nil {
			t.Fatal("refused", addr)
		}
	}
	results := Run(t.Context(), Input{OIDC: &config.OIDC{Issuer: "http://169.254.169.254/realms/test", PortalClientID: "x"}})
	if results[0].Status != Failed || !strings.Contains(results[0].Message, "refused address") {
		t.Fatal(results)
	}
	if Run(t.Context(), Input{})[0].Status != Skipped {
		t.Fatal("disabled SSO must skip the checks")
	}
}
