package portal

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type ssoProvider struct {
	cfg      *config.OIDC // the configuration this provider was built for
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	idp      *oidcclient.Client
	logout   string
}

// ssoState is one immutable SSO configuration with its discovery loop. A
// new state replaces the whole value; nothing in it is modified in place.
type ssoState struct {
	oidc      *config.OIDC // nil: local sign-in only
	secret    string
	provider  atomic.Pointer[ssoProvider]
	available atomic.Bool
	cancel    context.CancelFunc
	done      chan struct{}
}

func (s *ssoState) issuer() string {
	if s == nil || s.oidc == nil {
		return ""
	}
	return s.oidc.Issuer
}

// current returns the SSO configuration in use. It is never nil once New
// has returned.
func (h *Handler) current() *ssoState { return h.ssoState.Load() }

func discoverSSO(ctx context.Context, cfg *config.OIDC, secret, base string, timeout time.Duration) (*ssoProvider, error) {
	if cfg == nil || secret == "" {
		return nil, errors.New("single sign-on needs an issuer and a portal client secret")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	idp, err := oidcclient.New(ctx, cfg)
	if err != nil {
		return nil, errors.New("the identity provider's discovery document could not be read")
	}
	var metadata struct {
		Logout string `json:"end_session_endpoint"`
	}
	if idp.Provider.Claims(&metadata) != nil || !endpoint(metadata.Logout) {
		idp.Close()
		return nil, errors.New("the identity provider publishes no end-session endpoint")
	}
	return &ssoProvider{cfg: cfg, idp: idp, logout: metadata.Logout,
		oauth: oauth2.Config{ClientID: cfg.PortalClientID, ClientSecret: secret,
			RedirectURL: base + "/portal/callback", Endpoint: idp.Provider.Endpoint(),
			Scopes: []string{oidc.ScopeOpenID, "profile", "email"}},
		verifier: idp.Provider.Verifier(&oidc.Config{ClientID: cfg.PortalClientID})}, nil
}

// startSSO runs discovery for a configuration until stopped. An initial
// provider (from PrepareSSO) is used at once; the loop then only refreshes
// availability. The provider is closed when the state stops.
func (h *Handler) startSSO(cfg *config.OIDC, secret string, initial *ssoProvider) *ssoState {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ssoState{oidc: cfg, secret: secret, cancel: cancel, done: make(chan struct{})}
	if initial != nil {
		s.provider.Store(initial)
		s.available.Store(true)
	}
	go func() {
		defer close(s.done)
		defer func() {
			if p := s.provider.Load(); p != nil {
				p.idp.Close()
			}
		}()
		if cfg == nil {
			return
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for first := initial == nil; ; first = false {
			if !first {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
			provider, _ := discoverSSO(ctx, cfg, secret, h.cfg.Portal.BaseURL, time.Second)
			if provider != nil {
				if s.provider.Load() == nil {
					s.provider.Store(provider)
				} else {
					provider.idp.Close()
				}
			}
			s.available.Store(provider != nil)
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return s
}

func (s *ssoState) stop() {
	s.cancel()
	<-s.done
}

// PreparedSSO is a validated replacement SSO configuration, not yet in use.
type PreparedSSO struct {
	oidc     *config.OIDC
	secret   string
	provider *ssoProvider
}

// Close releases a prepared configuration that was not committed.
func (p *PreparedSSO) Close() {
	if p != nil && p.provider != nil {
		p.provider.idp.Close()
	}
}

// PrepareSSO builds the portal's SSO provider for cfg before anything
// changes. strict requires the provider to be reachable now; otherwise an
// unreachable provider is retried in the background after commit (rollback
// must work while the previous provider is down).
func (h *Handler) PrepareSSO(ctx context.Context, cfg *config.Config, strict bool) (*PreparedSSO, error) {
	prepared := &PreparedSSO{oidc: cfg.OIDC, secret: cfg.OIDCClientSecret}
	if cfg.OIDC == nil {
		return prepared, nil
	}
	provider, err := discoverSSO(ctx, cfg.OIDC, cfg.OIDCClientSecret, h.cfg.Portal.BaseURL, 10*time.Second)
	if err != nil && strict {
		return nil, err
	}
	prepared.provider = provider
	return prepared, nil
}

// CommitSSO switches the portal to a prepared configuration and stops the
// previous one. When the issuer changes, pending SSO logins of the previous
// issuer are discarded; its sessions stop authenticating because sessions
// are checked against the current issuer.
func (h *Handler) CommitSSO(ctx context.Context, prepared *PreparedSSO) {
	next := h.startSSO(prepared.oidc, prepared.secret, prepared.provider)
	previous := h.ssoState.Swap(next)
	if previous.issuer() != next.issuer() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM portal_login_attempts WHERE test_revision IS NULL`)
		_, _ = h.pool.Exec(ctx, `DELETE FROM sessions s USING users u WHERE s.user_id=u.id AND s.auth_method='oidc' AND u.issuer<>$1`, next.issuer())
	}
	previous.stop()
}

func (h *Handler) safeCookies() bool {
	// Keep API-key installations running even when their default HTTP origin
	// cannot issue portal cookies. Never silently weaken the Secure flag.
	return !h.cfg.Portal.CookieSecure || strings.HasPrefix(h.cfg.Portal.BaseURL, "https://")
}

func (h *Handler) requireSSO(w http.ResponseWriter) *ssoProvider {
	if !h.safeCookies() {
		fail(w, 503, "authentication_unavailable", "portal authentication requires a secure public origin")
		return nil
	}
	state := h.current()
	provider := state.provider.Load()
	if !state.available.Load() || provider == nil {
		w.Header().Set("Retry-After", "10")
		fail(w, 503, "oidc_unavailable", "single sign-on is unavailable; local sign-in remains available")
		return nil
	}
	return provider
}

func (h *Handler) authMethods(w http.ResponseWriter, _ *http.Request) {
	state := h.current()
	primary := "local"
	if state.oidc != nil && state.oidc.DefaultSignIn != "local" {
		primary = "oidc"
	}
	respond(w, 200, map[string]any{
		"default": primary,
		"local":   map[string]any{"available": h.safeCookies(), "login_url": "/portal/login/local", "password_url": "/portal/password"},
		"oidc":    map[string]any{"configured": state.oidc != nil, "available": h.safeCookies() && state.available.Load(), "login_url": "/portal/sign-in/keycloak"},
	})
}
