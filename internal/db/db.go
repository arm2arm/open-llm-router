// Package db opens the router's Postgres pool and applies the embedded,
// versioned migrations. Callers use pgx and plain SQL directly.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migration is one numbered schema change, migrations/NNNN_name.sql.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order.
func Migrations() []Migration {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	var list []Migration
	for _, entry := range entries {
		number, name, ok := strings.Cut(strings.TrimSuffix(entry.Name(), ".sql"), "_")
		version, err := strconv.Atoi(number)
		if !ok || err != nil || version <= 0 {
			panic("invalid migration file name " + entry.Name())
		}
		data, err := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		if err != nil {
			panic(err)
		}
		list = append(list, Migration{Version: version, Name: name, SQL: string(data)})
	}
	slices.SortFunc(list, func(a, b Migration) int { return a.Version - b.Version })
	for i, m := range list {
		if m.Version != i+1 {
			panic(fmt.Sprintf("migration versions must be 1..n without gaps; found %d at position %d", m.Version, i+1))
		}
	}
	return list
}

// Open connects to Postgres and applies pending migrations atomically before
// returning the pool. The caller must close it after requests and background
// work finish. ctx should have a startup deadline.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("OLR_DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx's parse errors can contain the original connection string,
		// including its password. Never return that error to startup logs.
		return nil, errors.New("OLR_DATABASE_URL is not a valid Postgres connection string")
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	// Pool creation is lazy. Ping and migration must both succeed before the
	// router can listen or report ready.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// OpenCredentials reserves a separate, small pool for API credential work.
// Invalid keys and verified token identity lookups cannot exhaust the portal,
// accounting or readiness pool, even when its configured maximum is small.
// This pool shares the migrated database; it never runs migrations itself.
func OpenCredentials(ctx context.Context, main *pgxpool.Pool) (*pgxpool.Pool, error) {
	cfg := main.Config().Copy()
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = 4, 0, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("create credential database pool failed")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("connect credential database pool failed")
	}
	return pool, nil
}

// Version returns the highest applied migration.
func Version(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var version int
	err := pool.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&version)
	return version, err
}

// migrate applies all pending migrations in one transaction, so a failure
// leaves the schema as it was. An advisory lock serializes concurrent
// startups: IF NOT EXISTS alone does not prevent races in Postgres's catalog.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations := Migrations()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		const schemaLock int64 = 0x4f4c52534348454d // "OLRSCHEM"
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version integer PRIMARY KEY,
			name text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		var current int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return err
		}
		if current > len(migrations) {
			return fmt.Errorf("database schema version %d is newer than this router's %d; run a newer router", current, len(migrations))
		}
		for _, m := range migrations[current:] {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply database schema: %w", err)
	}
	return nil
}
