package integration_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
)

const initialAdminPassword = "initial private admin passphrase"
const rotatedAdminPassword = "replacement private admin passphrase"

func TestProtectedAdminConcurrentBootstrapAndPasswordLifecycle(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	type result struct {
		account  auth.LocalAccount
		created  bool
		err      error
		password string
	}
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			password := fmt.Sprintf("concurrent private bootstrap password %d", i)
			account, created, err := admin.BootstrapLocalAdmin(t.Context(), []byte(password))
			results <- result{account, created, err, password}
		})
	}
	wg.Wait()
	close(results)
	var account auth.LocalAccount
	var password string
	creations := 0
	for result := range results {
		if result.err != nil || !result.account.ProtectedAdmin || !result.account.MustChangePassword {
			t.Fatal("bootstrap", result.err)
		}
		if account.ID != "" && account.ID != result.account.ID {
			t.Fatal("concurrent bootstrap created different administrators")
		}
		account = result.account
		if result.created {
			creations++
			password = result.password
		}
	}
	if creations != 1 {
		t.Fatal("bootstrap must create exactly one administrator")
	}
	var originalHash string
	if err := pool.QueryRow(t.Context(), `SELECT password_hash FROM local_credentials WHERE user_id=$1`, account.ID).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	verified, err := admin.VerifyLocalPassword(t.Context(), "admin", []byte(password))
	if err != nil || verified != account {
		t.Fatal("verify initial password", err)
	}
	for _, name := range []string{"admin", "unknown"} {
		if _, err := admin.VerifyLocalPassword(t.Context(), name, []byte("wrong private password")); !errors.Is(err, auth.ErrInvalidPassword) {
			t.Fatal("invalid password accepted", err)
		}
	}
	if _, err := admin.CreateKey(t.Context(), account.ID, "premature", time.Hour); err == nil {
		t.Fatal("issued an API key before required password change")
	}
	// Even an inserted key/session cannot bypass the first-change gate.
	key, _ := auth.Generate()
	hash := sha256.Sum256([]byte(key))
	execSQL(t, pool, `INSERT INTO api_keys(user_id,prefix,hash) VALUES($1,$2,$3)`, account.ID, key[:12], hash[:])
	execSQL(t, pool, `INSERT INTO sessions(id_hash,user_id,expires) VALUES('temporary',$1,now()+interval '1 hour')`, account.ID)
	authenticator, err := auth.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(authenticator.Close)
	if _, err := authenticator.Authenticate(t.Context(), key); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatal("API key bypassed initial password change", err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(password), []byte(password)); err == nil {
		t.Fatal("bootstrap credential was retained as the new password")
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte("wrong password"), []byte(rotatedAdminPassword)); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatal("password changed without the current credential", err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(password), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.VerifyLocalPassword(t.Context(), "admin", []byte(password)); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatal("bootstrap credential still works", err)
	}
	verified, err = admin.VerifyLocalPassword(t.Context(), "admin", []byte(rotatedAdminPassword))
	if err != nil || verified.MustChangePassword || verified.ID != account.ID {
		t.Fatal("first password change did not finish", err)
	}
	var sessions int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE user_id=$1`, account.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("password change retained old sessions", err)
	}
	if err := admin.SetGroups(t.Context(), account.ID, nil); err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(t.Context(), key)
	if err != nil || !principal.ProtectedAdmin || len(principal.Groups) != 0 {
		t.Fatal("protected authority depends on group membership", err)
	}
	// Reopening reapplies the migration checks and bootstrap never resets state.
	restarted := auth.Admin{Pool: openDatabase(t, database.dsn)}
	for _, retired := range [][]byte{[]byte(password), nil} {
		got, created, err := restarted.BootstrapLocalAdmin(t.Context(), retired)
		if err != nil || created || got != verified {
			t.Fatal("restart/reset bootstrap changed the administrator", err)
		}
	}
	var persisted string
	if err := pool.QueryRow(t.Context(), `SELECT row_to_json(c)::text FROM local_credentials c WHERE user_id=$1`, account.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	users, err := restarted.ListUsers(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(users)
	for _, secret := range []string{password, rotatedAdminPassword, originalHash} {
		if strings.Contains(string(metadata), secret) || strings.Contains(persisted, secret) {
			t.Fatal("credential leaked into metadata or retained bootstrap data")
		}
	}
}

func TestProtectedAdminDatabaseAndStoreGuards(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	admin := auth.Admin{Pool: pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	other, err := admin.AddUser(t.Context(), "ordinary", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func() error{
		func() error { return admin.DeleteUser(t.Context(), account.ID) },
		func() error { return admin.DisableUser(t.Context(), account.ID) },
	} {
		if err := mutation(); !errors.Is(err, auth.ErrProtectedAdmin) {
			t.Fatal("store operation bypassed protected admin guard", err)
		}
	}
	for _, sql := range []string{
		`DELETE FROM users WHERE id=$1`,
		`UPDATE users SET enabled=false WHERE id=$1`,
		`UPDATE users SET protected_admin=false WHERE id=$1`,
		`UPDATE users SET issuer='https://idp.example/realms/test' WHERE id=$1`,
		`UPDATE users SET sub='someone-else' WHERE id=$1`,
		`UPDATE users SET username='someone-else' WHERE id=$1`,
		`UPDATE users SET id=gen_random_uuid() WHERE id=$1`,
		`DELETE FROM local_credentials WHERE user_id=$1`,
		`UPDATE local_credentials SET user_id=gen_random_uuid() WHERE user_id=$1`,
		`UPDATE local_credentials SET password_hash='' WHERE user_id=$1`,
		`UPDATE local_credentials SET version=2 WHERE user_id=$1`,
	} {
		wantConstraint(t, pool, "23514", sql, account.ID)
	}
	for _, sql := range []string{
		`DELETE FROM users`, `DELETE FROM local_credentials`,
		`TRUNCATE users CASCADE`, `TRUNCATE local_credentials CASCADE`,
	} {
		wantConstraint(t, pool, "23514", sql)
	}
	// A normal local credential can still cascade on normal account erasure.
	execSQL(t, pool, `INSERT INTO local_credentials(user_id,password_hash)
		SELECT $1,password_hash FROM local_credentials WHERE user_id=$2`, other, account.ID)
	if _, err := admin.VerifyLocalPassword(t.Context(), "ordinary", []byte(initialAdminPassword)); err != nil {
		t.Fatal("normal local password verification failed", err)
	}
	wantConstraint(t, pool, "23514", `UPDATE users SET issuer='https://idp.example/realms/test' WHERE id=$1`, other)
	if err := admin.DisableUser(t.Context(), other); err != nil {
		t.Fatal("normal disable failed", err)
	}
	if _, err := admin.VerifyLocalPassword(t.Context(), "ordinary", []byte(initialAdminPassword)); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatal("disabled local credential authenticated", err)
	}
	if err := admin.DeleteUser(t.Context(), other); err != nil {
		t.Fatal("normal deletion failed", err)
	}
	if got, err := admin.ProtectedLocalAdmin(t.Context()); err != nil || got.ID != account.ID {
		t.Fatal("ordinary operations affected the protected account", err)
	}
}

func TestProtectedAdminRequiresCredentialAndRejectsAdoption(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	wantConstraint(t, pool, "23503", `INSERT INTO users(issuer,sub,username,protected_admin) VALUES('local','admin','admin',true)`)
	admin := auth.Admin{Pool: pool}
	if _, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte("short")); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatal("weak bootstrap password accepted", err)
	}
	var users int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatal("failed bootstrap left a partial account", err)
	}
	local, err := admin.AddUser(t.Context(), "admin", "same@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	sso, err := admin.SyncIdentity(t.Context(), "https://idp.example/realms/test", "admin", "admin", "same@example.org", nil)
	if err != nil || local == sso {
		t.Fatal("local and SSO identities were merged", err)
	}
	if _, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword)); !errors.Is(err, auth.ErrBootstrapConflict) {
		t.Fatal("existing local identity was adopted", err)
	}
	if _, err := admin.ProtectedLocalAdmin(t.Context()); !errors.Is(err, auth.ErrNotFound) {
		t.Fatal("failed adoption created a protected account", err)
	}
}

func TestProtectedAdminPortalGuardsAndIndependentAuthority(t *testing.T) {
	f := startPortal(t)
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	sso := f.browser(t)
	sso.login(t, "sso-admin", []string{"admins"})
	resp, body := sso.call(t, "PUT", "/portal/api/admin/users/"+account.ID+"/enabled", `{"enabled":false}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "protected local administrator") {
		t.Fatal("API disable bypassed protection", resp.StatusCode, string(body))
	}
	local := f.browser(t)
	installSession := func() {
		t.Helper()
		cookie := base64.RawURLEncoding.EncodeToString([]byte("local-session-for-guard-test-001"))
		hash := sha256.Sum256([]byte(cookie))
		execSQL(t, f.pool, `INSERT INTO sessions(id_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')`, hash[:], account.ID)
		u, _ := url.Parse(f.cfg.Portal.BaseURL + "/portal/")
		local.client.Jar.SetCookies(u, []*http.Cookie{{Name: "olr_session", Value: cookie, Path: "/portal"}})
	}
	installSession()
	if resp, _ := local.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("session bypassed required password change")
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	installSession()
	if err := admin.SetGroups(t.Context(), account.ID, []string{"unconfigured"}); err != nil {
		t.Fatal(err)
	}
	resp, body = local.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"admin":true`) || !strings.Contains(string(body), `"protected_admin":true`) {
		t.Fatal("protected authority depends on a configured admin group", resp.StatusCode, string(body))
	}
	resp, body = local.call(t, "GET", "/portal/api/admin/users", "", "")
	if resp.StatusCode != 200 || strings.Contains(string(body), "password_hash") || strings.Contains(string(body), "$argon2") || strings.Contains(string(body), rotatedAdminPassword) {
		t.Fatal("admin metadata failed or exposed a credential", resp.StatusCode)
	}
	resp, body = local.call(t, "GET", "/portal/api/admin/users/"+account.ID, "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"protected_admin":true`) {
		t.Fatal("protected account detail", resp.StatusCode)
	}
}
