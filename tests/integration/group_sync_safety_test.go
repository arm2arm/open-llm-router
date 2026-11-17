package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
)

func syncPeople(t *testing.T, f *tokenFixture, count int) []string {
	t.Helper()
	for i := range count {
		sub := fmt.Sprintf("sync-user-%d", i)
		if _, err := (auth.Admin{Pool: f.pool}).SyncIdentity(t.Context(), f.provider.issuer, sub, sub, "", []string{"staff"}, false); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.pool.Query(t.Context(), `SELECT sub FROM users WHERE issuer=$1 ORDER BY id`, f.provider.issuer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var subs []string
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return subs
}

func newSyncer(t *testing.T, f *tokenFixture) *groupsync.Syncer {
	t.Helper()
	s, err := groupsync.New(t.Context(), f.oidc, "sync-secret", f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func assertSyncPerson(t *testing.T, f *tokenFixture, sub string, enabled bool, groups []string) {
	t.Helper()
	var gotEnabled bool
	var gotGroups []string
	if err := f.pool.QueryRow(t.Context(), `SELECT enabled,groups FROM users WHERE issuer=$1 AND sub=$2`, f.provider.issuer, sub).Scan(&gotEnabled, &gotGroups); err != nil {
		t.Fatal(err)
	}
	if gotEnabled != enabled || !slices.Equal(gotGroups, groups) {
		t.Fatalf("%s: enabled=%t groups=%v, want %t %v", sub, gotEnabled, gotGroups, enabled, groups)
	}
}

func TestGroupSyncAdminOutageCannotDisableUsers(t *testing.T) {
	for _, status := range []int{404, 401, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := startTokenRouter(t)
			subs := syncPeople(t, f, 3)
			s := newSyncer(t, f)
			f.provider.mu.Lock()
			f.provider.adminStatus = status
			f.provider.mu.Unlock()
			result, err := s.Sync(t.Context())
			if err == nil || result.Disabled != 0 || result.Updated != 0 {
				t.Fatal("admin outage changed users", result, err)
			}
			for _, sub := range subs {
				assertSyncPerson(t, f, sub, true, []string{"staff"})
			}
		})
	}
}

func TestGroupSyncRunMetricsRecoverAfterProviderOutage(t *testing.T) {
	f := startTokenRouter(t)
	syncPeople(t, f, 1)
	s := newSyncer(t, f)
	s.Stats = &groupsync.Stats{}
	f.provider.mu.Lock()
	f.provider.adminStatus = 401
	f.provider.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, 100*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for s.Stats.Failure.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("failed run not counted")
		}
		time.Sleep(time.Millisecond)
	}
	if s.Stats.LastSuccess.Load() != 0 {
		t.Fatal("provider failure advanced success time")
	}
	f.provider.mu.Lock()
	f.provider.adminStatus = 0
	f.provider.mu.Unlock()
	for s.Stats.Success.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("recovered run not counted")
		}
		time.Sleep(time.Millisecond)
	}
	if s.Stats.LastSuccess.Load() == 0 {
		t.Fatal("successful recovery omitted timestamp")
	}
}

func TestGroupSyncDisableLimitStagesAllChanges(t *testing.T) {
	f := startTokenRouter(t)
	f.oidc.SyncMaxDisable = 1
	subs := syncPeople(t, f, 3)
	f.provider.setUser(subs[2], remoteUser{enabled: true, groups: []string{"students"}})
	s := newSyncer(t, f)
	result, err := s.Sync(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disable limit exceeded") || result.Disabled != 0 || result.Updated != 0 {
		t.Fatal("excessive revocation was not refused", result, err)
	}
	for _, sub := range subs {
		assertSyncPerson(t, f, sub, true, []string{"staff"})
	}
	// A confirmed departure can be applied by explicitly raising the guard.
	f.oidc.SyncMaxDisable = 2
	result, err = s.Sync(t.Context())
	if err != nil || result.Disabled != 2 || result.Updated != 1 {
		t.Fatal("confirmed bulk change failed", result, err)
	}
	assertSyncPerson(t, f, subs[2], true, []string{"students"})
}

func TestGroupSyncOneFailedUserDoesNotStarveOthers(t *testing.T) {
	f := startTokenRouter(t)
	subs := syncPeople(t, f, 3)
	f.provider.setUser(subs[0], remoteUser{status: 500})
	f.provider.setUser(subs[2], remoteUser{enabled: true, groups: []string{"students"}})
	result, err := newSyncer(t, f).Sync(t.Context())
	if err == nil || result.Checked != 3 || result.Failed != 1 || result.Disabled != 1 || result.Updated != 1 {
		t.Fatal("a failed first user blocked other users", result, err)
	}
	assertSyncPerson(t, f, subs[0], true, []string{"staff"})
	assertSyncPerson(t, f, subs[1], false, []string{"staff"})
	assertSyncPerson(t, f, subs[2], true, []string{"students"})
}

func TestGroupSyncDeadlineResumesAfterSlowUser(t *testing.T) {
	f := startTokenRouter(t)
	subs := syncPeople(t, f, 4)
	f.provider.setUser(subs[0], remoteUser{enabled: true, groups: []string{"staff"}, delay: 500 * time.Millisecond})
	s := newSyncer(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	result, err := s.Sync(ctx)
	cancel()
	if err == nil || result.Checked != 1 || result.Failed != 1 || result.Remaining != 3 {
		t.Fatal("slow user did not yield the scan", result, err)
	}
	f.provider.mu.Lock()
	f.provider.adminVisits = nil
	f.provider.mu.Unlock()
	ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err = s.Sync(ctx)
	if err != nil || result.Disabled != 3 {
		t.Fatal("later users were starved on the next run", result, err)
	}
	f.provider.mu.Lock()
	visits := slices.Clone(f.provider.adminVisits)
	f.provider.mu.Unlock()
	if len(visits) == 0 || visits[0] == subs[0] {
		t.Fatal("scan restarted at the slow user", visits)
	}
	for _, sub := range subs[1:] {
		assertSyncPerson(t, f, sub, false, []string{"staff"})
	}
}
