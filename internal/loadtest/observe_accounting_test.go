package loadtest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5"
)

func TestAccountingFindsIssuedAPIKey(t *testing.T) {
	dsn := os.Getenv("OLR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set OLR_TEST_DATABASE_URL to run Postgres integration tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect to test Postgres:", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := admin.Close(cleanup); err != nil {
			t.Error(err)
		}
	}()
	// Migrate an isolated database; never change the supplied development or CI DB.
	name := "olr_loadtest_" + strings.ToLower(rand.Text())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
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
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != name {
		t.Fatal("test DSN does not select the isolated database")
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	issuer := auth.Admin{Pool: pool}
	userID, err := issuer.AddUser(ctx, "loadtest-accounting", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := issuer.CreateKey(ctx, userID, "loadtest", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.New(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()
	principal, err := authenticator.Authenticate(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	r := runner{o: Options{DatabaseURL: dsn, APIKey: key}}
	if err := r.openAccounting(ctx); err != nil {
		t.Fatal("accounting must find a key issued and accepted by auth:", err)
	}
	defer r.account.pool.Close()
	if r.account.keyID != principal.KeyID || r.account.before != 0 {
		t.Fatal("accounting did not select the issued key and its usage baseline")
	}
	unknown, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	missing := runner{o: Options{DatabaseURL: dsn, APIKey: unknown}}
	if err := missing.openAccounting(ctx); err == nil || missing.account != nil {
		t.Fatal("accounting accepted an unissued key")
	}
}
