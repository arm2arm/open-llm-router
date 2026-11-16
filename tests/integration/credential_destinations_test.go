package integration_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthenticationChecksRefuseInheritedCredentialsAtNewConnection(t *testing.T) {
	for _, storage := range []string{"environment", "stored", "legacy_stored"} {
		t.Run(storage, func(t *testing.T) {
			var service *settings.AuthenticationService
			env := map[string]string{config.EnvOIDCClientSecret: "test-secret", config.EnvOIDCSyncSecret: "sync-secret"}
			f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, _ *proxy.Handler) {
				cfg.AdminAPI.Enabled = true
				cfg.Forwarders.UnknownUser = config.UnknownUserReject
				cfg.Retention = config.Retention{UsageDays: 90, UsageDailyDays: 400, RunAt: "03:00"}
				cfg.OIDC.SyncClientID, cfg.OIDC.SyncIntervalS = "olr-sync", 900
				cfg.OIDCSyncSecret = env[config.EnvOIDCSyncSecret]
				cfg.AuthSources = map[string]string{"portal_client_secret": config.SourceEnvironment, "sync_client_secret": config.SourceEnvironment}
				key, err := settings.LoadCipher(func(string) string { return settingsKey(19) })
				if err != nil {
					t.Fatal(err)
				}
				service = &settings.AuthenticationService{DB: pool, File: cfg, Cipher: key, Lookup: func(n string) (string, bool) { v, ok := env[n]; return v, ok }}
				h.Settings = service
			}})
			admin := signInLocalAdmin(t, f)
			path := "/portal/api/admin/settings/authentication"
			call := func(method, suffix string, body any, want int) []byte {
				t.Helper()
				b, _ := json.Marshal(body)
				resp, data := admin.call(t, method, path+suffix, string(b), f.cfg.Portal.BaseURL)
				if resp.StatusCode != want {
					t.Fatalf("%s %s: %d want %d: %s", method, suffix, resp.StatusCode, want, data)
				}
				return data
			}
			var view authenticationResponse
			if json.Unmarshal(call("POST", "/import", struct{}{}, 200), &view) != nil {
				t.Fatal("import")
			}
			actor := audit.Actor{Type: "operator_cli", Name: "credential-test"}
			baseline := *view.Settings.Active.Document
			if storage != "environment" {
				encoded, _ := json.Marshal(baseline)
				saved, err := service.SaveDraft(t.Context(), settings.DomainAuthentication, view.Settings.Version, encoded, map[string][]byte{"portal_client_secret": []byte("test-secret"), "sync_client_secret": []byte("sync-secret")}, actor)
				if err != nil {
					t.Fatal(err)
				}
				if storage == "legacy_stored" {
					// Reproduce pre-binding ciphertext and immutable history, using the old
					// format rather than rewriting an existing secret or revision.
					baseline = *saved.Draft.Document
					for name, ref := range map[string]*settings.SecretRef{"portal_client_secret": &baseline.PortalClientSecret, "sync_client_secret": &baseline.SyncClientSecret} {
						var id string
						if err := f.pool.QueryRow(t.Context(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
							t.Fatal(err)
						}
						block, _ := aes.NewCipher(bytes.Repeat([]byte{19}, 32))
						aead, _ := cipher.NewGCM(block)
						nonce := make([]byte, aead.NonceSize())
						value := "test-secret"
						if name == "sync_client_secret" {
							value = "sync-secret"
						}
						logical := "authentication." + name
						sealed := aead.Seal(nonce, nonce, []byte(value), []byte("olr managed secret v1\x00"+id+"\x00"+logical))
						execSQL(t, f.pool, `INSERT INTO managed_secrets(id,name,ciphertext,key_id) VALUES($1,$2,$3,$4)`, id, logical, sealed, service.Cipher.ID())
						*ref = settings.SecretRef{Stored: id}
					}
					encoded, _ = json.Marshal(baseline)
					var revision int64
					if err := f.pool.QueryRow(t.Context(), `INSERT INTO settings_revisions(domain,origin,document,created_by_label) VALUES('authentication','draft',$1,'legacy fixture') RETURNING id`, encoded).Scan(&revision); err != nil {
						t.Fatal(err)
					}
					execSQL(t, f.pool, `UPDATE settings_state SET draft_revision=$1,version=version+1 WHERE domain='authentication'`, revision)
					saved, err = service.View(t.Context(), settings.DomainAuthentication)
					if err != nil {
						t.Fatal(err)
					}
				}
				active, err := service.Activate(t.Context(), settings.DomainAuthentication, saved.Version, saved.Draft.ID, actor, false)
				if err != nil {
					t.Fatal("activate original connection", err)
				}
				view.Settings = active
				baseline = *active.Active.Document
			}
			// A second, otherwise reachable provider records all attempts, not just
			// the Authorization header, so a form-encoded leak is caught too.
			var attempts atomic.Int64
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(500) }))
			defer sink.Close()
			issued, data := admin.call(t, "POST", "/portal/api/admin/agent-keys", `{"label":"credential test","permissions":["authentication:read","authentication:write"],"allowed_networks":["127.0.0.1"],"expires_in_days":1}`, f.cfg.Portal.BaseURL)
			var key struct{ Key string }
			if issued.StatusCode != 201 || json.Unmarshal(data, &key) != nil {
				t.Fatal("agent key")
			}
			for _, change := range []string{"issuer", "backchannel", "portal_client", "sync_client"} {
				doc := baseline
				switch change {
				case "issuer":
					doc.Issuer = sink.URL + "/realms/test"
				case "backchannel":
					doc.BackchannelURL = sink.URL + "/realms/test"
				case "portal_client":
					doc.PortalClientID = "other-portal"
				case "sync_client":
					doc.SyncClientID = "other-sync"
				}
				raw, _ := json.Marshal(doc)
				saved, err := service.SaveDraft(t.Context(), settings.DomainAuthentication, view.Settings.Version, raw, nil, actor)
				if err != nil {
					t.Fatal(err)
				}
				view.Settings = saved
				if saved.Draft.Validation.Valid {
					t.Fatal("inherited credential accepted at changed", change)
				}
				for repeat := 0; repeat < 2; repeat++ { // a rejected draft cannot establish a binding
					call("POST", "/checks", map[string]any{"revision": saved.Draft.ID}, 400)
					call("POST", "/test-sign-in", map[string]any{"revision": saved.Draft.ID}, 400)
					agentCall := func(path string, body any, want int) []byte {
						t.Helper()
						data, _ := json.Marshal(body)
						req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/api/admin/v1/"+path, bytes.NewReader(data))
						req.Header.Set("Authorization", "Bearer "+key.Key)
						req.Header.Set("Content-Type", "application/json")
						resp, err := http.DefaultClient.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						out, err := io.ReadAll(resp.Body)
						if err != nil {
							t.Fatal(err)
						}
						if resp.StatusCode != want {
							t.Fatalf("agent %s: %d want %d", path, resp.StatusCode, want)
						}
						return out
					}
					args := map[string]any{"body": map[string]any{"revision": saved.Draft.ID}}
					var preview struct{ ID string }
					if json.Unmarshal(agentCall("changes/preview", map[string]any{"operation": "check_authentication", "arguments": args}, 201), &preview) != nil || preview.ID == "" {
						t.Fatal("check preview")
					}
					agentCall("changes/"+preview.ID+"/apply", map[string]any{"arguments": args, "idempotency_key": "check-" + jsonNumber(saved.Draft.ID) + "-" + jsonNumber(int64(repeat))}, 400)
				}
				if _, _, err := service.Candidate(t.Context(), settings.DomainAuthentication, saved.Draft.ID); err == nil {
					t.Fatal("candidate accepted inherited credential")
				}
				if _, err := service.Activate(t.Context(), settings.DomainAuthentication, saved.Version, saved.Draft.ID, actor, false); err == nil {
					t.Fatal("activated inherited credential")
				}
			}
			if attempts.Load() != 0 {
				t.Fatal("changed connection was contacted with inherited credentials")
			}
			// Explicitly supplying both secrets authorizes a new recipient; the check
			// now reaches that provider. Clearing secrets is also permitted.
			doc := baseline
			doc.Issuer = sink.URL + "/realms/test"
			raw, _ := json.Marshal(doc)
			saved, err := service.SaveDraft(t.Context(), settings.DomainAuthentication, view.Settings.Version, raw, map[string][]byte{"portal_client_secret": []byte("new-portal-secret"), "sync_client_secret": []byte("new-sync-secret")}, actor)
			if err != nil || !saved.Draft.Validation.Valid {
				t.Fatal("explicit new credentials refused", err)
			}
			call("POST", "/checks", map[string]any{"revision": saved.Draft.ID}, 200)
			if attempts.Load() == 0 {
				t.Fatal("control provider was unreachable")
			}
			doc.PortalClientSecret, doc.SyncClientSecret = settings.SecretRef{}, settings.SecretRef{}
			raw, _ = json.Marshal(doc)
			saved, err = service.SaveDraft(t.Context(), settings.DomainAuthentication, saved.Version, raw, nil, actor)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = service.Candidate(t.Context(), settings.DomainAuthentication, saved.Draft.ID); err != nil {
				t.Fatal("cleared credentials refused", err)
			}
		})
	}
}

func TestManagedEnvironmentSecretRotationKeepsSSOAvailable(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	env := managedEnvironment(settingsKey(20))
	env[config.EnvOIDCSyncSecret] = "old-sync"
	file := managedConfig(t, env)
	service := managedService(t, env, file)
	service.DB = pool
	actor := audit.Actor{Type: "operator_cli", Name: "rotation-test"}
	view, err := service.Import(t.Context(), settings.DomainAuthentication, actor)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(view.Active.Document)
	view, err = service.SaveDraft(t.Context(), settings.DomainAuthentication, view.Version, raw, map[string][]byte{"portal_client_secret": []byte("rotated-portal"), "sync_client_secret": []byte("rotated-sync")}, actor)
	if err != nil || !view.Draft.Validation.Valid {
		t.Fatal("rotation blocked by old environment", err, view.Draft)
	}
	view, err = service.Activate(t.Context(), settings.DomainAuthentication, view.Version, view.Draft.ID, actor, false)
	if err != nil {
		t.Fatal(err)
	}
	// Only after activation may deployment secrets be removed. A fresh process
	// with no old environment values still opens the active stored references.
	delete(env, config.EnvOIDCClientSecret)
	delete(env, config.EnvOIDCSyncSecret)
	restarted := managedService(t, env, managedConfig(t, env))
	restarted.DB = pool
	cfg, _, err := restarted.Candidate(t.Context(), settings.DomainAuthentication, view.Active.ID)
	if err != nil || cfg.OIDC == nil || cfg.OIDCClientSecret != "rotated-portal" || cfg.OIDCSyncSecret != "rotated-sync" {
		t.Fatal("rotation/restart lost SSO", err)
	}
}

func TestPreviousIssuerSummaryIncludesAdministrationKeys(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, _ *proxy.Handler) {
		h.Settings = &settings.AuthenticationService{DB: pool, File: cfg}
	}})
	admin := signInLocalAdmin(t, f)
	old := user(t, f.pool, "https://old.example/realms/test", "old-admin")
	execSQL(t, f.pool, `UPDATE users SET groups=ARRAY['admins'] WHERE id=$1`, old)
	for i, attributes := range []string{"false,clock_timestamp()+interval '1 day'", "true,clock_timestamp()+interval '1 day'", "false,clock_timestamp()-interval '1 day'"} {
		execSQL(t, f.pool, `INSERT INTO admin_api_keys(owner_id,label,prefix,hash,permissions,allowed_networks,revoked,expires) VALUES($1,'old admin','prefix',$2,ARRAY['service:read'],ARRAY['127.0.0.1'],`+attributes+`)`, old, credentialTestHash(i))
	}
	_, data := admin.call(t, "GET", "/portal/api/admin/settings/authentication", "", "")
	var view struct {
		OtherIssuers []struct {
			Issuer     string
			ActiveKeys int `json:"active_keys"`
			AdminKeys  int `json:"active_admin_keys"`
		} `json:"other_issuers"`
	}
	if json.Unmarshal(data, &view) != nil {
		t.Fatal("issuer summary")
	}
	found := false
	for _, issuer := range view.OtherIssuers {
		if strings.Contains(issuer.Issuer, "old.example") {
			found = true
			if issuer.ActiveKeys != 0 || issuer.AdminKeys != 1 {
				t.Fatal("active administration credential hidden", issuer)
			}
		}
	}
	if !found {
		t.Fatal("previous issuer absent")
	}
}

func credentialTestHash(i int) []byte {
	value := sha256.Sum256([]byte(jsonNumber(int64(i))))
	return value[:]
}
