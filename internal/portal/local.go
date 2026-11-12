package portal

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/jackc/pgx/v5"
)

func invalidCredentials(w http.ResponseWriter) {
	fail(w, 401, "authentication_error", "invalid local username or password")
}

func (h *Handler) localLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if !h.safeCookies() {
		fail(w, 503, "authentication_unavailable", "portal authentication requires a secure public origin")
		return
	}
	base, _ := url.Parse(h.cfg.Portal.BaseURL)
	if !strings.EqualFold(r.Host, base.Host) {
		fail(w, 403, "permission_error", "request host is forbidden")
		return
	}
	if !h.allowPassword(w, r, body.Username) {
		return
	}
	defer func() { <-h.passwordSlots }()
	if len(body.Username) > 256 {
		invalidCredentials(w)
		return
	}
	password := []byte(body.Password)
	defer clear(password)
	account, err := (auth.Admin{Pool: h.pool}).VerifyLocalPassword(r.Context(), body.Username, password)
	if errors.Is(err, auth.ErrInvalidPassword) {
		invalidCredentials(w)
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	session, lifetime, err := h.establishLocal(r, account)
	if errors.Is(err, pgx.ErrNoRows) {
		invalidCredentials(w)
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	h.cookie(w, sessionCookie, session, lifetime)
	// Drop an in-progress SSO challenge when choosing local authentication.
	h.cookie(w, loginCookie, "", -1)
	respond(w, 200, map[string]any{"must_change_password": account.MustChangePassword})
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if u.AuthMethod != "local" {
		fail(w, 403, "permission_error", "a local session is required")
		return
	}
	if !h.allowPassword(w, r, u.Username) {
		return
	}
	defer func() { <-h.passwordSlots }()
	current, next := []byte(body.Current), []byte(body.Next)
	defer clear(current)
	defer clear(next)
	err := (auth.Admin{Pool: h.pool}).ChangeLocalPassword(r.Context(), u.ID, current, next)
	if errors.Is(err, auth.ErrInvalidPassword) {
		invalidCredentials(w)
		return
	}
	if errors.Is(err, auth.ErrPasswordPolicy) || errors.Is(err, auth.ErrPasswordReuse) {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	// ChangeLocalPassword revokes all sessions. Require a fresh login with the
	// replacement password; never upgrade a restricted cookie in place.
	h.cookie(w, sessionCookie, "", -1)
	h.cookie(w, loginCookie, "", -1)
	respond(w, 200, map[string]bool{"login_required": true})
}
