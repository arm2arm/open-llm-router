package adminaccess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var Permissions = []string{"service:read", "usage:read", "audit:read", "users:read", "users:write", "users:credentials", "routing:read", "routing:write", "authentication:read", "authentication:write", "admin_keys:read", "admin_keys:write", "admin_keys:delegate"}
var ErrDenied = errors.New("administration access denied")
var ErrInput = errors.New("invalid administration key settings")
var ErrLimit = errors.New("active administration key limit reached")

type Key struct {
	Active        bool       `json:"active"`
	ID            string     `json:"id"`
	OwnerID       string     `json:"owner_id"`
	ParentID      *string    `json:"parent_id"`
	Label         string     `json:"label"`
	Prefix        string     `json:"prefix"`
	Permissions   []string   `json:"permissions"`
	Networks      []string   `json:"allowed_networks"`
	Created       time.Time  `json:"created"`
	Expires       time.Time  `json:"expires"`
	Revoked       bool       `json:"revoked"`
	PolicyVersion int64      `json:"policy_version"`
	LastUsed      *time.Time `json:"last_used"`
	LastIP        *string    `json:"last_ip"`
}
type Create struct {
	Label       string   `json:"label"`
	Permissions []string `json:"permissions"`
	Networks    []string `json:"allowed_networks"`
	Days        int      `json:"expires_in_days"`
	ReplacesID  string   `json:"replaces_key_id,omitempty"`
}

func (k Key) Allows(permission string) bool { return slices.Contains(k.Permissions, permission) }
func Sensitive(permissions []string) bool {
	for _, p := range permissions {
		if p == "authentication:write" || p == "users:credentials" || p == "admin_keys:delegate" {
			return true
		}
	}
	return false
}
func Validate(c Create, maxDays int) (Create, error) {
	c.Label = strings.TrimSpace(c.Label)
	if c.Label == "" || len(c.Label) > 100 || c.Days < 1 || c.Days > maxDays || len(c.Permissions) == 0 || len(c.Permissions) > len(Permissions) {
		return c, ErrInput
	}
	for _, p := range c.Permissions {
		if !slices.Contains(Permissions, p) {
			return c, ErrInput
		}
	}
	slices.Sort(c.Permissions)
	c.Permissions = slices.Compact(c.Permissions)
	var err error
	c.Networks, err = Networks(c.Networks)
	if err != nil {
		return c, err
	}
	return c, nil
}

const columns = `id::text,owner_id::text,parent_id::text,label,prefix,permissions,allowed_networks,created,expires,revoked,policy_version,last_used,last_ip`

func scan(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.OwnerID, &k.ParentID, &k.Label, &k.Prefix, &k.Permissions, &k.Networks, &k.Created, &k.Expires, &k.Revoked, &k.PolicyVersion, &k.LastUsed, &k.LastIP)
	return k, err
}
func Lookup(ctx context.Context, pool *pgxpool.Pool, raw string) (Key, error) {
	if !strings.HasPrefix(raw, "olr-admin-") || len(raw) != 53 {
		return Key{}, ErrDenied
	}
	hash := sha256.Sum256([]byte(raw))
	k, err := scan(pool.QueryRow(ctx, `SELECT `+columns+` FROM admin_api_keys WHERE hash=$1`, hash[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrDenied
	}
	if err != nil {
		return k, err
	}
	if err = Active(ctx, pool, k); err != nil {
		return k, err
	}
	return k, nil
}

// Active checks every ancestor in the database. No positive authorization cache.
func Active(ctx context.Context, pool *pgxpool.Pool, k Key) error {
	var valid bool
	err := pool.QueryRow(ctx, `WITH RECURSIVE chain AS (
 SELECT id,parent_id,owner_id,revoked,expires FROM admin_api_keys WHERE id=$1
 UNION ALL SELECT p.id,p.parent_id,p.owner_id,p.revoked,p.expires FROM admin_api_keys p JOIN chain c ON c.parent_id=p.id
 ) SELECT COALESCE(bool_and(NOT c.revoked AND c.expires>clock_timestamp() AND u.enabled AND NOT COALESCE(l.must_change_password,false)),false)
 FROM chain c JOIN users u ON u.id=c.owner_id LEFT JOIN local_credentials l ON l.user_id=u.id`, k.ID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrDenied
	}
	return nil
}
func List(ctx context.Context, pool *pgxpool.Pool, owner string) ([]Key, error) {
	rows, err := pool.Query(ctx, `SELECT `+columns+` FROM admin_api_keys WHERE owner_id=$1 ORDER BY created DESC LIMIT 500`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []Key{}
	for rows.Next() {
		k, err := scan(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range keys {
		err := Active(ctx, pool, keys[i])
		if err != nil && !errors.Is(err, ErrDenied) {
			return nil, err
		}
		keys[i].Active = err == nil
	}
	return keys, nil
}
func Issue(ctx context.Context, pool *pgxpool.Pool, owner string, c Create, parent *Key, maxActive int, actor audit.Actor) (Key, string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Key{}, "", err
	}
	secret := "olr-admin-" + base64.RawURLEncoding.EncodeToString(entropy[:])
	hash := sha256.Sum256([]byte(secret))
	var key Key
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT enabled FROM users WHERE id=$1 FOR UPDATE`, owner).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return ErrDenied
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_api_keys WHERE owner_id=$1 AND NOT revoked AND expires>clock_timestamp()`, owner).Scan(&count); err != nil {
			return err
		}
		if count >= maxActive {
			return ErrLimit
		}
		if c.ReplacesID != "" {
			previous, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM admin_api_keys WHERE id=$1 AND owner_id=$2 FOR SHARE`, c.ReplacesID, owner))
			if err != nil || !Subnets(c.Networks, previous.Networks) {
				return ErrDenied
			}
			for _, permission := range c.Permissions {
				if !previous.Allows(permission) {
					return ErrDenied
				}
			}
		}
		var parentID *string
		expires := time.Now().UTC().Add(time.Duration(c.Days) * 24 * time.Hour)
		if parent != nil {
			if !parent.Allows("admin_keys:delegate") || !parent.Allows("admin_keys:write") || owner != parent.OwnerID || !Subnets(c.Networks, parent.Networks) {
				return ErrDenied
			}
			for _, p := range c.Permissions {
				if !parent.Allows(p) {
					return ErrDenied
				}
			}
			if expires.After(parent.Expires) {
				expires = parent.Expires
			}
			parentID = &parent.ID
			// Hold the parent against concurrent revocation until issuance commits.
			current, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM admin_api_keys WHERE id=$1 FOR SHARE`, parent.ID))
			if err != nil {
				return err
			}
			if current.Revoked || !current.Expires.After(time.Now()) || current.PolicyVersion != parent.PolicyVersion {
				return ErrDenied
			}
		}
		var err error
		key, err = scan(tx.QueryRow(ctx, `INSERT INTO admin_api_keys(owner_id,parent_id,label,prefix,hash,permissions,allowed_networks,expires) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+columns, owner, parentID, c.Label, secret[:18], hash[:], c.Permissions, c.Networks, expires))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin_key.issue", Actor: actor, TargetType: "admin_key", TargetID: key.ID, Result: audit.Succeeded})
	})
	if err != nil {
		return Key{}, "", err
	}
	key.Active = true
	return key, secret, nil
}
func Revoke(ctx context.Context, pool *pgxpool.Pool, owner, id string, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE owner_id=$1 AND id=$2`, owner, id)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrDenied
		}
		if _, err := tx.Exec(ctx, `WITH RECURSIVE children AS (SELECT id FROM admin_api_keys WHERE parent_id=$1 UNION ALL SELECT k.id FROM admin_api_keys k JOIN children c ON k.parent_id=c.id) UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE id IN (SELECT id FROM children)`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin_key.revoke", Actor: actor, TargetType: "admin_key", TargetID: id, Result: audit.Succeeded})
	})
}

// Narrow can only reduce an existing key's authority. Rotation issues a new key.
func Narrow(ctx context.Context, pool *pgxpool.Pool, owner, id string, permissions, networks []string, expires time.Time, actor audit.Actor) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		k, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM admin_api_keys WHERE id=$1 AND owner_id=$2 FOR UPDATE`, id, owner))
		if err != nil {
			return ErrDenied
		}
		if k.Revoked || expires.After(k.Expires) || !expires.After(time.Now()) || len(permissions) == 0 || !Subnets(networks, k.Networks) {
			return ErrDenied
		}
		for _, p := range permissions {
			if !k.Allows(p) {
				return ErrDenied
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE admin_api_keys SET permissions=$2,allowed_networks=$3,expires=$4,policy_version=policy_version+1 WHERE id=$1`, id, permissions, networks, expires); err != nil {
			return err
		}
		// Existing descendants may otherwise retain permissions removed from this key.
		if _, err := tx.Exec(ctx, `WITH RECURSIVE children AS (SELECT id FROM admin_api_keys WHERE parent_id=$1 UNION ALL SELECT k.id FROM admin_api_keys k JOIN children c ON k.parent_id=c.id) UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE id IN (SELECT id FROM children)`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "admin_key.narrow", Actor: actor, TargetType: "admin_key", TargetID: id, Result: audit.Succeeded})
	})
}
