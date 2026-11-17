package integration_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func emailForwarder(t *testing.T, f *usageRouter) *forwarderSetup {
	t.Helper()
	a := auth.Admin{Pool: f.pool}
	owner, err := a.AddUser(t.Context(), "email-forwarder", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := a.CreateForwarderKey(t.Context(), owner, "email-forwarder", "interactive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &forwarderSetup{usageRouter: f, webui: owner, key: key}
}

func emailMapping(unknown string) *config.Forwarders {
	return &config.Forwarders{UnknownUser: unknown, HeaderMap: map[string]string{"X-OLR-End-User": "username", "X-Email": "email"}}
}

func portalEmailLogin(t *testing.T, f *portalFixture, sub, email string, verified any) string {
	t.Helper()
	b := f.browser(t)
	q, _ := b.challenge(t)
	code := f.provider.authorize(t, q, sub, []string{"staff"}, func(c map[string]any) {
		c["email"] = email
		if verified != nil {
			c["email_verified"] = verified
		}
	})
	resp, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 303 {
		t.Fatal("OIDC callback", resp.StatusCode)
	}
	_, data := b.call(t, "GET", "/portal/api/me", "", "")
	var me struct{ User struct{ ID string } }
	if json.Unmarshal(data, &me) != nil || me.User.ID == "" {
		t.Fatalf("me: %s", data)
	}
	return me.User.ID
}

func TestExistingSSOEmailRequiresFreshVerification(t *testing.T) {
	database := newDatabase(t)
	// Build an actual version-17 database, independently of later migrations.
	pool, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	execSQL(t, pool, `CREATE TABLE schema_migrations (version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	for _, migration := range db.Migrations()[:17] {
		execSQL(t, pool, migration.SQL)
		execSQL(t, pool, `INSERT INTO schema_migrations(version,name) VALUES ($1,$2)`, migration.Version, migration.Name)
	}
	id := user(t, pool, "https://idp.example/realm", "legacy")
	upgraded := openDatabase(t, database.dsn)
	var email string
	var verified bool
	if err := upgraded.QueryRow(t.Context(), `SELECT email,email_verified FROM users WHERE id=$1`, id).Scan(&email, &verified); err != nil || email != "shared@example.org" || verified {
		t.Fatal("upgrade lost the display address or trusted an unverified legacy claim", email, verified, err)
	}
}

func TestForwarderEmailRequiresVerifiedPortalClaim(t *testing.T) {
	for _, unknown := range []string{config.UnknownUserReject, config.UnknownForwarderPolicy} {
		t.Run(unknown, func(t *testing.T) {
			f := startPortal(t, usageOptions{forwarders: emailMapping(unknown)})
			forwarder := emailForwarder(t, f.usageRouter)
			var unverified []string
			for _, sub := range []string{"false-claim", "missing-claim"} {
				var claim any
				if sub == "false-claim" {
					claim = false
				}
				unverified = append(unverified, portalEmailLogin(t, f, sub, "owner@example.org", claim))
				if resp := forwarder.send(t, forwarder.key, map[string]string{"X-Email": "OWNER@example.org"}); resp.StatusCode != 403 {
					t.Fatal("unverified email selected", resp.StatusCode)
				}
				if resp := forwarder.send(t, forwarder.key, map[string]string{"X-OLR-End-User": sub}); resp.StatusCode != 200 {
					t.Fatal("username mapping refused", resp.StatusCode)
				}
			}
			owner := portalEmailLogin(t, f, "owner", "owner@example.org", true)
			if resp := forwarder.send(t, forwarder.key, map[string]string{"X-Email": "OWNER@example.org"}); resp.StatusCode != 200 {
				t.Fatal("verified owner hidden by unverified duplicates", resp.StatusCode)
			}
			// A fresh unverified assertion invalidates the warmed email cache.
			if id := portalEmailLogin(t, f, "owner", "owner@example.org", false); id != owner {
				t.Fatal("identity changed")
			}
			if resp := forwarder.send(t, forwarder.key, map[string]string{"X-Email": "owner@example.org"}); resp.StatusCode != 403 {
				t.Fatal("verification revocation ignored", resp.StatusCode)
			}
			f.stop()
			if forwarder.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, owner) != 1 ||
				forwarder.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1 OR forwarded_for=$2`, unverified[0], unverified[1]) != 2 || f.writer.Counters().Written != 3 {
				t.Fatal("incorrect usage attribution", f.writer.Counters())
			}
		})
	}
}

func TestForwarderEmailRequiresVerifiedAccessToken(t *testing.T) {
	for _, unknown := range []string{config.UnknownUserReject, config.UnknownForwarderPolicy} {
		t.Run(unknown, func(t *testing.T) {
			f := startTokenRouter(t, emailMapping(unknown))
			forwarder := emailForwarder(t, f.usageRouter)
			for _, sub := range []string{"false-token", "missing-token", "owner-token"} {
				token := f.token(t, func(c map[string]any) {
					c["sub"], c["preferred_username"], c["email"] = sub, sub, "owner@example.org"
					if sub != "missing-token" {
						c["email_verified"] = sub == "owner-token"
					}
				})
				if status, _ := f.models(t, token); status != 200 {
					t.Fatal("valid access token refused", status)
				}
				want := 403
				if sub == "owner-token" {
					want = 200
				}
				if resp := forwarder.send(t, forwarder.key, map[string]string{"X-Email": "owner@example.org"}); resp.StatusCode != want {
					t.Fatalf("%s: email response %d, want %d", sub, resp.StatusCode, want)
				}
			}
			// A newer assertion can remove verification even if other claims
			// are unchanged; replaying an older verified token cannot restore it.
			old := f.token(t, func(c map[string]any) {
				c["sub"], c["preferred_username"], c["email"], c["email_verified"], c["iat"] = "owner-token", "owner-token", "owner@example.org", true, time.Now().Add(-time.Second).Unix()
			})
			newer := f.token(t, func(c map[string]any) {
				c["sub"], c["preferred_username"], c["email"], c["email_verified"], c["iat"] = "owner-token", "owner-token", "owner@example.org", false, time.Now().Add(2*time.Second).Unix()
			})
			if status, _ := f.models(t, newer); status != 200 {
				t.Fatal("newer token refused", status)
			}
			if status, _ := f.models(t, old); status != 200 {
				t.Fatal("old token refused", status)
			}
			if resp := forwarder.send(t, forwarder.key, map[string]string{"X-Email": "owner@example.org"}); resp.StatusCode != 403 {
				t.Fatal("old token restored email verification", resp.StatusCode)
			}
			f.stop()
			if f.writer.Counters().Written != 1 {
				t.Fatal("unverified token received forwarded usage", f.writer.Counters())
			}
		})
	}
}
