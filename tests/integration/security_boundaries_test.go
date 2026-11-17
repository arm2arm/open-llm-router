package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

func requireAuditAction(t *testing.T, pool *pgxpool.Pool, action string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action=$1 AND result='succeeded'`, action).Scan(&count); err != nil || count == 0 {
		t.Fatalf("missing successful history action %s: %v", action, err)
	}
}

func TestAdministrationKeySecurityBoundaries(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	a := auth.Admin{Pool: pool}
	owner, _, err := a.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeLocalPassword(t.Context(), owner.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "portal_user", ID: owner.ID}
	issue := func(parent *adminaccess.Key, permissions []string, cap int) (adminaccess.Key, string, error) {
		return adminaccess.Issue(t.Context(), pool, owner.ID, adminaccess.Create{Label: "boundary", Days: 1, Permissions: permissions, Networks: []string{"10.0.0.0/24"}}, parent, cap, actor)
	}
	parent, raw, err := issue(nil, []string{"admin_keys:delegate", "admin_keys:write", "service:read"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("permission_subset", func(t *testing.T) {
		if _, _, err := issue(&parent, []string{"users:read"}, 50); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("delegating parent widened permissions", err)
		}
	})
	t.Run("delegation_permission", func(t *testing.T) {
		nonDelegating, _, err := issue(nil, []string{"admin_keys:write", "service:read"}, 50)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := issue(&nonDelegating, []string{"service:read"}, 50); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("non-delegating parent issued a child", err)
		}
	})
	t.Run("narrow_expiry", func(t *testing.T) {
		if err := adminaccess.Narrow(t.Context(), pool, owner.ID, parent.ID, parent.Permissions, parent.Networks, parent.Expires.Add(time.Hour), actor); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("narrow extended expiry", err)
		}
	})
	t.Run("narrow_network", func(t *testing.T) {
		if err := adminaccess.Narrow(t.Context(), pool, owner.ID, parent.ID, parent.Permissions, []string{"10.0.0.0/16"}, parent.Expires, actor); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("narrow widened networks", err)
		}
	})
	t.Run("active_cap", func(t *testing.T) {
		if _, _, err := issue(nil, []string{"service:read"}, 1); !errors.Is(err, adminaccess.ErrLimit) {
			t.Fatal("active key limit bypassed", err)
		}
	})
	t.Run("revoke_owner", func(t *testing.T) {
		other, err := a.CreateLocalUser(t.Context(), "other-key-owner", "", []string{"admins"}, []byte(initialAdminPassword))
		if err != nil {
			t.Fatal(err)
		}
		if err := adminaccess.Revoke(t.Context(), pool, other, parent.ID, actor); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("another owner revoked a key", err)
		}
		if _, err := adminaccess.Lookup(t.Context(), pool, raw); err != nil {
			t.Fatal("foreign revocation changed the key", err)
		}
	})
	t.Run("expired_ancestor", func(t *testing.T) {
		child, _, err := issue(&parent, []string{"service:read"}, 50)
		if err != nil {
			t.Fatal(err)
		}
		execSQL(t, pool, `UPDATE admin_api_keys SET expires=clock_timestamp()-interval '1 second' WHERE id=$1`, parent.ID)
		if err := adminaccess.Active(t.Context(), pool, child); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("expired ancestor still active", err)
		}
		if err := adminaccess.Active(t.Context(), pool, parent); !errors.Is(err, adminaccess.ErrDenied) {
			t.Fatal("expired key still active", err)
		}
		execSQL(t, pool, `UPDATE admin_api_keys SET expires=$2 WHERE id=$1`, parent.ID, parent.Expires)
	})
	if err := adminaccess.Narrow(t.Context(), pool, owner.ID, parent.ID, []string{"service:read"}, parent.Networks, parent.Expires, actor); err != nil {
		t.Fatal(err)
	}
	if err := adminaccess.Revoke(t.Context(), pool, owner.ID, parent.ID, actor); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"local_admin.bootstrap", "credential.rotate", "admin_key.issue", "admin_key.narrow", "admin_key.revoke"} {
		requireAuditAction(t, pool, action)
	}
}

func TestExpiredKeyDeletionOwnerAndHistory(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	a := auth.Admin{Pool: pool}
	one, err := a.CreateLocalUser(t.Context(), "key-owner-one", "", []string{"staff"}, []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.CreateLocalUser(t.Context(), "key-owner-two", "", []string{"staff"}, []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeLocalPassword(t.Context(), one, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateKey(t.Context(), one, "owner test", time.Hour); err != nil {
		t.Fatal(err)
	}
	keys, err := a.ListKeys(t.Context(), one)
	if err != nil || len(keys) != 1 {
		t.Fatal("key listing", err)
	}
	execSQL(t, pool, `UPDATE api_keys SET revoked=true WHERE id=$1`, keys[0].ID)
	if deleted, err := a.RemoveOwnedKey(t.Context(), two, keys[0].ID); err == nil || deleted {
		t.Fatal("foreign revoked key deleted", err)
	}
	if deleted, err := a.RemoveOwnedKey(t.Context(), one, keys[0].ID); err != nil || !deleted {
		t.Fatal("owner deletion failed", err)
	}
	id, err := a.SyncIdentity(t.Context(), "https://identity.example", "audited-subject", "example", "", []string{"staff"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetGroups(t.Context(), id, []string{"students"}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"key.create", "key.delete", "user.groups", "user.identity_sync"} {
		requireAuditAction(t, pool, action)
	}
}
