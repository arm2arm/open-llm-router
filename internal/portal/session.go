package portal

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
)

type user struct {
	ID             string            `json:"id"`
	Username       string            `json:"username"`
	Email          string            `json:"email"`
	Groups         []string          `json:"groups"`
	Overrides      map[string]*int64 `json:"overrides"`
	ProtectedAdmin bool              `json:"protected_admin"`
}

func (h *Handler) authenticated(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash, ok := cookieHash(r, sessionCookie)
		if !ok {
			fail(w, 401, "authentication_error", "authentication required")
			return
		}
		var u user
		err := h.pool.QueryRow(r.Context(), `SELECT u.id,u.username,u.email,u.groups,u.protected_admin,
			(SELECT jsonb_object_agg(o.field,o.value) FROM quota_overrides o WHERE o.user_id=u.id)
			FROM sessions s JOIN users u ON u.id=s.user_id
			WHERE s.id_hash=$1 AND s.expires>clock_timestamp() AND u.enabled AND NOT EXISTS
			(SELECT 1 FROM local_credentials c WHERE c.user_id=u.id AND c.must_change_password)`, hash[:]).Scan(&u.ID, &u.Username, &u.Email, &u.Groups, &u.ProtectedAdmin, &u.Overrides)
		if errors.Is(err, pgx.ErrNoRows) {
			h.cookie(w, sessionCookie, "", -1)
			fail(w, 401, "authentication_error", "authentication required")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		if u.Groups == nil {
			u.Groups = []string{}
		}
		if u.Overrides == nil {
			u.Overrides = map[string]*int64{}
		}
		next(w, r, u)
	}
}

func (h *Handler) signout(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	hash, _ := cookieHash(r, sessionCookie)
	if _, err := h.pool.Exec(r.Context(), `DELETE FROM sessions WHERE id_hash=$1`, hash[:]); err != nil {
		unavailable(w)
		return
	}
	if hash, ok := cookieHash(r, loginCookie); ok {
		if _, err := h.pool.Exec(r.Context(), `DELETE FROM portal_login_attempts WHERE id_hash=$1`, hash[:]); err != nil {
			unavailable(w)
			return
		}
	}
	h.cookie(w, sessionCookie, "", -1)
	h.cookie(w, loginCookie, "", -1)
	u, _ := url.Parse(h.logout)
	q := u.Query()
	q.Set("client_id", h.oauth.ClientID)
	q.Set("post_logout_redirect_uri", h.cfg.Portal.BaseURL+"/portal/")
	u.RawQuery = q.Encode()
	respond(w, 200, map[string]string{"logout_url": u.String()})
}

func (h *Handler) establish(r *http.Request, id string) (string, error) {
	session := randomToken()
	hash := sha256.Sum256([]byte(session))
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		if old, ok := cookieHash(r, sessionCookie); ok {
			if _, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE id_hash=$1`, old[:]); err != nil {
				return err
			}
		}
		_, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE expires<=clock_timestamp()`)
		if err != nil {
			return err
		}
		result, err := tx.Exec(r.Context(), `INSERT INTO sessions(id_hash,user_id,expires) SELECT $1,id,clock_timestamp()+interval '8 hours' FROM users WHERE id=$2 AND enabled
			AND NOT EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=users.id AND c.must_change_password)`, hash[:], id)
		if err == nil && result.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return err
	})
	return session, err
}
