package portal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

// MaxPendingLogins caps unfinished login attempts; each expires after ten minutes.
var MaxPendingLogins = 10000

var errTooManyLogins = errors.New("too many pending logins")

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	state, nonce, browser, verifier := randomToken(), randomToken(), randomToken(), oauth2.GenerateVerifier()
	idHash, stateHash := sha256.Sum256([]byte(browser)), sha256.Sum256([]byte(state))
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM portal_login_attempts WHERE expires<=clock_timestamp()`); err != nil {
			return err
		}
		if old, ok := cookieHash(r, loginCookie); ok {
			if _, err := tx.Exec(r.Context(), `DELETE FROM portal_login_attempts WHERE id_hash=$1`, old[:]); err != nil {
				return err
			}
		}
		// The login route is public: bound the rows it can create. The count
		// is not serialized across concurrent logins, so the cap is approximate.
		result, err := tx.Exec(r.Context(), `INSERT INTO portal_login_attempts(id_hash,state_hash,nonce,verifier,expires)
			SELECT $1,$2,$3,$4,clock_timestamp()+interval '10 minutes'
			WHERE (SELECT count(*) FROM portal_login_attempts) < $5`, idHash[:], stateHash[:], nonce, verifier, MaxPendingLogins)
		if err == nil && result.RowsAffected() == 0 {
			return errTooManyLogins
		}
		return err
	})
	if errors.Is(err, errTooManyLogins) {
		w.Header().Set("Retry-After", "60")
		fail(w, 503, "overloaded_error", "too many logins in progress; try again shortly")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	h.cookie(w, loginCookie, browser, 10*time.Minute)
	http.Redirect(w, r, h.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusSeeOther)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	idHash, ok := cookieHash(r, loginCookie)
	query := r.URL.Query()
	if !ok || len(query["state"]) != 1 || query.Get("state") == "" || len(query["code"]) != 1 || query.Get("code") == "" || query.Has("error") {
		fail(w, 400, "invalid_request_error", "invalid login callback")
		return
	}
	stateHash := sha256.Sum256([]byte(query.Get("state")))
	var nonce, verifier string
	err := h.pool.QueryRow(r.Context(), `DELETE FROM portal_login_attempts WHERE id_hash=$1 AND state_hash=$2 AND expires>clock_timestamp() RETURNING nonce,verifier`, idHash[:], stateHash[:]).Scan(&nonce, &verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 400, "invalid_request_error", "invalid or expired login state")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	h.cookie(w, loginCookie, "", -1)
	ctx := oidc.ClientContext(r.Context(), h.client)
	token, err := h.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		var rejected *oauth2.RetrieveError
		if errors.As(err, &rejected) && rejected.Response.StatusCode >= 400 && rejected.Response.StatusCode < 500 {
			fail(w, 400, "invalid_request_error", "authorization code exchange failed")
		} else {
			unavailable(w)
		}
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		fail(w, 400, "invalid_request_error", "identity token is missing")
		return
	}
	id, err := h.verifier.Verify(ctx, raw)
	if err != nil || id.Subject == "" || id.Issuer != h.cfg.OIDC.Issuer || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		fail(w, 400, "invalid_request_error", "identity token validation failed")
		return
	}
	if id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil {
		fail(w, 400, "invalid_request_error", "identity token validation failed")
		return
	}
	var claims map[string]json.RawMessage
	if id.Claims(&claims) != nil {
		fail(w, 400, "invalid_request_error", "invalid identity claims")
		return
	}
	var username, email, party string
	var groups []string
	decode := func(name string, value any) bool {
		raw, ok := claims[name]
		return !ok || json.Unmarshal(raw, value) == nil
	}
	if !decode(h.cfg.OIDC.UsernameClaim, &username) || !decode("email", &email) || !decode(h.cfg.OIDC.GroupsClaim, &groups) || !decode("azp", &party) || (party != "" && party != h.oauth.ClientID) || (len(id.Audience) > 1 && party != h.oauth.ClientID) {
		fail(w, 400, "invalid_request_error", "invalid identity claims")
		return
	}
	if username == "" {
		username = id.Subject
	}
	if groups == nil {
		groups = []string{}
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	userID, err := (&auth.Admin{Pool: h.pool}).SyncIdentity(r.Context(), id.Issuer, id.Subject, username, email, groups)
	if errors.Is(err, auth.ErrNotFound) {
		fail(w, 403, "permission_error", "user is disabled")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	session, err := h.establish(r, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 403, "permission_error", "user is disabled")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	h.cookie(w, sessionCookie, session, sessionLifetime)
	http.Redirect(w, r, "/portal/", http.StatusSeeOther)
}
