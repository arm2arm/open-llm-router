package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/server"
)

type fakeStore struct {
	record  keyRecord
	err     error
	lookups int
	touches int
	onRead  func()
}

func (s *fakeStore) lookup(context.Context, [32]byte) (keyRecord, error) {
	s.lookups++
	r := s.record
	if s.onRead != nil {
		s.onRead()
	}
	return r, s.err
}

func (s *fakeStore) touch(context.Context, string) error { s.touches++; return nil }

func fixture(t *testing.T) (*Authenticator, *fakeStore, string) {
	t.Helper()
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(key))
	s := &fakeStore{record: keyRecord{Principal: Principal{UserID: "user", Username: "pilot", Groups: []string{"staff"}, KeyID: "key"}, hash: hash[:], enabled: true}}
	a := &Authenticator{store: s, now: time.Now}
	a.invalidate(true)
	return a, s, key
}

func TestGenerate(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		key, err := Generate()
		if err != nil || !validKey(key) || seen[key] {
			t.Fatal("generated invalid or repeated key", err)
		}
		seen[key] = true
	}
}

func TestCacheLifetimeAndExpiry(t *testing.T) {
	a, store, key := fixture(t)
	now := time.Now()
	a.now = func() time.Time { return now }
	for range 3 {
		p, err := a.Authenticate(t.Context(), key)
		if err != nil || p.UserID != "user" || p.Username != "pilot" || p.KeyID != "key" || len(p.Groups) != 1 || p.Groups[0] != "staff" {
			t.Fatalf("principal=%+v err=%v", p, err)
		}
		p.Groups[0] = "modified by caller"
	}
	if store.lookups != 1 || store.touches != 1 {
		t.Fatalf("hot path queried store: %d lookups, %d touches", store.lookups, store.touches)
	}
	now = now.Add(cacheTTL)
	if _, err := a.Authenticate(t.Context(), key); err != nil || store.lookups != 2 {
		t.Fatalf("TTL did not refresh: %v, lookups=%d", err, store.lookups)
	}
	expires := now.Add(time.Second)
	store.record.expires = &expires
	a.invalidate(true)
	if _, err := a.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	now = expires
	if _, err := a.Authenticate(t.Context(), key); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("cached key survived expiry", err)
	}
}

func TestInvalidationDuringLookup(t *testing.T) {
	a, store, key := fixture(t)
	store.onRead = func() {
		store.record.revoked = true
		store.onRead = nil
		a.invalidate(true)
	}
	if _, err := a.Authenticate(t.Context(), key); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("in-flight lookup repopulated cache after revoke", err)
	}
}

func TestListenerLossBypassesCache(t *testing.T) {
	a, store, key := fixture(t)
	if _, err := a.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	a.invalidate(false)
	for range 2 {
		if _, err := a.Authenticate(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	if store.lookups != 3 || len(a.cache) != 0 {
		t.Fatal("credentials were cached without invalidation listener")
	}
	store.record.enabled = false
	if _, err := a.Authenticate(t.Context(), key); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("disabled user accepted without listener", err)
	}
}

func TestMiddleware(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens", "/v1/models", "/v1/unknown", "/v1"} {
		for _, state := range []string{"missing", "malformed", "unknown", "revoked", "expired", "disabled", "mismatched hash", "bearer", "x-api-key", "both matching", "both conflicting", "duplicate", "unavailable"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				a, store, key := fixture(t)
				r := httptest.NewRequest("POST", path, nil)
				want := 401
				switch state {
				case "missing":
				case "malformed":
					r.Header.Set("Authorization", "Bearer olr-invalid")
				default:
					r.Header.Set("Authorization", "Bearer "+key)
				}
				switch state {
				case "unknown":
					store.err = ErrInvalidKey
				case "revoked":
					store.record.revoked = true
				case "expired":
					expired := time.Now().Add(-time.Second)
					store.record.expires = &expired
				case "disabled":
					store.record.enabled = false
				case "mismatched hash":
					store.record.hash = make([]byte, 32)
				case "bearer":
					r.Header.Set("Authorization", "bEaReR "+key)
					want = 200
				case "x-api-key":
					r.Header.Del("Authorization")
					r.Header.Set("X-Api-Key", key)
					want = 200
				case "both matching":
					r.Header.Set("X-Api-Key", key)
					want = 200
				case "both conflicting":
					other, _ := Generate()
					r.Header.Set("X-Api-Key", other)
				case "duplicate":
					r.Header.Add("Authorization", "Bearer "+key)
				case "unavailable":
					store.err = errors.New("database unavailable")
					want = 503
				}
				rec := httptest.NewRecorder()
				a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p, ok := FromContext(r.Context())
					if !ok || p.Username != "pilot" || p.Groups[0] != "staff" {
						t.Fatal("principal missing from request context")
					}
					p.Groups[0] = "mutated"
					p, _ = FromContext(r.Context())
					if p.Groups[0] != "staff" {
						t.Fatal("caller mutated context principal")
					}
					w.WriteHeader(200)
				})).ServeHTTP(rec, r)
				if rec.Code != want {
					t.Fatalf("status=%d body=%s want=%d", rec.Code, rec.Body.String(), want)
				}
				if want != 200 {
					var body map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					_, anthropic := body["type"]
					if anthropic != strings.Contains(path, "/messages") {
						t.Fatalf("wrong error shape: %s", rec.Body.String())
					}
					if strings.Contains(rec.Body.String(), key) {
						t.Fatal("error leaked key")
					}
				}
			})
		}
	}
}

func TestServerAuthenticationAndLogs(t *testing.T) {
	a, _, key := fixture(t)
	var logs bytes.Buffer
	srv := server.New(slog.New(slog.NewJSONHandler(&logs, nil)))
	srv.SetAPIMiddleware(a.Middleware)
	srv.Handle("GET /v1/models", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	for _, path := range []string{"/healthz", "/readyz", "/v1/models", "/v1/missing", "/v1//models"} {
		for _, authenticated := range []bool{false, true} {
			req := httptest.NewRequest("GET", path+"?secret="+key, nil)
			if authenticated {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if strings.HasPrefix(path, "/v1/") && !authenticated && rec.Code != 401 {
				t.Fatalf("unprotected route %s: %d", path, rec.Code)
			}
			if !strings.HasPrefix(path, "/v1/") && rec.Code != 200 {
				t.Fatalf("health probe requires authentication: %s", path)
			}
		}
	}
	hash := sha256.Sum256([]byte(key))
	for _, secret := range []string{key, fmt.Sprintf("%x", hash), fmt.Sprintf("%v", hash[:])} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("request log contains key material")
		}
	}
}
