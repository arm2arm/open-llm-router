package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Postgres tests are opt-in locally and required by the CI service container.
// Use a temporary table so this smoke test leaves a shared dev database intact.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("OLR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OLR_TEST_DATABASE_URL to run Postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to Postgres:", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		if err := conn.Close(closeCtx); err != nil {
			t.Error("close Postgres connection:", err)
		}
	}()
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE olr_ci_probe (value text NOT NULL)`); err != nil {
		t.Fatal("create temporary table:", err)
	}
	const want = "Postgres round trip"
	if _, err := conn.Exec(ctx, `INSERT INTO olr_ci_probe (value) VALUES ($1)`, want); err != nil {
		t.Fatal("insert:", err)
	}
	var got string
	if err := conn.QueryRow(ctx, `SELECT value FROM olr_ci_probe`).Scan(&got); err != nil {
		t.Fatal("query:", err)
	}
	if got != want {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
}
