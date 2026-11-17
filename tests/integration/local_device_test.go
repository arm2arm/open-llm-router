package integration_test

import (
	"crypto/sha256"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/retention"
)

func TestKnownDeviceLocalLoginSurvivesAnonymousFlood(t *testing.T) {
	f := startLocalPortal(t, nil)
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	known, anonymous := f.browser(t), f.browser(t)
	known.localLogin(t, "admin", rotatedAdminPassword, 200)
	base, _ := url.Parse(f.cfg.Portal.BaseURL + "/portal/")
	var device *http.Cookie
	for _, cookie := range known.client.Jar.Cookies(base) {
		if cookie.Name == auth.LocalDeviceCookie {
			device = cookie
		}
	}
	if device == nil || len(device.Value) != 43 {
		t.Fatal("successful local sign-in did not remember the device")
	}
	if resp, _ := known.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 200 {
		t.Fatal("logout failed")
	}
	if resp, _ := known.call(t, "GET", "/portal/api/admin/users", "", ""); resp.StatusCode != 401 {
		t.Fatal("device cookie alone authenticated a session")
	}
	// Initial successful login used one anonymous attempt; expire that window
	// so the attacker's complete ten-guess budget is tested independently.
	execSQL(t, f.pool, `DELETE FROM portal_auth_throttle`)
	for range 10 {
		anonymous.localLogin(t, "admin", "incorrect private password", 401)
	}
	anonymous.localLogin(t, "admin", rotatedAdminPassword, 429)
	known.localLogin(t, "admin", rotatedAdminPassword, 200)
	// Even the anonymous global ceiling and a full anonymous bucket table
	// cannot consume this device's separate storage or guessing allowance.
	global := sha256.Sum256([]byte("global"))
	execSQL(t, f.pool, `UPDATE portal_auth_throttle SET attempts=60 WHERE bucket=$1`, global[:])
	execSQL(t, f.pool, `INSERT INTO portal_auth_throttle(bucket,attempts,expires)
		SELECT sha256(('flood-'||n)::bytea),1,clock_timestamp()+interval '1 minute'
		FROM generate_series(1,4096-(SELECT count(*) FROM portal_auth_throttle WHERE NOT known_device)) n`)
	known.localLogin(t, "admin", rotatedAdminPassword, 200)
	// Reauthentication retains a stable device budget, rather than minting a
	// fresh allowance on each successful login.
	for range 8 {
		known.localLogin(t, "admin", "incorrect private password", 401)
	}
	known.localLogin(t, "admin", rotatedAdminPassword, 429)
	// A new operator-issued device can still recover while the flood persists.
	raw, err := admin.CreateLocalDevice(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery := f.browser(t)
	recovery.client.Jar.SetCookies(base, []*http.Cookie{{Name: auth.LocalDeviceCookie, Value: raw, Path: "/portal"}})
	recovery.localLogin(t, "admin", rotatedAdminPassword, 200)
	if resp, _ := recovery.call(t, "GET", "/portal/api/admin/users", "", ""); resp.StatusCode != 200 {
		t.Fatal("operator-issued device did not restore administration")
	}
	// A valid cookie is bound to its account. An expired cookie is anonymous.
	recovery.localLogin(t, "different-account", "incorrect private password", 429)
	hash := sha256.Sum256([]byte(raw))
	execSQL(t, f.pool, `UPDATE local_auth_devices SET expires=clock_timestamp()-interval '1 second' WHERE hash=$1`, hash[:])
	recovery.localLogin(t, "admin", rotatedAdminPassword, 429)
}

func TestLocalDevicesAreBoundedAndExpire(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := range 12 {
		raw, err := admin.CreateLocalDevice(t.Context(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = raw
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM local_auth_devices WHERE user_id=$1`, account.ID).Scan(&count); err != nil || count != 8 {
		t.Fatal("device registration was unbounded", count, err)
	}
	hash := sha256.Sum256([]byte(first))
	if known, err := admin.KnownLocalDevice(t.Context(), "admin", hash); err != nil || known {
		t.Fatal("oldest device was not evicted", err)
	}
	execSQL(t, pool, `UPDATE local_auth_devices SET expires=clock_timestamp()-interval '1 second'`)
	result, err := retention.Prune(t.Context(), pool, config.Retention{UsageDays: 90, UsageDailyDays: 400}, time.UTC, time.Now())
	if err != nil || result.LocalDevices != 8 {
		t.Fatal("expired devices were not pruned", result, err)
	}
}
