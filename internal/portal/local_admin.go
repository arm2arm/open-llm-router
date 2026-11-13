package portal

import (
	"errors"
	"net/http"
	"slices"

	"github.com/arm2arm/open-llm-router/internal/auth"
)

func (h *Handler) localGroups(w http.ResponseWriter, groups []string) ([]string, bool) {
	if len(groups) > 64 {
		fail(w, 400, "invalid_request_error", "choose at most 64 existing groups")
		return nil, false
	}
	names := slices.Clone(groups)
	if names == nil {
		names = []string{}
	}
	slices.Sort(names)
	for i, name := range names {
		if _, ok := h.routingConfig().Groups[name]; !ok || (i > 0 && names[i-1] == name) {
			fail(w, 400, "invalid_request_error", "group names must be distinct existing configured groups")
			return nil, false
		}
	}
	return names, true
}

func localAdminError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, auth.ErrProtectedAdmin):
		fail(w, 403, "permission_error", "the protected local administrator cannot be created, disabled, reassigned or administratively password-reset")
	case errors.Is(err, auth.ErrLocalIdentity):
		fail(w, 403, "permission_error", "Keycloak identities and memberships are managed by their identity provider")
	case errors.Is(err, auth.ErrNotFound):
		fail(w, 404, "not_found_error", "user or key not found")
	case errors.Is(err, auth.ErrLocalUserExists):
		fail(w, 409, "conflict_error", err.Error())
	case errors.Is(err, auth.ErrLocalUserInput), errors.Is(err, auth.ErrPasswordPolicy), errors.Is(err, auth.ErrPasswordReuse):
		fail(w, 400, "invalid_request_error", err.Error())
	default:
		unavailable(w)
	}
	return true
}

func (h *Handler) createLocalUser(w http.ResponseWriter, r *http.Request, by user) {
	var body struct {
		Username string   `json:"username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
		Password string   `json:"password"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	groups, ok := h.localGroups(w, body.Groups)
	if !ok {
		return
	}
	if !h.allowPassword(w, r, "admin:"+by.ID) {
		return
	}
	defer func() { <-h.passwordSlots }()
	password := []byte(body.Password)
	defer clear(password)
	id, err := (auth.Admin{Pool: h.pool}).CreateLocalUser(r.Context(), body.Username, body.Email, groups, password)
	if localAdminError(w, err) {
		return
	}
	respond(w, http.StatusCreated, map[string]any{"id": id, "must_change_password": true})
}

func (h *Handler) setLocalGroups(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct {
		Groups *[]string `json:"groups"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if body.Groups == nil {
		fail(w, 400, "invalid_request_error", "groups must be an array; use [] to remove all memberships")
		return
	}
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	groups, ok := h.localGroups(w, *body.Groups)
	if !ok {
		return
	}
	if localAdminError(w, (auth.Admin{Pool: h.pool}).SetLocalGroups(r.Context(), id, groups)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resetLocalPassword(w http.ResponseWriter, r *http.Request, by user) {
	var body struct {
		Password string `json:"password"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	if !h.allowPassword(w, r, "admin:"+by.ID) {
		return
	}
	defer func() { <-h.passwordSlots }()
	password := []byte(body.Password)
	defer clear(password)
	if localAdminError(w, (auth.Admin{Pool: h.pool}).ResetLocalPassword(r.Context(), id, password)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeUserKey(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	key, ok := pathUUID(w, r, "key", "key not found")
	if !ok {
		return
	}
	if localAdminError(w, (auth.Admin{Pool: h.pool}).RevokeOwnedKey(r.Context(), id, key)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Keep a fail-closed tombstone for clients that still know the old endpoint.
// There is no recovery implementation or plaintext credential in storage.
func (h *Handler) copyUserKey(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	fail(w, http.StatusGone, "key_recovery_removed", "API keys are shown only at creation; create a replacement and revoke the lost key")
}
