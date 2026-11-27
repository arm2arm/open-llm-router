package portal

import (
	"context"
	"errors"
	"net/http"

	"github.com/arm2arm/open-llm-router/internal/groupsync"
)

// GroupSync exposes the configured background job to portal administrators.
type GroupSync interface {
	GroupSyncEnabled() bool
	SyncGroups(context.Context) (groupsync.Result, error)
}

func (h *Handler) groupSyncEnabled() bool {
	return h.GroupSync != nil && h.GroupSync.GroupSyncEnabled()
}

func (h *Handler) syncGroups(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if !h.groupSyncEnabled() {
		fail(w, 409, "sync_disabled", "Configure and activate a Keycloak sync client and secret in Admin → Keycloak to enable group sync.")
		return
	}
	result, err := h.GroupSync.SyncGroups(r.Context())
	if errors.Is(err, groupsync.ErrDisabled) || errors.Is(err, groupsync.ErrUnavailable) {
		fail(w, 503, "sync_unavailable", "Keycloak group sync is unavailable. Check the active sync settings and connection checks, then try again.")
		return
	}
	problem := ""
	if err != nil {
		problem = "Group sync did not finish. Check the router logs and Keycloak sync permissions, then try again."
	}
	// A run can apply some completed reads before a timeout or individual
	// lookup failure. Include its counts rather than hiding partial progress.
	respond(w, 200, map[string]any{"result": result, "problem": problem})
}

// IdentityDirectory reads a user from the identity provider through the
// group-sync service account. A GroupSync may implement it.
type IdentityDirectory interface {
	LookupIdentity(ctx context.Context, issuer, subject string) (groupsync.Identity, error)
}
