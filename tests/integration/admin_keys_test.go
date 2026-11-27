package integration_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
)

func TestAdminKeyDelegationNarrowingAndRecovery(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	owner, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), owner.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "portal_user", ID: owner.ID}
	makeKey := func(parent *adminaccess.Key, permissions, networks []string) (adminaccess.Key, string, error) {
		c, err := adminaccess.Validate(adminaccess.Create{Label: "integration agent", Days: 7, Permissions: permissions, Networks: networks}, 90)
		if err != nil {
			return adminaccess.Key{}, "", err
		}
		return adminaccess.Issue(t.Context(), pool, owner.ID, c, parent, 10, actor)
	}
	parent, raw, err := makeKey(nil, adminaccess.Permissions, []string{"192.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	child, childRaw, err := makeKey(&parent, []string{"service:read"}, []string{"192.0.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Expires.After(parent.Expires) {
		t.Fatal("child outlives parent")
	}
	if _, _, err := makeKey(&child, []string{"users:write"}, []string{"192.0.2.4"}); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("child escalated permission")
	}
	if _, _, err := makeKey(&parent, []string{"service:read"}, []string{"192.0.0.0/16"}); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("child expanded network")
	}
	if err := adminaccess.Narrow(t.Context(), pool, owner.ID, parent.ID, []string{"service:read"}, []string{"192.0.2.4/32"}, parent.Expires, actor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := makeKey(&parent, []string{"users:read"}, []string{"192.0.2.4"}); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("stale parent snapshot issued expanded child after narrowing")
	}
	if _, err := adminaccess.Lookup(t.Context(), pool, childRaw); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("narrowing did not invalidate child")
	}
	if err := adminaccess.Narrow(t.Context(), pool, owner.ID, parent.ID, adminaccess.Permissions, []string{"192.0.2.0/24"}, time.Now().Add(30*24*time.Hour), actor); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("narrowing expanded authority")
	}
	if _, err := adminaccess.Lookup(t.Context(), pool, raw); err != nil {
		t.Fatal("narrowed key lost read permission", err)
	}
	var replacementRaw string
	for _, tc := range []struct {
		permissions, networks []string
		denial                string
	}{
		{[]string{"users:read"}, []string{"192.0.2.4"}, "Permission users:read exceeds"},
		{[]string{"service:read"}, []string{"192.0.2.4", "198.51.100.0/24"}, "Network 198.51.100.0/24 is outside"},
		{[]string{"service:read"}, []string{"192.0.2.0/24"}, "Network 192.0.2.0/24 is outside"},
		{[]string{"service:read"}, []string{"192.0.2.4"}, ""},
	} {
		c, err := adminaccess.Validate(adminaccess.Create{Label: "replacement", Days: 7, Permissions: tc.permissions, Networks: tc.networks, ReplacesID: parent.ID}, 90)
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := adminaccess.Issue(t.Context(), pool, owner.ID, c, nil, 10, actor)
		if tc.denial == "" {
			if err != nil {
				t.Fatal("replacement with equal grants refused", err)
			}
			replacementRaw = secret
		} else if secret != "" || !errors.Is(err, adminaccess.ErrDenied) || !strings.Contains(err.Error(), tc.denial) {
			t.Fatalf("expected a rotation denial identifying the rejected grant, got %v", err)
		}
	}
	other := user(t, pool, "local", "other-key-owner")
	for _, tc := range []struct{ owner, original string }{
		{owner.ID, "invalid-id"},
		{owner.ID, "00000000-0000-0000-0000-000000000000"},
		{other, parent.ID},
	} {
		c := adminaccess.Create{Label: "replacement", Days: 1, Permissions: []string{"service:read"}, Networks: []string{"192.0.2.4/32"}, ReplacesID: tc.original}
		_, secret, err := adminaccess.Issue(t.Context(), pool, tc.owner, c, nil, 10, audit.Actor{Type: "portal_user", ID: tc.owner})
		if secret != "" || !errors.Is(err, adminaccess.ErrDenied) || !strings.Contains(err.Error(), "select a key owned by the signed-in administrator") {
			t.Fatalf("expected an unavailable original key, got %v", err)
		}
	}
	if err := adminaccess.Revoke(t.Context(), pool, owner.ID, parent.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := adminaccess.Lookup(t.Context(), pool, raw); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("revoked key authenticated")
	}
	if _, err := adminaccess.Lookup(t.Context(), pool, replacementRaw); err != nil {
		t.Fatal("revocation of old key disabled its independent replacement", err)
	}
	_, raw, err = makeKey(nil, []string{"service:read"}, []string{"192.0.2.4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), owner.ID, []byte(rotatedAdminPassword), []byte("another-private-admin-password-1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := adminaccess.Lookup(t.Context(), pool, raw); !errors.Is(err, adminaccess.ErrDenied) {
		t.Fatal("password change did not revoke administration access")
	}
}
