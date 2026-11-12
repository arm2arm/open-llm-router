package integration_test

import (
	"errors"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"sync"
	"testing"
	"time"
)

func TestKeyIssuancePolicy(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	owner := user(t, pool, "local", "key-policy")
	store := auth.Admin{Pool: pool, KeyPolicy: config.Auth{MaxKeyLifetimeDays: 1, MaxActiveKeysPerUser: 3}}
	for _, create := range []func() (string, error){
		func() (string, error) {
			return store.CreateKey(t.Context(), owner, "long", 24*time.Hour+time.Microsecond)
		},
		func() (string, error) {
			return store.CreateKeyOfClass(t.Context(), owner, "long", "batch", 48*time.Hour)
		},
		func() (string, error) {
			return store.CreateForwarderKey(t.Context(), owner, "long", "interactive", 48*time.Hour)
		},
	} {
		if _, err := create(); !errors.Is(err, auth.ErrKeyLifetime) {
			t.Fatal("lifetime policy", err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 24)
	for range 24 {
		wg.Go(func() { _, err := store.CreateKey(t.Context(), owner, "concurrent", 24*time.Hour); results <- err })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, auth.ErrKeyLimit) {
			t.Fatal(err)
		}
	}
	if success != 3 {
		t.Fatalf("issued %d keys, want exactly 3", success)
	}
	keys, err := store.ListKeys(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeKey(t.Context(), keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateKey(t.Context(), owner, "after revocation", time.Hour); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `UPDATE api_keys SET expires=clock_timestamp()-interval '1 second' WHERE id=$1`, keys[1].ID)
	if _, err := store.CreateKey(t.Context(), owner, "after expiry", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateKey(t.Context(), owner, "full again", time.Hour); !errors.Is(err, auth.ErrKeyLimit) {
		t.Fatal(err)
	}
}
