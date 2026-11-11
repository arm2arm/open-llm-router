// Package db opens the router's Postgres pool and applies the embedded pilot
// schema. Callers use pgx and plain SQL directly.
package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Open connects to Postgres and applies the schema atomically before returning
// the pool. The caller must close it after requests and background work finish.
// ctx should have a startup deadline.
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
	// Pool creation is lazy. Ping and schema application must both succeed
	// before the router can listen or report ready.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Serialize concurrent startups: IF NOT EXISTS alone does not prevent
		// races in Postgres's catalog when two processes create the same table.
		const schemaLock int64 = 0x4f4c52534348454d // "OLRSCHEM"
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLock); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schema)
		return err
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply database schema: %w", err)
	}
	return pool, nil
}
