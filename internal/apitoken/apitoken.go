// Package apitoken accepts identity-provider access tokens on /v1/*, for
// services that authenticate with client credentials instead of an API key.
package apitoken

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheTTL bounds how long a verified token's principal stays cached. Admin
// changes (disablement, overrides, memberships) invalidate it earlier: entries
// carry the authenticator's cache generation and older generations are
// discarded, so the next request observes the change.
const cacheTTL = time.Minute
const maxCached = 4096

// Verifier checks a token's signature against the provider's JWKS (fetched
// again when a token names an unknown key), its issuer, its audience and its
// expiry, then maps it to the router user (issuer, sub), created on first use.
type Verifier struct {
	cfg      *config.OIDC
	verifier *oidc.IDTokenVerifier
	idp      *oidcclient.Client
	admin    auth.Admin
	mu       sync.Mutex
	cache    map[[32]byte]entry
	now      func() time.Time
	// fresh reports the invalidation generation and whether caching is safe
	// (the invalidation listener is connected).
	fresh func() (uint64, bool)
}

type entry struct {
	principal  auth.Principal
	until      time.Time
	generation uint64
}

// New discovers the provider. fresh is normally Authenticator.Freshness;
// nil disables caching. Close releases its connections.
func New(ctx context.Context, cfg *config.OIDC, pool *pgxpool.Pool, fresh func() (uint64, bool)) (*Verifier, error) {
	idp, err := oidcclient.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		fresh = func() (uint64, bool) { return 0, false }
	}
	return &Verifier{cfg: cfg, idp: idp, admin: auth.Admin{Pool: pool}, cache: map[[32]byte]entry{}, now: time.Now, fresh: fresh,
		verifier: idp.Provider.Verifier(&oidc.Config{ClientID: cfg.Audience})}, nil
}

func (v *Verifier) Close() { v.idp.Close() }

// Authenticate returns the principal of a valid access token, or
// auth.ErrInvalidKey.
func (v *Verifier) Authenticate(ctx context.Context, raw string) (auth.Principal, error) {
	hash := sha256.Sum256([]byte(raw))
	// Read the generation before any database lookup: an invalidation that
	// races with this request makes the stored entry stale at once.
	generation, cacheable := v.fresh()
	v.mu.Lock()
	cached, ok := v.cache[hash]
	v.mu.Unlock()
	if ok && cacheable && cached.generation == generation && v.now().Before(cached.until) {
		return cached.principal, nil
	}
	token, err := v.verifier.Verify(v.idp.Context(ctx), raw)
	if err != nil || token.Subject == "" || token.Issuer != v.cfg.Issuer {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	var claims map[string]json.RawMessage
	if token.Claims(&claims) != nil {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	var username, email string
	var groups []string
	decode := func(name string, value any) bool {
		raw, ok := claims[name]
		return !ok || json.Unmarshal(raw, value) == nil
	}
	if !decode(v.cfg.UsernameClaim, &username) || !decode("email", &email) || !decode(v.cfg.GroupsClaim, &groups) {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	if username == "" {
		username = token.Subject
	}
	groups = auth.GroupNames(groups, v.cfg.GroupNames)
	id, groups, err := v.admin.TokenIdentity(ctx, token.Issuer, token.Subject, username, email, groups, token.IssuedAt)
	if errors.Is(err, auth.ErrNotFound) {
		return auth.Principal{}, auth.ErrInvalidKey // disabled in the router
	}
	if err != nil {
		return auth.Principal{}, errors.New("token user lookup failed")
	}
	overrides, err := v.admin.ListOverrides(ctx, id)
	if err != nil {
		return auth.Principal{}, errors.New("token user lookup failed")
	}
	p := auth.Principal{UserID: id, Username: username, Groups: groups, KeyClass: "interactive", Overrides: map[string]*int64{}}
	for _, o := range overrides {
		p.Overrides[o.Field] = o.Value
	}
	until := v.now().Add(cacheTTL)
	if token.Expiry.Before(until) {
		until = token.Expiry
	}
	if !cacheable {
		return p, nil
	}
	v.mu.Lock()
	if len(v.cache) >= maxCached {
		v.cache = map[[32]byte]entry{}
	}
	v.cache[hash] = entry{principal: p, until: until, generation: generation}
	v.mu.Unlock()
	return p, nil
}
