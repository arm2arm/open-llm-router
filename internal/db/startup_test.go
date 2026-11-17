package db

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStartupMigrationHasSeparateBudget(t *testing.T) {
	dsn := os.Getenv("OLR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OLR_TEST_DATABASE_URL not set")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("olr_migration_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+ident+" WITH (FORCE)") })
	databaseURL, err := url.Parse(dsn)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		t.Fatal("OLR_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	databaseURL.Path = "/" + name
	databaseURL.RawPath = ""
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	list := append(Migrations(), Migration{Version: len(Migrations()) + 1, Name: "slow_test", SQL: "SELECT pg_sleep(12)"})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	started := time.Now()
	pool, err := open(ctx, databaseURL.String(), list)
	if err != nil {
		t.Fatal("migration exceeded connection deadline", err)
	}
	defer pool.Close()
	if time.Since(started) < 12*time.Second {
		t.Fatal("slow migration did not run")
	}
	version, err := Version(t.Context(), pool)
	if err != nil || version != len(list) {
		t.Fatal("migration not committed", version, err)
	}
	for _, message := range []string{"database migration started", "database migration finished", "slow_test"} {
		if !strings.Contains(logs.String(), message) {
			t.Fatal("missing migration diagnostic", message)
		}
	}
}

func TestStartupPingRemainsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	started := time.Now()
	pool, err := Open(t.Context(), "postgres://postgres@"+listener.Addr().String()+"/postgres?sslmode=disable&connect_timeout=60")
	if pool != nil || err == nil {
		t.Fatal("unresponsive database accepted")
	}
	if elapsed := time.Since(started); elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatal("connection budget changed", elapsed)
	}
	<-finished
}
