package portal

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Password-enabled and API-key-only accounts have the same administrative
// authority. Disabled accounts must be checked too: enabling one restores it.
func (h *Handler) targetAdministrator(w http.ResponseWriter, r *http.Request, id string) (bool, bool) {
	var target user
	err := h.pool.QueryRow(r.Context(), `SELECT groups,protected_admin FROM users WHERE id=$1`, id).Scan(&target.Groups, &target.ProtectedAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found_error", "user not found")
		return false, false
	}
	if err != nil {
		unavailable(w)
		return false, false
	}
	return h.access(target).Admin, true
}

// SSO administrators cannot manufacture a fresh local administrator session.
// An agent instead needs explicit delegation, granted by a recent local login.
func allowAdministratorAuthority(w http.ResponseWriter, r *http.Request, by user, administrator bool) bool {
	if !administrator {
		return true
	}
	if agent, ok := r.Context().Value(agentContext{}).(agentIdentity); ok {
		if !agent.Key.Allows("admin_keys:delegate") {
			fail(w, 403, "permission_error", "an administrator target requires admin_keys:delegate")
			return false
		}
		return true
	}
	if !recentLocal(by) {
		fail(w, 403, "reauthentication_required", "sign in again with a local administrator account (within the last 15 minutes) to grant or reset local administrator access")
		return false
	}
	return true
}

// Preview and apply enforce the same target-sensitive requirement as the
// mutation handlers. Ordinary helpdesk resets/enablement need no delegation.
func (h *Handler) authorizeAgentTarget(w http.ResponseWriter, r *http.Request, a agentIdentity, op *agentOperation, args agentArguments) bool {
	if op.AdministratorTargetPermission == "" {
		return true
	}
	if op.Name == "set_user_enabled" {
		var body struct{ Enabled bool }
		if json.Unmarshal(args.Body, &body) != nil || !body.Enabled {
			return true
		}
	}
	var id pgtype.UUID
	if id.Scan(args.Parameters["id"]) != nil || !id.Valid {
		fail(w, 404, "not_found_error", "user not found")
		return false
	}
	administrator, ok := h.targetAdministrator(w, r, id.String())
	if !ok || !allowAdministratorAuthority(w, r, a.User, administrator) {
		return false
	}
	if administrator {
		op.Additional = append(op.Additional, op.AdministratorTargetPermission)
	}
	return true
}
