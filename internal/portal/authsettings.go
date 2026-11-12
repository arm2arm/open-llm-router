package portal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/idpcheck"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// Changing how people sign in requires a fresh local sign-in: an SSO session
// cannot reconfigure its own identity provider, and an old session cannot
// redirect logins elsewhere.
const recentLocalSignIn = 15 * time.Minute
const testLoginCookie = "olr_test_login"

func portalActor(u user) audit.Actor {
	return audit.Actor{Type: "portal_user", ID: u.ID, Name: u.Username}
}

func recentLocal(u user) bool {
	return u.AuthMethod == "local" && time.Since(u.SessionCreated) <= recentLocalSignIn
}

// settingsAdmin guards changes, checks and test sign-ins.
func (h *Handler) settingsAdmin(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return h.admin(func(w http.ResponseWriter, r *http.Request, u user) {
		if h.Settings == nil {
			unavailable(w)
			return
		}
		if !recentLocal(u) {
			fail(w, 403, "reauthentication_required", "sign in again with a local administrator account (within the last 15 minutes) to change authentication settings")
			return
		}
		next(w, r, u)
	})
}

func settingsError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var invalid *settings.ValidationError
	switch {
	case errors.As(err, &invalid):
		fail(w, 400, "invalid_request_error", err.Error())
	case errors.Is(err, settings.ErrNoCipher):
		fail(w, 400, "invalid_request_error", err.Error())
	case errors.Is(err, settings.ErrConflict), errors.Is(err, settings.ErrManaged), errors.Is(err, settings.ErrNotManaged), errors.Is(err, settings.ErrNoDraft):
		fail(w, 409, "conflict_error", err.Error())
	case errors.Is(err, settings.ErrRevision), errors.Is(err, settings.ErrUnknownDomain):
		fail(w, 404, "not_found_error", err.Error())
	default:
		unavailable(w)
	}
	return true
}

type authenticationView struct {
	Settings settings.View `json:"settings"`
	// URLs to register on the identity provider's portal client.
	URLs struct {
		Redirect   string `json:"redirect_uri"`
		PostLogout string `json:"post_logout_redirect_uri"`
		WebOrigin  string `json:"web_origin"`
	} `json:"urls"`
	// Runtime is what this router process is running, which can differ from
	// the active revision until it restarts.
	Runtime struct {
		Revision      int64  `json:"revision"`
		Problem       string `json:"problem,omitempty"`
		SSOConfigured bool   `json:"sso_configured"`
		SSOAvailable  bool   `json:"sso_available"`
	} `json:"runtime"`
	CanChange          bool   `json:"can_change"`
	ChangeRequirement  string `json:"change_requirement,omitempty"`
	EncryptionKeyReady bool   `json:"encryption_key_configured"`
}

func (h *Handler) authenticationView(r *http.Request, u user) (authenticationView, error) {
	var out authenticationView
	view, err := h.Settings.View(r.Context(), settings.DomainAuthentication)
	if err != nil {
		return out, err
	}
	out.Settings = view
	out.URLs.Redirect, out.URLs.PostLogout, out.URLs.WebOrigin = h.cfg.Portal.BaseURL+"/portal/callback", h.cfg.Portal.BaseURL+"/", h.cfg.Portal.BaseURL
	out.Runtime.Revision, out.Runtime.Problem = h.SettingsRuntime.Revision, h.SettingsRuntime.Problem
	out.Runtime.SSOConfigured, out.Runtime.SSOAvailable = h.cfg.OIDC != nil, h.ssoAvailable.Load()
	out.CanChange = recentLocal(u)
	if !out.CanChange {
		out.ChangeRequirement = "Changes need a local administrator sign-in within the last 15 minutes."
	}
	out.EncryptionKeyReady = h.Settings.Cipher != nil
	return out, nil
}

func (h *Handler) authenticationSettings(w http.ResponseWriter, r *http.Request, u user) {
	if h.Settings == nil {
		unavailable(w)
		return
	}
	view, err := h.authenticationView(r, u)
	if settingsError(w, err) {
		return
	}
	respond(w, 200, view)
}

func (h *Handler) respondAuthentication(w http.ResponseWriter, r *http.Request, u user, err error) {
	if settingsError(w, err) {
		return
	}
	h.authenticationSettings(w, r, u)
}

func (h *Handler) importAuthentication(w http.ResponseWriter, r *http.Request, u user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	_, err := h.Settings.Import(r.Context(), settings.DomainAuthentication, portalActor(u))
	h.respondAuthentication(w, r, u, err)
}

func (h *Handler) saveAuthenticationDraft(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Version  int64             `json:"version"`
		Document json.RawMessage   `json:"document"`
		Secrets  map[string]string `json:"secrets"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if body.Version <= 0 || len(body.Document) == 0 {
		fail(w, 400, "invalid_request_error", "version and document are required")
		return
	}
	secrets := map[string][]byte{}
	for name, value := range body.Secrets {
		if value != "" {
			secrets[name] = []byte(value)
		}
	}
	_, err := h.Settings.SaveDraft(r.Context(), settings.DomainAuthentication, body.Version, body.Document, secrets, portalActor(u))
	for _, value := range secrets {
		clear(value)
	}
	h.respondAuthentication(w, r, u, err)
}

func (h *Handler) discardAuthenticationDraft(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Version int64 `json:"version"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	_, err := h.Settings.Discard(r.Context(), settings.DomainAuthentication, body.Version, portalActor(u))
	h.respondAuthentication(w, r, u, err)
}

// checkAuthentication runs connection checks for a revision (0: the current
// configuration). The optional test service client is used once and not kept.
func (h *Handler) checkAuthentication(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Revision         int64  `json:"revision"`
		TestClientID     string `json:"test_client_id"`
		TestClientSecret string `json:"test_client_secret"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if body.Revision < 0 || len(body.TestClientID) > 256 || len(body.TestClientSecret) > 4096 {
		fail(w, 400, "invalid_request_error", "revision must be 0 or a revision ID; the test client is limited in size")
		return
	}
	cfg, revision, err := h.Settings.Candidate(r.Context(), settings.DomainAuthentication, body.Revision)
	if settingsError(w, err) {
		return
	}
	results := idpcheck.Run(r.Context(), idpcheck.Input{OIDC: cfg.OIDC, PortalSecret: cfg.OIDCClientSecret, SyncSecret: cfg.OIDCSyncSecret,
		TestClientID: body.TestClientID, TestClientSecret: body.TestClientSecret})
	if settingsError(w, h.Settings.RecordChecks(r.Context(), settings.DomainAuthentication, revision, results, nil, portalActor(u))) {
		return
	}
	view, err := h.authenticationView(r, u)
	if settingsError(w, err) {
		return
	}
	respond(w, 200, map[string]any{"revision": revision, "results": results, "view": view})
}

// startTestSignIn begins an authorization-code login with a revision's
// settings. The callback verifies the identity token and claims, records the
// result on the revision and creates no user or session.
func (h *Handler) startTestSignIn(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	cfg, revision, err := h.Settings.Candidate(r.Context(), settings.DomainAuthentication, body.Revision)
	if settingsError(w, err) {
		return
	}
	switch {
	case revision == 0:
		fail(w, 409, "conflict_error", "import the settings before testing sign-in")
		return
	case cfg.OIDC == nil:
		fail(w, 400, "invalid_request_error", "single sign-on is disabled in this revision")
		return
	case cfg.OIDCClientSecret == "":
		fail(w, 400, "invalid_request_error", "this revision has no portal client secret")
		return
	}
	base, err := url.Parse(h.cfg.Portal.BaseURL)
	if err != nil || !strings.EqualFold(r.Host, base.Host) {
		fail(w, 400, "invalid_request_error", "open the portal at its public origin to test sign-in")
		return
	}
	idp, err := idpcheck.Connect(r.Context(), cfg.OIDC)
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	defer idp.Close()
	oauth := oauth2.Config{ClientID: cfg.OIDC.PortalClientID, ClientSecret: cfg.OIDCClientSecret, RedirectURL: h.cfg.Portal.BaseURL + "/portal/callback",
		Endpoint: idp.Provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"}}
	state, nonce, browser, verifier := randomToken(), randomToken(), randomToken(), oauth2.GenerateVerifier()
	idHash, stateHash := sha256.Sum256([]byte(browser)), sha256.Sum256([]byte(state))
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM portal_login_attempts WHERE expires<=clock_timestamp() OR test_user=$1`, u.ID); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO portal_login_attempts(id_hash,state_hash,nonce,verifier,expires,test_revision,test_user)
			VALUES($1,$2,$3,$4,clock_timestamp()+interval '10 minutes',$5,$6)`, idHash[:], stateHash[:], nonce, verifier, revision, u.ID)
		return err
	})
	if err != nil {
		unavailable(w)
		return
	}
	h.cookie(w, testLoginCookie, browser, 10*time.Minute)
	respond(w, 200, map[string]string{"url": oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))})
}

var providerError = regexp.MustCompile(`^[a-z_]{1,64}$`)

// testCallback finishes a test sign-in if the callback belongs to one; it
// reports false for an ordinary login.
func (h *Handler) testCallback(w http.ResponseWriter, r *http.Request) bool {
	idHash, ok := cookieHash(r, testLoginCookie)
	if !ok {
		return false
	}
	query := r.URL.Query()
	stateHash := sha256.Sum256([]byte(query.Get("state")))
	var nonce, verifier, testUser string
	var revision int64
	err := h.pool.QueryRow(r.Context(), `DELETE FROM portal_login_attempts WHERE id_hash=$1 AND state_hash=$2 AND test_revision IS NOT NULL
		AND expires>clock_timestamp() RETURNING nonce,verifier,test_revision,test_user`, idHash[:], stateHash[:]).Scan(&nonce, &verifier, &revision, &testUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return false // not this test attempt; continue as an ordinary login
	}
	h.cookie(w, testLoginCookie, "", -1)
	if err != nil {
		unavailable(w)
		return true
	}
	// Only the administrator who started the test, still freshly signed in
	// locally, can complete it.
	u, err := h.session(r)
	if err != nil || u.ID != testUser || !h.access(u).Admin || !recentLocal(u) || h.Settings == nil {
		fail(w, 403, "permission_error", "the test sign-in must be completed by the administrator who started it")
		return true
	}
	result := h.verifyTestSignIn(r, revision, nonce, verifier)
	if settingsError(w, h.Settings.RecordChecks(r.Context(), settings.DomainAuthentication, revision, nil, &result, portalActor(u))) {
		return true
	}
	http.Redirect(w, r, "/portal/admin?authentication_test="+result.Status+"#authentication", http.StatusSeeOther)
	return true
}

func (h *Handler) verifyTestSignIn(r *http.Request, revision int64, nonce, verifier string) idpcheck.Result {
	failed := func(message string) idpcheck.Result {
		return idpcheck.Result{Name: idpcheck.TestSignIn, Status: idpcheck.Failed, Message: message}
	}
	query := r.URL.Query()
	if code := query.Get("error"); code != "" {
		if !providerError.MatchString(code) {
			code = "unknown"
		}
		return failed("the identity provider returned the error " + code)
	}
	if len(query["code"]) != 1 || query.Get("code") == "" {
		return failed("the identity provider returned no authorization code")
	}
	cfg, _, err := h.Settings.Candidate(r.Context(), settings.DomainAuthentication, revision)
	if err != nil || cfg.OIDC == nil {
		return failed("the revision under test can no longer be applied")
	}
	idp, err := idpcheck.Connect(r.Context(), cfg.OIDC)
	if err != nil {
		return failed(err.Error())
	}
	defer idp.Close()
	oauth := oauth2.Config{ClientID: cfg.OIDC.PortalClientID, ClientSecret: cfg.OIDCClientSecret, RedirectURL: h.cfg.Portal.BaseURL + "/portal/callback",
		Endpoint: idp.Provider.Endpoint()}
	ctx := idp.Context(r.Context())
	token, err := oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		return failed("the authorization code exchange failed; check the portal client secret and the registered redirect URI")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return failed("the identity provider returned no identity token")
	}
	id, err := idp.Provider.Verifier(&oidc.Config{ClientID: cfg.OIDC.PortalClientID}).Verify(ctx, raw)
	if err != nil || id.Subject == "" || id.Issuer != cfg.OIDC.Issuer || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return failed("the identity token could not be verified for this issuer and portal client")
	}
	var claims map[string]json.RawMessage
	if id.Claims(&claims) != nil {
		return failed("the identity token claims could not be read")
	}
	var username string
	if json.Unmarshal(claims[cfg.OIDC.UsernameClaim], &username) != nil || username == "" {
		return failed(fmt.Sprintf("the identity token has no %s claim; add a mapper or choose another username claim", cfg.OIDC.UsernameClaim))
	}
	var groups []string
	if raw, present := claims[cfg.OIDC.GroupsClaim]; !present || json.Unmarshal(raw, &groups) != nil {
		return failed(fmt.Sprintf("the identity token has no %s claim with a list of group names; add a group membership mapper", cfg.OIDC.GroupsClaim))
	}
	if !hasRequiredClaim(claims, cfg.OIDC.RequiredClaim) {
		return failed(fmt.Sprintf("this user lacks the required %s claim value; the router would refuse the sign-in", cfg.OIDC.RequiredClaim.Name))
	}
	known := 0
	for _, group := range groups {
		if _, ok := h.cfg.Groups[group]; ok {
			known++
		}
	}
	return idpcheck.Result{Name: idpcheck.TestSignIn, Status: idpcheck.Passed,
		Message: fmt.Sprintf("signed in with a verified identity token: %s present, %d groups (%d configured here)", cfg.OIDC.UsernameClaim, len(groups), known)}
}
