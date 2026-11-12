package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCredentialFloodPreservesPortalDatabaseConnections(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	browser.login(t, "reader", []string{"staff"})
	credentials, err := db.OpenCredentials(t.Context(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	if credentials.Config().MaxConns != 4 || credentials.Config().MinConns != 0 {
		t.Fatal("credential pool has no fixed connection budget")
	}
	a, err := auth.New(t.Context(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Hold every credential connection to simulate slow authentication SQL.
	// All portal/session/accounting traffic still uses the original pool.
	var held []*pgxpool.Conn
	for range 4 {
		conn, err := credentials.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	results := make(chan error, 64)
	for i := range 64 {
		wg.Go(func() {
			_, err := a.Authenticate(ctx, fmt.Sprintf("olr-%043d", i))
			results <- err
		})
	}
	select {
	case err := <-results:
		if !errors.Is(err, auth.ErrAuthBusy) {
			t.Fatal("credential overload did not fail fast", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential overload queued behind the database")
	}
	started := time.Now()
	for _, path := range []string{"/portal/api/me", "/portal/api/keys"} {
		if resp, body := browser.call(t, "GET", path, "", ""); resp.StatusCode != 200 {
			t.Fatal("credential flood blocked portal", path, resp.StatusCode, string(body))
		}
	}
	check, cancelCheck := context.WithTimeout(t.Context(), time.Second)
	defer cancelCheck()
	if err := f.pool.Ping(check); err != nil || time.Since(started) > time.Second {
		t.Fatal("credential flood blocked normal database traffic", err)
	}
}
