// Package auth authenticates API keys and manages bootstrap users and keys.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apierr"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/reqid"
)

const cacheTTL = time.Minute
const maxCachedKeys = 4096
const keyLength = 4 + 43 // 32 random bytes, encoded as fixed-width base62

var ErrInvalidKey = errors.New("invalid API key")
var ErrNotFound = errors.New("user or key not found")

// Principal identifies the caller for authorization and usage accounting.
type Principal struct {
	UserID   string
	Username string
	Groups   []string
	KeyID    string
}

type contextKey struct{}

// NewContext carries a trusted, authenticated principal to downstream handlers.
func NewContext(ctx context.Context, p Principal) context.Context {
	p.Groups = slices.Clone(p.Groups)
	return context.WithValue(ctx, contextKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	p.Groups = slices.Clone(p.Groups)
	return p, ok
}

// Generate returns a new plaintext key. Only its SHA-256 digest and display
// prefix may be persisted. No plaintext key is retained by Authenticator.
func Generate() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", errors.New("generate API key: random source unavailable")
	}
	encoded := new(big.Int).SetBytes(entropy[:]).Text(62)
	return "olr-" + strings.Repeat("0", 43-len(encoded)) + encoded, nil
}

func validKey(key string) bool {
	if len(key) != keyLength || !strings.HasPrefix(key, "olr-") {
		return false
	}
	for _, c := range key[4:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

type keyRecord struct {
	Principal
	hash    []byte
	expires *time.Time
	enabled bool
	revoked bool
}

type keyStore interface {
	lookup(context.Context, [32]byte) (keyRecord, error)
	touch(context.Context, string) error
}

type cacheEntry struct {
	principal Principal
	until     time.Time
}

// Authenticator caches successful lookups only. Changes to users or keys clear
// the cache through a dedicated Postgres LISTEN connection (see notifications.go).
type Authenticator struct {
	store      keyStore
	mu         sync.Mutex
	cache      map[[32]byte]cacheEntry
	generation uint64
	listening  bool
	now        func() time.Time
	cancel     context.CancelFunc
	done       chan struct{}
}

func (a *Authenticator) invalidate(listening bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation++
	a.cache = make(map[[32]byte]cacheEntry)
	a.listening = listening
}

func (a *Authenticator) Authenticate(ctx context.Context, key string) (Principal, error) {
	if !validKey(key) {
		return Principal{}, ErrInvalidKey
	}
	hash := sha256.Sum256([]byte(key))
	for {
		if err := ctx.Err(); err != nil {
			return Principal{}, err
		}
		a.mu.Lock()
		generation := a.generation
		entry, ok := a.cache[hash]
		if ok && a.listening && a.now().Before(entry.until) {
			a.mu.Unlock()
			entry.principal.Groups = slices.Clone(entry.principal.Groups)
			return entry.principal, nil
		}
		delete(a.cache, hash)
		a.mu.Unlock()

		record, err := a.store.lookup(ctx, hash)
		if err != nil {
			return Principal{}, err
		}
		now := a.now()
		if subtle.ConstantTimeCompare(hash[:], record.hash) != 1 || !record.enabled || record.revoked ||
			(record.expires != nil && !now.Before(*record.expires)) {
			return Principal{}, ErrInvalidKey
		}
		if err := a.store.touch(ctx, record.KeyID); err != nil {
			return Principal{}, err
		}
		until := now.Add(cacheTTL)
		if record.expires != nil && record.expires.Before(until) {
			until = *record.expires
		}
		a.mu.Lock()
		if generation != a.generation {
			// A revoke may have committed while the lookup was in flight.
			a.mu.Unlock()
			continue
		}
		if a.listening {
			if len(a.cache) >= maxCachedKeys {
				a.cache = make(map[[32]byte]cacheEntry)
			}
			a.cache[hash] = cacheEntry{principal: record.Principal, until: until}
		}
		a.mu.Unlock()
		record.Groups = slices.Clone(record.Groups)
		return record.Principal, nil
	}
}

// Middleware authenticates the entire /v1 namespace, including unknown routes
// and unsupported methods, before the mux can redirect or reject a request.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" && !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		protocol := config.ProtocolOpenAI
		if r.URL.Path == "/v1/messages" || strings.HasPrefix(r.URL.Path, "/v1/messages/") || r.Header.Get("Anthropic-Version") != "" {
			protocol = config.ProtocolAnthropic
		}
		key, err := requestKey(r)
		var p Principal
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			p, err = a.Authenticate(ctx, key)
			cancel()
		}
		if err != nil {
			if errors.Is(err, ErrInvalidKey) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="olr"`)
				apierr.Write(w, protocol, 401, "authentication_error", "a valid API key is required")
			} else {
				apierr.Write(w, protocol, 503, "api_error", "authentication is temporarily unavailable")
			}
			return
		}
		if details := reqid.DetailsFromContext(r.Context()); details != nil {
			details.UserID, details.KeyPrefix = p.UserID, key[:12]
		}
		next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), p)))
	})
}

func requestKey(r *http.Request) (string, error) {
	// Reject ambiguity rather than choosing a credential based on header order.
	auth, api := r.Header.Values("Authorization"), r.Header.Values("X-Api-Key")
	if len(auth) > 1 || len(api) > 1 {
		return "", ErrInvalidKey
	}
	var bearer, key string
	if len(auth) == 1 {
		parts := strings.Split(auth[0], " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !validKey(parts[1]) {
			return "", ErrInvalidKey
		}
		bearer = parts[1]
	}
	if len(api) == 1 {
		key = api[0]
		if !validKey(key) {
			return "", ErrInvalidKey
		}
	}
	if bearer != "" {
		if key != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(key)) != 1 {
			return "", ErrInvalidKey
		}
		return bearer, nil
	}
	if key == "" {
		return "", ErrInvalidKey
	}
	return key, nil
}
