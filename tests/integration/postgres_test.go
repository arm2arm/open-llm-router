package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type testDatabase struct {
	dsn   string
	name  string
	admin *pgx.Conn
}

// Every test owns a fresh database, not the shared Compose/CI database. The
// supplied role needs CREATEDB and permission to terminate its own connections.
func newDatabase(t *testing.T) testDatabase {
	t.Helper()
	dsn := os.Getenv("OLR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OLR_TEST_DATABASE_URL to run Postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test Postgres:", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	name := "olr_test_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal("create isolated test database:", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error("drop test database:", err)
		}
	})
	// ConnConfig.ConnString returns the original parsed string even after
	// changing Config.Database. Rewrite the DSN itself so Open gets our DB.
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		query := u.Query()
		query.Del("database")
		query.Set("dbname", name)
		u.RawQuery = query.Encode()
		dsn = u.String()
	} else {
		dsn += " dbname=" + name
	}
	check, err := pgx.ParseConfig(dsn)
	if err != nil || check.Database != name {
		t.Fatal("test DSN does not select the isolated database")
	}
	return testDatabase{dsn: dsn, name: name, admin: admin}
}

func openDatabase(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func user(t *testing.T, pool *pgxpool.Pool, issuer, sub string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(), `INSERT INTO users (issuer, sub, email)
		VALUES ($1, $2, 'shared@example.org') RETURNING id`, issuer, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func wantConstraint(t *testing.T, pool *pgxpool.Pool, code, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(t.Context(), sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("constraint error = %v, want SQLSTATE %s", err, code)
	}
}

func TestSchemaAddsStreamFlagToExistingUsage(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	id := user(t, pool, "local", "legacy")
	execSQL(t, pool, `ALTER TABLE usage DROP COLUMN is_stream`)
	execSQL(t, pool, `INSERT INTO usage(user_id,model,replica,protocol,status,input_tokens,output_tokens,latency_ms,ttfb_ms)
		VALUES ($1,'chat-model','http://backend','openai','200',3,7,100,30)`, id)
	upgraded := openDatabase(t, database.dsn)
	var stream bool
	var input, output, ttfb int64
	if err := upgraded.QueryRow(t.Context(), `SELECT is_stream,input_tokens,output_tokens,ttfb_ms FROM usage WHERE user_id=$1`, id).
		Scan(&stream, &input, &output, &ttfb); err != nil || stream || input != 3 || output != 7 || ttfb != 30 {
		t.Fatal(stream, input, output, ttfb, err)
	}
}

func TestSchemaStartup(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	id := user(t, pool, "local", "pilot")
	var enabled bool
	var groups []string
	var synced time.Time
	if err := pool.QueryRow(t.Context(), `SELECT enabled, groups, synced_at FROM users WHERE id = $1`, id).
		Scan(&enabled, &groups, &synced); err != nil || !enabled || len(groups) != 0 || synced.IsZero() {
		t.Fatalf("user defaults: enabled=%v groups=%v synced=%v err=%v", enabled, groups, synced, err)
	}
	// OIDs prove the second startup keeps the same tables and index. It must
	// also preserve existing data, rather than recreating an empty schema.
	objects := func(p *pgxpool.Pool) []uint32 {
		t.Helper()
		var ids []uint32
		err := p.QueryRow(t.Context(), `SELECT ARRAY[
			'users'::regclass::oid, 'api_keys'::regclass::oid, 'sessions'::regclass::oid,
			'usage'::regclass::oid, 'usage_daily'::regclass::oid, 'usage_user_id_ts_idx'::regclass::oid]`).Scan(&ids)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	before := objects(pool)
	pool.Close()
	pool = openDatabase(t, database.dsn)
	after := objects(pool)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("schema object changed: %v -> %v", before, after)
		}
	}
	var got string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM users WHERE issuer = 'local' AND sub = 'pilot'`).Scan(&got); err != nil || got != id {
		t.Fatalf("user after restart = %q, %v", got, err)
	}
}

func TestConcurrentStartup(t *testing.T) {
	database := newDatabase(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			pool, err := db.Open(ctx, database.dsn)
			if err != nil {
				t.Error(err)
				return
			}
			pool.Close()
		})
	}
	wg.Wait()
}

func TestSchemaFailureRollsBack(t *testing.T) {
	database := newDatabase(t)
	conn, err := pgx.Connect(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	// Force an error after users, keys and sessions have been created. An
	// incompatible pre-existing table must fail startup without partial DDL.
	if _, err := conn.Exec(t.Context(), `CREATE TABLE usage (incompatible text)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, database.dsn)
	if pool != nil || err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("incompatible schema was accepted")
	}
	var tables []string
	if err := conn.QueryRow(t.Context(), `SELECT array_agg(tablename::text ORDER BY tablename)
		FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0] != "usage" {
		t.Fatalf("schema application left partial DDL: %v", tables)
	}
}

func TestUserIdentity(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	first := user(t, pool, "https://issuer-a.example", "same-sub")
	second := user(t, pool, "https://issuer-b.example", "same-sub")
	third := user(t, pool, "https://issuer-a.example", "other-sub")
	if first == second || first == third || second == third {
		t.Fatal("distinct logins shared a surrogate ID")
	}
	wantConstraint(t, pool, "23505", `INSERT INTO users (issuer, sub) VALUES ('https://issuer-a.example', 'same-sub')`)
}

func TestDeletionRules(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	owner := user(t, pool, "local", "owner")
	other := user(t, pool, "local", "other")
	hash := sha256.Sum256([]byte("test key"))
	var key string
	var class string
	var trusted, revoked bool
	if err := pool.QueryRow(t.Context(), `INSERT INTO api_keys (user_id, prefix, hash)
		VALUES ($1, 'olr-test', $2) RETURNING id, class, trusted_forwarder, revoked`, owner, hash[:]).
		Scan(&key, &class, &trusted, &revoked); err != nil || class != "interactive" || trusted || revoked {
		t.Fatalf("key defaults: class=%q trusted=%v revoked=%v err=%v", class, trusted, revoked, err)
	}
	wantConstraint(t, pool, "23505", `INSERT INTO api_keys (user_id, prefix, hash) VALUES ($1, 'another', $2)`, other, hash[:])
	wantConstraint(t, pool, "23503", `INSERT INTO api_keys (user_id, prefix, hash) VALUES (gen_random_uuid(), 'orphan', 'orphan')`)
	for _, id := range []string{owner, other} {
		execSQL(t, pool, `INSERT INTO sessions (id_hash, user_id, expires) VALUES ($1, $2, now() + interval '1 day')`, []byte(id), id)
		execSQL(t, pool, `INSERT INTO usage (key_id, user_id, model, replica, protocol, status, input_tokens, output_tokens, latency_ms, ttfb_ms)
			VALUES ($1, $2, 'chat-model', 'mock-a', 'openai', '200', 12, 34, 100, 10)`, key, id)
		execSQL(t, pool, `INSERT INTO usage_daily (day, user_id, model, requests, input_tokens, output_tokens)
			VALUES (current_date, $1, 'chat-model', 1, 12, 34)`, id)
	}
	wantConstraint(t, pool, "23505", `INSERT INTO usage_daily (day, user_id, model) VALUES (current_date, $1, 'chat-model')`, owner)
	execSQL(t, pool, `DELETE FROM api_keys WHERE id = $1`, key)
	var retained int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE key_id IS NULL AND input_tokens = 12 AND output_tokens = 34`).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("usage retained after key deletion = %d, %v", retained, err)
	}
	// Recreate a key to prove user deletion cascades through keys as well.
	execSQL(t, pool, `INSERT INTO api_keys (user_id, prefix, hash) VALUES ($1, 'olr-test', $2)`, owner, hash[:])
	// A forwarded request is also personal data of the referenced end user.
	execSQL(t, pool, `INSERT INTO usage (user_id, forwarded_for, model, replica, protocol, status, latency_ms)
		VALUES ($1, $2, 'chat-model', 'mock-a', 'anthropic', 'aborted', 5)`, other, owner)
	execSQL(t, pool, `DELETE FROM users WHERE id = $1`, owner)
	for _, table := range []string{"api_keys", "sessions", "usage", "usage_daily"} {
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE user_id = $1", owner).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s rows left for deleted user = %d, %v", table, count, err)
		}
	}
	for _, table := range []string{"users", "sessions", "usage", "usage_daily"} {
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 1 {
			t.Errorf("%s rows for unaffected user = %d, %v", table, count, err)
		}
	}
}
