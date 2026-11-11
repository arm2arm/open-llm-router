package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type refreshStore struct {
	mu      sync.Mutex
	record  keyRecord
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *refreshStore) lookup(ctx context.Context, _ [32]byte) (keyRecord, error) {
	s.mu.Lock()
	record := s.record
	s.mu.Unlock()
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return record, nil
	case <-ctx.Done():
		return keyRecord{}, ctx.Err()
	}
}
func (*refreshStore) touch(context.Context, string) error { return nil }

func TestConcurrentCacheRefreshUsesOneLookup(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(key))
	store := &refreshStore{record: keyRecord{Principal: Principal{UserID: "user"}, hash: hash[:], enabled: true}, started: make(chan struct{}), release: make(chan struct{})}
	a := &Authenticator{store: store, now: time.Now}
	a.invalidate(true)
	a.cache[hash] = cacheEntry{principal: store.record.Principal, until: time.Now().Add(-time.Second)}
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for range 256 {
		ready.Add(1)
		done.Go(func() {
			ready.Done()
			<-start
			if _, err := a.Authenticate(t.Context(), key); err != nil {
				t.Error(err)
			}
		})
	}
	ready.Wait()
	close(start)
	<-store.started
	// Keep the database operation in flight while the burst reaches auth.
	time.Sleep(25 * time.Millisecond)
	close(store.release)
	done.Wait()
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("one shared refresh made %d database lookups", got)
	}
}

func refreshFixture(t *testing.T) (*Authenticator, *refreshStore, string) {
	t.Helper()
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(key))
	s := &refreshStore{record: keyRecord{Principal: Principal{UserID: "user"}, hash: hash[:], enabled: true}, started: make(chan struct{}), release: make(chan struct{})}
	a := &Authenticator{store: s, now: time.Now}
	a.invalidate(true)
	return a, s, key
}

func TestCancelledRefreshOwnerDoesNotPoisonWaiters(t *testing.T) {
	a, s, key := refreshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	owner := make(chan error, 1)
	go func() { _, err := a.Authenticate(ctx, key); owner <- err }()
	<-s.started
	waiter := make(chan error, 1)
	go func() { _, err := a.Authenticate(t.Context(), key); waiter <- err }()
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner: %v", err)
	}
	close(s.release)
	if err := <-waiter; err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if s.calls.Load() != 2 {
		t.Fatalf("lookups: %d", s.calls.Load())
	}
}

func TestCancelledWaiterDoesNotCancelRefreshOwner(t *testing.T) {
	a, s, key := refreshFixture(t)
	owner := make(chan error, 1)
	go func() { _, err := a.Authenticate(t.Context(), key); owner <- err }()
	<-s.started
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := a.Authenticate(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: %v", err)
	}
	close(s.release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if s.calls.Load() != 1 {
		t.Fatalf("lookups: %d", s.calls.Load())
	}
}

func TestSharedRefreshCannotRepopulateCacheAfterRevocation(t *testing.T) {
	a, s, key := refreshFixture(t)
	done := make(chan error, 1)
	go func() { _, err := a.Authenticate(t.Context(), key); done <- err }()
	<-s.started
	s.mu.Lock()
	s.record.revoked = true
	s.mu.Unlock()
	a.invalidate(true)
	close(s.release)
	if err := <-done; !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key accepted: %v", err)
	}
	if len(a.cache) != 0 {
		t.Fatal("revoked key repopulated cache")
	}
}
