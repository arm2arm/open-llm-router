package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/jackc/pgx/v5/pgxpool"
)

type portalGroupSync struct{ syncer *groupsync.Syncer }

func (s portalGroupSync) GroupSyncEnabled() bool { return s.syncer != nil }
func (s portalGroupSync) SyncGroups(ctx context.Context) (groupsync.Result, error) {
	return s.syncer.RunOnce(ctx)
}

func TestPortalGroupSyncUpdatesMembershipsAndChecksAccess(t *testing.T) {
	var syncer *groupsync.Syncer
	f := startPortal(t, usageOptions{
		portalConfig: func(cfg *config.Config) { cfg.OIDC.SyncClientID = "olr-sync" },
		portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, _ *proxy.Handler) {
			var err error
			syncer, err = groupsync.New(t.Context(), cfg.OIDC, "sync-secret", pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			syncer.Stats = &groupsync.Stats{}
			t.Cleanup(syncer.Close)
			h.GroupSync = portalGroupSync{syncer}
		},
	})
	const path = "/portal/api/admin/groups/sync"
	admin, member := f.browser(t), f.browser(t)
	admin.login(t, "sync-admin", []string{"admins"})
	member.login(t, "sync-member", []string{"staff"})
	f.provider.setUser("sync-admin", remoteUser{enabled: true, groups: []string{"admins"}})
	f.provider.setUser("sync-member", remoteUser{enabled: true, groups: []string{"research"}})
	store := auth.Admin{Pool: f.pool}
	if _, err := store.SyncIdentity(t.Context(), f.provider.issuer, "gone-user", "gone-user", "", []string{"staff"}, false); err != nil {
		t.Fatal(err)
	}
	local, err := store.AddUser(t.Context(), "local-person", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		browser      *portalBrowser
		body, origin string
		status       int
	}{
		{f.browser(t), `{}`, f.cfg.Portal.BaseURL, 401},
		{member, `{}`, f.cfg.Portal.BaseURL, 403},
		{admin, `{}`, "https://other.example", 403},
		{admin, `{"issuer":"https://other.example"}`, f.cfg.Portal.BaseURL, 400},
	} {
		response, data := tc.browser.call(t, "POST", path, tc.body, tc.origin)
		if response.StatusCode != tc.status {
			t.Fatalf("sync access: got %d want %d: %s", response.StatusCode, tc.status, data)
		}
	}
	response, data := admin.call(t, "GET", "/portal/api/admin/groups", "", "")
	if response.StatusCode != 200 || !strings.Contains(string(data), `"sync_enabled":true`) {
		t.Fatalf("sync availability: %d %s", response.StatusCode, data)
	}
	response, data = admin.call(t, "POST", path, `{}`, f.cfg.Portal.BaseURL)
	var run struct {
		Result  groupsync.Result `json:"result"`
		Problem string           `json:"problem"`
	}
	if response.StatusCode != 200 || json.Unmarshal(data, &run) != nil || run.Problem != "" || run.Result.Checked != 3 || run.Result.Updated != 1 || run.Result.Disabled != 1 || run.Result.Skipped {
		t.Fatalf("manual sync: %d %s", response.StatusCode, data)
	}
	response, data = member.call(t, "GET", "/portal/api/me", "", "")
	if response.StatusCode != 200 || !strings.Contains(string(data), `"groups":["research"]`) {
		t.Fatalf("existing session did not observe synced groups: %d %s", response.StatusCode, data)
	}
	response, data = member.call(t, "GET", "/portal/api/models", "", "")
	var access struct {
		Models []json.RawMessage `json:"models"`
	}
	if response.StatusCode != 200 || json.Unmarshal(data, &access) != nil || len(access.Models) != 0 {
		t.Fatalf("synced membership granted model access without a policy: %d %s", response.StatusCode, data)
	}
	var localGroups []string
	if err := f.pool.QueryRow(t.Context(), `SELECT groups FROM users WHERE id=$1`, local).Scan(&localGroups); err != nil || len(localGroups) != 1 || localGroups[0] != "staff" {
		t.Fatal("sync changed a local account", localGroups, err)
	}
	if syncer.Stats.Success.Load() != 1 || syncer.Stats.LastSuccess.Load() == 0 {
		t.Fatal("manual sync did not update job metrics")
	}
	f.provider.mu.Lock()
	f.provider.adminStatus = 401
	f.provider.mu.Unlock()
	response, data = admin.call(t, "POST", path, `{}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 200 || json.Unmarshal(data, &run) != nil || run.Problem == "" || run.Result.Updated != 0 || run.Result.Disabled != 0 {
		t.Fatalf("provider failure was hidden: %d %s", response.StatusCode, data)
	}
	f.provider.mu.Lock()
	f.provider.adminStatus = 0
	f.provider.adminVisits = nil
	f.provider.mu.Unlock()
	f.provider.setUser("sync-admin", remoteUser{enabled: true, groups: []string{"admins"}, delay: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = syncer.RunOnce(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		for _, sub := range f.provider.adminVisits {
			if sub == "sync-admin" {
				return true
			}
		}
		return false
	})
	response, data = admin.call(t, "POST", path, `{}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 200 || json.Unmarshal(data, &run) != nil || !run.Result.Skipped {
		t.Fatalf("overlapping sync was not skipped: %d %s", response.StatusCode, data)
	}
}

func TestPortalGroupSyncDisabled(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	b.login(t, "sync-admin", []string{"admins"})
	response, data := b.call(t, "POST", "/portal/api/admin/groups/sync", `{}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 409 || !strings.Contains(string(data), "sync_disabled") {
		t.Fatalf("disabled sync: %d %s", response.StatusCode, data)
	}
}
