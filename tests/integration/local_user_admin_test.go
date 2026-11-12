package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
)

const userTemporaryPassword = "temporary-local-user-password"
const userChangedPassword = "changed-local-user-password"
const userResetPassword = "reset-local-user-password"

func adminCall(t *testing.T, b *portalBrowser, method, path string, body any, want int) []byte {
	t.Helper()
	encoded := ""
	if body != nil {
		value, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(value)
	}
	response, data := b.call(t, method, path, encoded, b.f.cfg.Portal.BaseURL)
	for _, secret := range []string{userTemporaryPassword, userChangedPassword, userResetPassword, "$argon2id$", "password_hash"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("administration response leaked a credential")
		}
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: %d want %d: %s", method, path, response.StatusCode, want, data)
	}
	return data
}

func createPortalUser(t *testing.T, operator *portalBrowser, name string, groups []string) string {
	t.Helper()
	data := adminCall(t, operator, "POST", "/portal/api/admin/users", map[string]any{"username": name, "email": "shared@example.test", "groups": groups, "password": userTemporaryPassword}, 201)
	var result struct {
		ID         string
		MustChange bool `json:"must_change_password"`
	}
	if json.Unmarshal(data, &result) != nil || result.ID == "" || !result.MustChange {
		t.Fatal("invalid local creation response")
	}
	return result.ID
}

type managedUser struct {
	User struct {
		ID, Issuer  string
		Groups      []string
		Protected   bool `json:"protected_admin"`
		HasPassword bool `json:"has_local_password"`
		MustChange  bool `json:"must_change_password"`
	}
	Enabled, Admin bool
	Models         []string
	Limits         map[string]any
	Keys           []auth.KeyInfo
}

func managedDetail(t *testing.T, operator *portalBrowser, id string) managedUser {
	t.Helper()
	data := adminCall(t, operator, "GET", "/portal/api/admin/users/"+id, nil, 200)
	var result managedUser
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLocalUserAdministrationLifecycleAndCacheInvalidation(t *testing.T) {
	f := startPortal(t, usageOptions{group: &config.Group{Models: []string{"chat-model"}, RPM: limitPtr(7)}})
	operator := f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	person := f.browser(t)
	ssoID := person.login(t, "alex", []string{"staff"})
	id := createPortalUser(t, operator, "alex", []string{"staff"})
	if id == ssoID {
		t.Fatal("local and SSO identities linked by name/email")
	}
	local := f.browser(t)
	local.localLogin(t, "alex", userTemporaryPassword, 200)
	adminCall(t, local, "GET", "/portal/api/me", nil, 403)
	adminCall(t, local, "POST", "/portal/api/keys", map[string]string{"label": "blocked"}, 403)
	adminCall(t, local, "POST", "/portal/password", map[string]string{"current_password": userTemporaryPassword, "new_password": userChangedPassword}, 200)
	local.localLogin(t, "alex", userChangedPassword, 200)
	detail := managedDetail(t, operator, id)
	if detail.User.Issuer != "local" || !detail.User.HasPassword || detail.User.MustChange || detail.Limits["rpm"] != float64(7) || len(detail.Models) != 2 || detail.Admin {
		t.Fatal("incorrect effective local access")
	}
	created := adminCall(t, local, "POST", "/portal/api/keys", map[string]string{"label": "workstation"}, 201)
	var key struct{ Key string }
	if json.Unmarshal(created, &key) != nil || key.Key == "" {
		t.Fatal("no key")
	}
	other, err := auth.New(t.Context(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	principal, err := other.Authenticate(t.Context(), key.Key)
	if err != nil || len(principal.Groups) != 1 || principal.Groups[0] != "staff" {
		t.Fatal("new local key did not authenticate")
	}
	apiModels := func(want int) []byte {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("key status %d want %d", response.StatusCode, want)
		}
		return data
	}
	apiModels(200) // Also warm the router's independent cache.
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/overrides/rpm", map[string]any{"value": 3, "reason": "local pilot"}, 204)
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/groups", map[string]any{"groups": []string{}}, 204)
	adminCall(t, local, "GET", "/portal/api/me", nil, 401)
	principal, err = other.Authenticate(t.Context(), key.Key)
	if err != nil || len(principal.Groups) != 0 || principal.Overrides["rpm"] == nil || *principal.Overrides["rpm"] != 3 {
		t.Fatal("other router retained stale permissions")
	}
	if strings.Contains(string(apiModels(200)), "chat-model") {
		t.Fatal("removed model grant remained cached")
	}
	detail = managedDetail(t, operator, id)
	if len(detail.Models) != 0 || detail.Limits["rpm"] != float64(3) {
		t.Fatal("group update lost overrides or model filtering")
	}
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/groups", map[string]any{"groups": []string{"staff"}}, 204)
	local.localLogin(t, "alex", userChangedPassword, 200)
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/enabled", map[string]bool{"enabled": false}, 204)
	apiModels(401)
	if _, err := other.Authenticate(t.Context(), key.Key); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatal("disabled key remained cached")
	}
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/enabled", map[string]bool{"enabled": true}, 204)
	adminCall(t, local, "GET", "/portal/api/me", nil, 401) // Re-enable must not restore old cookies.
	apiModels(200)                                         // Temporarily disabling preserves the unrevoked key.
	local.localLogin(t, "alex", userChangedPassword, 200)
	adminCall(t, operator, "POST", "/portal/api/admin/users/"+id+"/password", map[string]string{"password": userChangedPassword}, 400)
	adminCall(t, local, "GET", "/portal/api/me", nil, 200) // A rejected reset leaves access intact.
	adminCall(t, operator, "POST", "/portal/api/admin/users/"+id+"/password", map[string]string{"password": userResetPassword}, 204)
	adminCall(t, local, "GET", "/portal/api/me", nil, 401)
	apiModels(401)
	if _, err := other.Authenticate(t.Context(), key.Key); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatal("reset did not revoke a cached key")
	}
	local.localLogin(t, "alex", userChangedPassword, 401)
	local.localLogin(t, "alex", userResetPassword, 200)
	adminCall(t, local, "GET", "/portal/api/me", nil, 403)
	detail = managedDetail(t, operator, id)
	if !detail.User.MustChange || len(detail.Keys) != 1 || !detail.Keys[0].Revoked {
		t.Fatal("reset status/key metadata incorrect")
	}
	adminCall(t, local, "POST", "/portal/password", map[string]string{"current_password": userResetPassword, "new_password": userChangedPassword}, 200)
	local.localLogin(t, "alex", userChangedPassword, 200)
	apiModels(401)                                          // A fresh password does not reactivate revoked keys.
	adminCall(t, person, "GET", "/portal/api/me", nil, 200) // Same-name SSO account is untouched.
	if strings.Contains(f.logs.String(), userTemporaryPassword) || strings.Contains(f.logs.String(), userResetPassword) {
		t.Fatal("admin operation logged a password")
	}
}

func TestLocalUserAdministrationPrivilegeSourceAndProtectedGuards(t *testing.T) {
	f := startPortal(t)
	operator, ordinary := f.browser(t), f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	sso := ordinary.login(t, "sso-user", []string{"staff"})
	store := auth.Admin{Pool: f.pool}
	root, _, err := store.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	id := createPortalUser(t, operator, "reader", []string{"staff"})
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/portal/api/admin/users", map[string]any{"username": "denied", "password": userTemporaryPassword}},
		{"PUT", "/portal/api/admin/users/" + id + "/groups", map[string]any{"groups": []string{"admins"}}},
		{"POST", "/portal/api/admin/users/" + id + "/password", map[string]string{"password": userResetPassword}},
		{"DELETE", "/portal/api/admin/users/" + id + "/keys/00000000-0000-0000-0000-000000000000", struct{}{}},
	} {
		adminCall(t, ordinary, test.method, test.path, test.body, 403)
		response, _ := operator.call(t, test.method, test.path, string(mustJSON(t, test.body)), "https://evil.example")
		if response.StatusCode != 403 {
			t.Fatal("cross-origin admin mutation accepted")
		}
	}
	for _, target := range []string{root.ID, sso} {
		adminCall(t, operator, "PUT", "/portal/api/admin/users/"+target+"/groups", map[string]any{"groups": []string{}}, 403)
		adminCall(t, operator, "POST", "/portal/api/admin/users/"+target+"/password", map[string]string{"password": userResetPassword}, 403)
	}
	adminCall(t, operator, "PUT", "/portal/api/admin/users/"+root.ID+"/enabled", map[string]bool{"enabled": false}, 403)
	adminCall(t, operator, "POST", "/portal/api/admin/users", map[string]any{"username": "admin", "password": userTemporaryPassword}, 403)
	adminCall(t, operator, "POST", "/portal/api/admin/users", map[string]any{"username": "reader", "password": userTemporaryPassword}, 409)
	for _, body := range []any{
		map[string]any{"username": "space user", "password": userTemporaryPassword},
		map[string]any{"username": "bad-email", "email": "not an email", "password": userTemporaryPassword},
		map[string]any{"username": "short", "password": "short"},
		map[string]any{"username": "unknown", "password": userTemporaryPassword, "groups": []string{"missing"}},
		map[string]any{"username": "elevated", "password": userTemporaryPassword, "protected_admin": true},
	} {
		adminCall(t, operator, "POST", "/portal/api/admin/users", body, 400)
	}
	for _, body := range []any{struct{}{}, map[string]any{"groups": nil}, map[string]any{"groups": []string{"staff", "staff"}}, map[string]any{"groups": []string{"missing"}}} {
		adminCall(t, operator, "PUT", "/portal/api/admin/users/"+id+"/groups", body, 400)
	}
	if err := store.SetLocalGroups(t.Context(), root.ID, []string{}); !errors.Is(err, auth.ErrProtectedAdmin) {
		t.Fatal("store allowed protected memberships mutation")
	}
	if err := store.ResetLocalPassword(t.Context(), root.ID, []byte(userResetPassword)); !errors.Is(err, auth.ErrProtectedAdmin) {
		t.Fatal("store allowed protected password reset")
	}
	var credentials int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM local_credentials WHERE user_id=$1`, sso).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatal("password attached to SSO identity")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOperatorKeyRevocationIsOwnerScopedAndMetadataOnly(t *testing.T) {
	f := startPortal(t)
	operator := f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	store := auth.Admin{Pool: f.pool}
	id, err := store.AddUser(t.Context(), "service", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.CreateKey(t.Context(), id, "service key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := store.ListKeys(t.Context(), id)
	if err != nil || len(keys) != 1 {
		t.Fatal("missing key", err)
	}
	other, err := auth.New(t.Context(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	data := adminCall(t, operator, "GET", "/portal/api/admin/users/"+id, nil, 200)
	if strings.Contains(string(data), key) || strings.Contains(string(data), "\"hash\"") {
		t.Fatal("admin key listing exposed key material")
	}
	adminCall(t, operator, "DELETE", "/portal/api/admin/users/"+f.owner+"/keys/"+keys[0].ID, struct{}{}, 404)
	if _, err := other.Authenticate(t.Context(), key); err != nil {
		t.Fatal("wrong-owner request revoked a key")
	}
	adminCall(t, operator, "DELETE", "/portal/api/admin/users/"+id+"/keys/"+keys[0].ID, struct{}{}, 204)
	if _, err := other.Authenticate(t.Context(), key); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatal("operator revocation left cached access")
	}
	if detail := managedDetail(t, operator, id); len(detail.Keys) != 1 || !detail.Keys[0].Revoked {
		t.Fatal("key history was removed")
	}
	adminCall(t, operator, "POST", "/portal/api/admin/users/"+id+"/password", map[string]string{"password": userTemporaryPassword}, 204)
	if detail := managedDetail(t, operator, id); !detail.User.HasPassword || !detail.User.MustChange {
		t.Fatal("API-key-only user did not receive required-change credential")
	}
}

func TestLocalKeyIssuanceWaitsForCredentialReset(t *testing.T) {
	f := startLocalPortal(t, nil)
	store := auth.Admin{Pool: f.pool}
	id, err := store.CreateLocalUser(t.Context(), "concurrent", "", nil, []byte(userTemporaryPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeLocalPassword(t.Context(), id, []byte(userTemporaryPassword), []byte(userChangedPassword)); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM users WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := store.CreateKey(t.Context(), id, "racing reset", time.Hour); done <- err }()
	waitFor(t, func() bool {
		var count int
		err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`).Scan(&count)
		return err == nil && count > 0
	})
	if _, err := tx.Exec(t.Context(), `UPDATE local_credentials SET must_change_password=true WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("key issued from a pre-reset snapshot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("key issuance did not unblock")
	}
}

func TestLocalSessionIssuanceWaitsForDisablement(t *testing.T) {
	f := startLocalPortal(t, nil)
	store := auth.Admin{Pool: f.pool}
	id, err := store.CreateLocalUser(t.Context(), "concurrent", "", nil, []byte(userTemporaryPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeLocalPassword(t.Context(), id, []byte(userTemporaryPassword), []byte(userChangedPassword)); err != nil {
		t.Fatal(err)
	}
	old := f.browser(t)
	old.localLogin(t, "concurrent", userChangedPassword, 200)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `UPDATE users SET enabled=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var requestErr error
	done := make(chan int, 1)
	b := f.browser(t)
	body := string(mustJSON(t, map[string]string{"username": "concurrent", "password": userChangedPassword}))
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/portal/login/local", strings.NewReader(body))
		req.Host = "portal.example.test"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", f.cfg.Portal.BaseURL)
		response, err := b.client.Do(req)
		requestErr = err
		if err != nil {
			done <- 0
			return
		}
		response.Body.Close()
		done <- response.StatusCode
	}()
	waitFor(t, func() bool {
		var count int
		err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE%'`).Scan(&count)
		return err == nil && count > 0
	})
	if _, err := tx.Exec(t.Context(), `DELETE FROM sessions WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-done:
		if status != 401 || requestErr != nil {
			t.Fatalf("concurrent session accepted: status=%d err=%v", status, requestErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login did not unblock")
	}
	if err := store.SetEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	adminCall(t, old, "GET", "/portal/api/me", nil, 401)
}
