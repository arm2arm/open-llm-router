package portal

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/jackc/pgx/v5"
)

type user struct {
	ID                     string            `json:"id"`
	Username               string            `json:"username"`
	Email                  string            `json:"email"`
	Groups                 []string          `json:"groups"`
	Overrides              map[string]*int64 `json:"overrides"`
	ProtectedAdmin         bool              `json:"protected_admin"`
	AuthMethod             string            `json:"auth_method"`
	PasswordChangeRequired bool              `json:"must_change_password"`
}

func (h *Handler) authenticated(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return h.withSession(false, next)
}

func (h *Handler) session(r *http.Request) (user, error) {
	var u user
	hash, ok := cookieHash(r, sessionCookie)
	if !ok {
		return u, pgx.ErrNoRows
	}
	err := h.pool.QueryRow(r.Context(), `SELECT u.id,u.username,u.email,u.groups,u.protected_admin,
		s.auth_method,(s.scope='password_change' OR COALESCE(c.must_change_password,false)),
		(SELECT jsonb_object_agg(o.field,o.value) FROM quota_overrides o WHERE o.user_id=u.id)
		FROM sessions s JOIN users u ON u.id=s.user_id
		LEFT JOIN local_credentials c ON c.user_id=u.id
		WHERE s.id_hash=$1 AND s.expires>clock_timestamp() AND u.enabled AND
		((s.auth_method='oidc' AND u.issuer<>'local') OR
		(s.auth_method='local' AND u.issuer='local' AND c.revision=s.credential_revision))`, hash[:]).
		Scan(&u.ID, &u.Username, &u.Email, &u.Groups, &u.ProtectedAdmin, &u.AuthMethod, &u.PasswordChangeRequired, &u.Overrides)
	return u, err
}

func (h *Handler) withSession(allowPasswordChange bool, next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := h.session(r)
		if errors.Is(err, pgx.ErrNoRows) {
			h.cookie(w, sessionCookie, "", -1)
			fail(w, 401, "authentication_error", "authentication required")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		if u.PasswordChangeRequired && !allowPasswordChange {
			fail(w, 403, "password_change_required", "change your password before using the portal")
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

func (h *Handler) signout(w http.ResponseWriter, r *http.Request, u user) {
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
	logoutURL := "/portal/login/local"
	if sso := h.sso.Load(); u.AuthMethod == "oidc" && sso != nil && h.ssoAvailable.Load() {
		providerURL, _ := url.Parse(sso.logout)
		q := providerURL.Query()
		q.Set("client_id", sso.oauth.ClientID)
		q.Set("post_logout_redirect_uri", h.cfg.Portal.BaseURL+"/portal/")
		providerURL.RawQuery = q.Encode()
		logoutURL = providerURL.String()
	}
	respond(w, 200, map[string]string{"logout_url": logoutURL})
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
		result, err := tx.Exec(r.Context(), `INSERT INTO sessions(id_hash,user_id,expires) SELECT $1,id,clock_timestamp()+interval '8 hours' FROM users WHERE id=$2 AND enabled AND issuer<>'local'
			AND NOT EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=users.id AND c.must_change_password)`, hash[:], id)
		if err == nil && result.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return err
	})
	return session, err
}

func (h *Handler) establishLocal(r *http.Request, account auth.LocalAccount) (string, time.Duration, error) {
	session := randomToken()
	hash := sha256.Sum256([]byte(session))
	lifetime, scope := sessionLifetime, "full"
	if account.MustChangePassword {
		lifetime, scope = 10*time.Minute, "password_change"
	}
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		if old, ok := cookieHash(r, loginCookie); ok {
			if _, err := tx.Exec(r.Context(), `DELETE FROM portal_login_attempts WHERE id_hash=$1`, old[:]); err != nil {
				return err
			}
		}
		if old, ok := cookieHash(r, sessionCookie); ok {
			if _, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE id_hash=$1`, old[:]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM sessions WHERE expires<=clock_timestamp()`); err != nil {
			return err
		}
		result, err := tx.Exec(r.Context(), `INSERT INTO sessions(id_hash,user_id,expires,auth_method,scope,credential_revision)
			SELECT $1,u.id,clock_timestamp()+$5::bigint*interval '1 second','local',$6,c.revision
			FROM users u JOIN local_credentials c ON c.user_id=u.id
			WHERE u.id=$2 AND u.issuer='local' AND u.enabled AND c.revision=$3 AND c.must_change_password=$4`,
			hash[:], account.ID, account.CredentialRevision, account.MustChangePassword, int64(lifetime.Seconds()), scope)
		if err == nil && result.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return err
	})
	return session, lifetime, err
}
