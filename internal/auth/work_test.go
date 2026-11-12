package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestInvalidCredentialCachingAndResponses(t *testing.T) {
	for _, state := range []string{"unknown", "disabled", "revoked", "expired", "password change", "hash mismatch"} {
		t.Run(state, func(t *testing.T) {
			a, s, key := fixture(t)
			switch state {
			case "unknown":
				s.err = ErrInvalidKey
			case "disabled":
				s.record.enabled = false
			case "revoked":
				s.record.revoked = true
			case "expired":
				expiry := time.Now().Add(-time.Second)
				s.record.expires = &expiry
			case "password change":
				s.record.passwordChangeRequired = true
			case "hash mismatch":
				s.record.hash = make([]byte, 32)
			}
			h := a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid key reached handler") }))
			for i := range maxCredentialFailures + 1 {
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r.Header.Set("Authorization", "Bearer "+key)
				// Arbitrary forwarding headers cannot reset digest throttles.
				r.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i))
				r.Header.Set("Forwarded", fmt.Sprintf("for=192.0.2.%d", i))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 401
				if i == maxCredentialFailures {
					want = 429
				}
				if w.Code != want || (want == 429 && w.Header().Get("Retry-After") != "5") {
					t.Fatalf("attempt %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			if s.lookups != 1 || s.touches != 0 {
				t.Fatalf("negative hits performed database work: %d/%d", s.lookups, s.touches)
			}
		})
	}
}

func TestNegativeCacheExpiryAndInvalidation(t *testing.T) {
	for _, listening := range []bool{true, false} {
		t.Run(fmt.Sprint(listening), func(t *testing.T) {
			a, s, key := fixture(t)
			a.invalidate(listening)
			now := time.Now()
			a.now = func() time.Time { return now }
			s.err = ErrInvalidKey
			_, _ = a.Authenticate(t.Context(), key)
			s.err = nil // Simulate a newly available key, even without LISTEN.
			if _, err := a.Authenticate(t.Context(), key); !errors.Is(err, ErrInvalidKey) {
				t.Fatal("denial not cached", err)
			}
			now = now.Add(negativeTTL)
			if _, err := a.Authenticate(t.Context(), key); err != nil {
				t.Fatal("new key remained denied after TTL", err)
			}
			a.invalidate(listening)
			s.record.enabled = false
			_, _ = a.Authenticate(t.Context(), key)
			s.record.enabled = true
			a.invalidate(listening)
			if _, err := a.Authenticate(t.Context(), key); err != nil {
				t.Fatal("admin invalidation did not clear denial", err)
			}
		})
	}
}

func TestTransientLookupErrorsAreNotCached(t *testing.T) {
	a, s, key := fixture(t)
	s.err = errors.New("database unavailable")
	_, _ = a.Authenticate(t.Context(), key)
	s.err = nil
	if _, err := a.Authenticate(t.Context(), key); err != nil || s.lookups != 2 {
		t.Fatal("transient error cached", err, s.lookups)
	}
}

func TestFailureCacheBoundAndGeneration(t *testing.T) {
	var c FailureCache
	now := time.Now()
	for i := range maxNegativeCredentials * 2 {
		hash := sha256.Sum256(fmt.Appendf(nil, "candidate-%d", i))
		c.Remember(hash, 1, now)
	}
	if len(c.entries) != maxNegativeCredentials || c.order.Len() != maxNegativeCredentials {
		t.Fatal("negative cache exceeded its bound")
	}
	hash := sha256.Sum256([]byte("candidate-8191"))
	if err := c.Check(hash, 2, now); err != nil {
		t.Fatal("previous generation denial reused", err)
	}
}

func TestCredentialWorkBoundsUniqueMissesAndWaiters(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			a, s, key := refreshFixture(t)
			var wg sync.WaitGroup
			for i := range MaxCredentialWork {
				candidate := key
				if !sameKey {
					candidate = fmt.Sprintf("olr-%043d", i)
				}
				wg.Go(func() { _, _ = a.Authenticate(t.Context(), candidate) })
			}
			deadline := time.Now().Add(time.Second)
			for len(a.work.slots) != MaxCredentialWork && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(a.work.slots) != MaxCredentialWork {
				close(s.release)
				wg.Wait()
				t.Fatal("work did not reach the budget")
			}
			for i := range 256 {
				candidate := fmt.Sprintf("olr-%043d", i+1000)
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r.Header.Set("X-Api-Key", candidate)
				w := httptest.NewRecorder()
				start := time.Now()
				a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("overload reached handler") })).ServeHTTP(w, r)
				if w.Code != 503 || w.Header().Get("Retry-After") != "1" || time.Since(start) > time.Second {
					t.Fatalf("overload did not fail fast: %d", w.Code)
				}
			}
			// Cached valid credentials bypass saturated miss admission.
			hash := sha256.Sum256([]byte(key))
			a.mu.Lock()
			a.cache[hash] = cacheEntry{principal: Principal{UserID: "cached"}, until: time.Now().Add(time.Minute)}
			a.mu.Unlock()
			if p, err := a.Authenticate(t.Context(), key); err != nil || p.UserID != "cached" {
				t.Error("cached credential blocked by misses", err)
			}
			close(s.release)
			wg.Wait()
			if got := s.calls.Load(); got > MaxCredentialWork || (sameKey && got != 1) {
				t.Fatal("database work unbounded", got)
			}
			if len(a.work.slots) != 0 {
				t.Fatal("work slots leaked")
			}
		})
	}
}

func TestCancelledMissReleasesWork(t *testing.T) {
	a, s, key := refreshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := a.Authenticate(ctx, key); done <- err }()
	<-s.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || len(a.work.slots) != 0 {
		t.Fatal("cancelled lookup retained work", err)
	}
}
