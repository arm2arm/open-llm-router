package portal

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type ssoProvider struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	idp      *oidcclient.Client
	logout   string
}

func (h *Handler) discoverSSO(ctx context.Context) *ssoProvider {
	if h.cfg.OIDC == nil || h.cfg.OIDCClientSecret == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	idp, err := oidcclient.New(ctx, h.cfg.OIDC)
	if err != nil {
		return nil
	}
	var metadata struct {
		Logout string `json:"end_session_endpoint"`
	}
	if idp.Provider.Claims(&metadata) != nil || !endpoint(metadata.Logout) {
		idp.Close()
		return nil
	}
	return &ssoProvider{idp: idp, logout: metadata.Logout,
		oauth: oauth2.Config{ClientID: h.cfg.OIDC.PortalClientID, ClientSecret: h.cfg.OIDCClientSecret,
			RedirectURL: h.cfg.Portal.BaseURL + "/portal/callback", Endpoint: idp.Provider.Endpoint(),
			Scopes: []string{oidc.ScopeOpenID, "profile", "email"}},
		verifier: idp.Provider.Verifier(&oidc.Config{ClientID: h.cfg.OIDC.PortalClientID})}
}

func (h *Handler) startSSO() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		defer func() {
			if p := h.sso.Load(); p != nil {
				p.idp.Close()
			}
		}()
		if h.cfg.OIDC == nil {
			return
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			provider := h.discoverSSO(ctx)
			if provider != nil {
				if h.sso.Load() == nil {
					h.sso.Store(provider)
				} else {
					provider.idp.Close()
				}
			}
			h.ssoAvailable.Store(provider != nil)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
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
	if !h.ssoAvailable.Load() {
		w.Header().Set("Retry-After", "10")
		fail(w, 503, "oidc_unavailable", "single sign-on is unavailable; local sign-in remains available")
		return nil
	}
	return h.sso.Load()
}

func (h *Handler) authMethods(w http.ResponseWriter, _ *http.Request) {
	primary := "local"
	if h.cfg.OIDC != nil && h.cfg.OIDC.DefaultSignIn != "local" {
		primary = "oidc"
	}
	respond(w, 200, map[string]any{
		"default": primary,
		"local":   map[string]any{"available": h.safeCookies(), "login_url": "/portal/login/local", "password_url": "/portal/password"},
		"oidc":    map[string]any{"configured": h.cfg.OIDC != nil, "available": h.safeCookies() && h.ssoAvailable.Load(), "login_url": "/portal/sign-in/keycloak"},
	})
}
