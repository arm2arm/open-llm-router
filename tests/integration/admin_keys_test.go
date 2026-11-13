package integration_test

import (
	"errors"
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
		allowed               bool
	}{
		{[]string{"users:read"}, []string{"192.0.2.4"}, false},
		{[]string{"service:read"}, []string{"192.0.2.0/24"}, false},
		{[]string{"service:read"}, []string{"192.0.2.4"}, true},
	} {
		c, err := adminaccess.Validate(adminaccess.Create{Label: "replacement", Days: 7, Permissions: tc.permissions, Networks: tc.networks, ReplacesID: parent.ID}, 90)
		if err != nil {
			t.Fatal(err)
		}
		_, secret, err := adminaccess.Issue(t.Context(), pool, owner.ID, c, nil, 10, actor)
		if tc.allowed {
			if err != nil {
				t.Fatal("replacement with equal grants refused", err)
			}
			replacementRaw = secret
		} else if !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("replacement broadened grants")
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
