// Package auth authenticates API keys and manages bootstrap users and keys.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"maps"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apierr"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const cacheTTL = time.Minute
const maxCachedKeys = 4096
const keyLength = 4 + 43 // 32 random bytes, encoded as fixed-width base62

var ErrInvalidKey = errors.New("invalid API key")
var ErrNotFound = errors.New("user or key not found")
var errCacheChanged = errors.New("authentication cache changed during lookup")

// Principal identifies the caller for authorization and usage accounting.
type Principal struct {
	UserID    string
	Username  string
	Groups    []string
	KeyID     string
	KeyClass  string            // "interactive" or "batch"
	Overrides map[string]*int64 // per-user limits; treat as read-only
	// ProtectedAdmin is read from the database, never from token claims or groups.
	ProtectedAdmin bool
	// TrustedForwarder marks a key that may name the end user of a call.
	TrustedForwarder bool
	// ForwardedBy is the key owner when the principal is an end user named
	// by a trusted forwarder; UserID, groups and limits are then the end user's.
	ForwardedBy string
}

type contextKey struct{}

// NewContext carries a trusted, authenticated principal to downstream handlers.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p.clone())
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p.clone(), ok
}

func (p Principal) clone() Principal {
	p.Groups = slices.Clone(p.Groups)
	p.Overrides = maps.Clone(p.Overrides)
	return p
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
	hash                   []byte
	expires                *time.Time
	enabled                bool
	revoked                bool
	passwordChangeRequired bool
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
	// Forwarders is installed before serving; see config.Forwarders.
	Forwarders config.Forwarders
	// Tokens, if set before serving, authenticates a bearer token that is
	// not an API key, such as an identity-provider access token.
	Tokens     func(context.Context, string) (Principal, error)
	pool       *pgxpool.Pool
	endUsers   map[string]endUserEntry
	store      keyStore
	mu         sync.Mutex
	cache      map[[32]byte]cacheEntry
	refreshing map[[32]byte]chan struct{}
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
	a.endUsers = make(map[string]endUserEntry)
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
			return entry.principal.clone(), nil
		}
		delete(a.cache, hash)
		var refresh chan struct{}
		if a.listening {
			if pending := a.refreshing[hash]; pending != nil {
				a.mu.Unlock()
				select {
				case <-ctx.Done():
					return Principal{}, ctx.Err()
				case <-pending:
					continue // Recheck cache, expiry and invalidation generation.
				}
			}
			if a.refreshing == nil {
				a.refreshing = make(map[[32]byte]chan struct{})
			}
			refresh = make(chan struct{})
			a.refreshing[hash] = refresh
		}
		a.mu.Unlock()
		principal, err := a.refresh(ctx, hash, generation)
		if refresh != nil {
			a.mu.Lock()
			delete(a.refreshing, hash)
			close(refresh)
			a.mu.Unlock()
		}
		if errors.Is(err, errCacheChanged) {
			continue
		}
		return principal, err
	}
}

// refresh is shared only while the invalidation listener is healthy. Waiters
// own their contexts; a cancelled owner wakes them to retry, rather than
// cancelling their requests. Without a listener every call still reads the DB.
func (a *Authenticator) refresh(ctx context.Context, hash [32]byte, generation uint64) (Principal, error) {
	record, err := a.store.lookup(ctx, hash)
	if err != nil {
		return Principal{}, err
	}
	now := a.now()
	if subtle.ConstantTimeCompare(hash[:], record.hash) != 1 || !record.enabled || record.revoked || record.passwordChangeRequired ||
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
	defer a.mu.Unlock()
	if generation != a.generation {
		// Never repopulate the cache from a lookup that raced a revocation.
		return Principal{}, errCacheChanged
	}
	if a.listening {
		if len(a.cache) >= maxCachedKeys {
			a.cache = make(map[[32]byte]cacheEntry)
		}
		a.cache[hash] = cacheEntry{principal: record.Principal, until: until}
	}
	return record.Principal.clone(), nil
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
		var p Principal
		key, err := requestKey(r)
		if token := bearerToken(r); err != nil && a.Tokens != nil && token != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			p, err = a.Tokens(ctx, token)
			cancel()
			key = "token-------" // a display prefix for the request log
		} else if err == nil {
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
		r = r.Clone(r.Context())
		p, err = a.forward(r, p)
		if err != nil {
			switch {
			case errors.Is(err, errUnknownEndUser), errors.Is(err, errDisabledEndUser):
				apierr.Write(w, protocol, 403, "permission_error", "the end user is unknown or disabled")
			case errors.Is(err, errEndUserConflict):
				apierr.Write(w, protocol, 400, "invalid_request_error", "end-user headers must name one user")
			default:
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

// bearerToken returns the single bearer credential when it is not shaped
// like an API key and no X-Api-Key accompanies it.
func bearerToken(r *http.Request) string {
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 || len(r.Header.Values("X-Api-Key")) != 0 {
		return ""
	}
	parts := strings.Split(auth[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.HasPrefix(parts[1], "olr-") || len(parts[1]) > 16384 {
		return ""
	}
	return parts[1]
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

// Freshness reports the cache generation and whether cached principals may
// be used. Other caches of authorization data (such as API access tokens)
// store the generation they read and discard entries from older generations,
// so every invalidation published by an admin change reaches them as well.
func (a *Authenticator) Freshness() (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation, a.listening
}
