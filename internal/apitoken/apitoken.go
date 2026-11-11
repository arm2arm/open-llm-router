// Package apitoken accepts identity-provider access tokens on /v1/*, for
// services that authenticate with client credentials instead of an API key.
package apitoken

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cacheTTL bounds how long a verified token's user stays cached; a user
// disabled in the router is refused within it.
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
}

type entry struct {
	principal auth.Principal
	until     time.Time
}

// New discovers the provider. Close releases its connections.
func New(ctx context.Context, cfg *config.OIDC, pool *pgxpool.Pool) (*Verifier, error) {
	idp, err := oidcclient.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Verifier{cfg: cfg, idp: idp, admin: auth.Admin{Pool: pool}, cache: map[[32]byte]entry{}, now: time.Now,
		verifier: idp.Provider.Verifier(&oidc.Config{ClientID: cfg.Audience})}, nil
}

func (v *Verifier) Close() { v.idp.Close() }

// Authenticate returns the principal of a valid access token, or
// auth.ErrInvalidKey.
func (v *Verifier) Authenticate(ctx context.Context, raw string) (auth.Principal, error) {
	hash := sha256.Sum256([]byte(raw))
	v.mu.Lock()
	cached, ok := v.cache[hash]
	v.mu.Unlock()
	if ok && v.now().Before(cached.until) {
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
	if groups == nil {
		groups = []string{}
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	// The token's groups are current; store them like a portal login does.
	id, err := v.admin.EnsureIdentity(ctx, token.Issuer, token.Subject, username, email, groups)
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
	v.mu.Lock()
	if len(v.cache) >= maxCached {
		v.cache = map[[32]byte]entry{}
	}
	v.cache[hash] = entry{principal: p, until: until}
	v.mu.Unlock()
	return p, nil
}
