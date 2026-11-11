package integration_test

import (
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apitoken"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tokenFixture struct {
	*usageRouter
	provider *identityProvider
	oidc     *config.OIDC
}

func startTokenRouter(t *testing.T) *tokenFixture {
	t.Helper()
	p := newIdentityProvider(t)
	f := &tokenFixture{provider: p, oidc: &config.OIDC{Issuer: p.issuer, PortalClientID: "olr-portal", GroupsClaim: "groups",
		UsernameClaim: "preferred_username", Audience: "olr-api", SyncClientID: "olr-sync", SyncIntervalS: 60}}
	f.usageRouter = startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 3}), usageOptions{
		group: &config.Group{Models: []string{"*"}},
		configure: func(cfg *config.Config, _ *proxy.Handler, _ *server.Server, pool *pgxpool.Pool) {
			cfg.Groups["students"] = config.Group{Models: []string{"other-model"}}
		},
		tokens: func(pool *pgxpool.Pool) func(*auth.Authenticator) {
			return func(a *auth.Authenticator) {
				v, err := apitoken.New(t.Context(), f.oidc, pool)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(v.Close)
				a.Tokens = v.Authenticate
			}
		},
	})
	return f
}

func (f *tokenFixture) token(t *testing.T, alter func(map[string]any)) string {
	claims := map[string]any{"iss": f.provider.issuer, "sub": "service-1", "aud": []string{"olr-api", "account"}, "azp": "olr-service",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(), "preferred_username": "service-account-olr-service", "groups": []string{"staff"}}
	if alter != nil {
		alter(claims)
	}
	return f.provider.sign(t, claims)
}

func (f *tokenFixture) models(t *testing.T, bearer string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAccessTokensOnTheAPI(t *testing.T) {
	f := startTokenRouter(t)
	good := f.token(t, nil)
	if status, body := f.models(t, good); status != 200 || !strings.Contains(body, "chat-model") {
		t.Fatal("service token", status, body)
	}
	resp := f.call(t, good, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatal("inference with a token", resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	var user string
	if err := f.pool.QueryRow(t.Context(), `SELECT u.id FROM usage a JOIN users u ON u.id=a.user_id WHERE u.issuer=$1 AND u.sub='service-1' AND a.key_id IS NULL`, f.provider.issuer).Scan(&user); err != nil {
		t.Fatal("token usage not recorded for the token's user", err)
	}
	for name, alter := range map[string]func(map[string]any){
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://other.example/realms/test" },
		"wrong audience": func(c map[string]any) { c["aud"] = "olr-portal" },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no subject":     func(c map[string]any) { delete(c, "sub") },
	} {
		if status, _ := f.models(t, f.token(t, alter)); status != 401 {
			t.Error(name, status)
		}
	}
	// A token signed by another key is refused.
	other := newIdentityProvider(t)
	forged := other.sign(t, map[string]any{"iss": f.provider.issuer, "sub": "service-1", "aud": "olr-api", "exp": time.Now().Add(time.Minute).Unix()})
	if status, _ := f.models(t, forged); status != 401 {
		t.Error("foreign signature", status)
	}
	if status, _ := f.models(t, "not-a-jwt"); status != 401 {
		t.Error("garbage token", status)
	}
	// Disabled in the router: refused once the token cache entry expires.
	if err := (auth.Admin{Pool: f.pool}).DisableUser(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	fresh := f.token(t, func(c map[string]any) { c["jti"] = "new" })
	if status, _ := f.models(t, fresh); status != 401 {
		t.Error("disabled user's new token", status)
	}
}

func TestGroupSyncDisablesAndRegroups(t *testing.T) {
	f := startTokenRouter(t)
	admin := auth.Admin{Pool: f.pool}
	type person struct{ sub, id, key string }
	var people []*person
	for _, sub := range []string{"stays", "disabled", "deleted", "regrouped"} {
		id, err := admin.SyncIdentity(t.Context(), f.provider.issuer, sub, sub, "", []string{"staff"})
		if err != nil {
			t.Fatal(err)
		}
		key, err := admin.CreateKey(t.Context(), id, sub, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		people = append(people, &person{sub, id, key})
		f.provider.setUser(sub, remoteUser{enabled: true, groups: []string{"staff"}})
	}
	// Warm every API-key cache.
	for _, p := range people {
		if status, _ := f.models(t, p.key); status != 200 {
			t.Fatal("warm-up", p.sub, status)
		}
	}
	f.provider.setUser("disabled", remoteUser{enabled: false, groups: []string{"staff"}})
	f.provider.removeUser("deleted")
	f.provider.setUser("regrouped", remoteUser{enabled: true, groups: []string{"students"}})
	syncer, err := groupsync.New(t.Context(), f.oidc, "sync-secret", f.pool, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer syncer.Close()
	result, err := syncer.Sync(t.Context())
	if err != nil || result.Checked != 4 || result.Disabled != 2 || result.Updated != 1 {
		t.Fatal(result, err)
	}
	if status, _ := f.models(t, people[0].key); status != 200 {
		t.Fatal("unchanged user lost access", status)
	}
	for _, p := range people[1:3] {
		if status, _ := f.models(t, p.key); status != 401 {
			t.Fatal(p.sub, "kept API access", status)
		}
	}
	// The group change applies to the existing key at once.
	if status, body := f.models(t, people[3].key); status != 200 || !strings.Contains(body, `"data":[]`) {
		t.Fatal("regrouped user's models", status, body)
	}
	var groups []string
	if err := f.pool.QueryRow(t.Context(), `SELECT groups FROM users WHERE id=$1`, people[3].id).Scan(&groups); err != nil || !slices.Equal(groups, []string{"students"}) {
		t.Fatal(groups, err)
	}
	// Users of other issuers are not touched, and a second run changes nothing.
	if result, err := syncer.Sync(t.Context()); err != nil || result.Checked != 2 || result.Disabled != 0 || result.Updated != 0 {
		t.Fatal("second run", result, err)
	}
	// Without a valid service account nothing is changed.
	wrong, err := groupsync.New(t.Context(), f.oidc, "wrong", f.pool, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	f.provider.removeUser("stays")
	if result, err := wrong.Sync(t.Context()); err == nil || result.Disabled != 0 {
		t.Fatal("sync with a wrong secret", result, err)
	}
}
