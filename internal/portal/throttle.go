package portal

import (
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

var errThrottled = errors.New("local authentication throttled")

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	return ip.String() // Forwarded/X-Forwarded-For are deliberately ignored.
}

// Each request has bounded database state and at most two concurrent Argon2
// operations per process. Fixed one-minute windows never extend on rejection
// and never mutate users.enabled, including for the protected administrator.
func (h *Handler) allowPassword(w http.ResponseWriter, r *http.Request, username string) bool {
	select {
	case h.passwordSlots <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 429, "rate_limit_error", "too many authentication attempts; try again shortly")
		return false
	}
	retry := 1
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		const lock int64 = 0x4f4c524c4f47494e // "OLRLOGIN"
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock($1)`, lock); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM portal_auth_throttle WHERE expires<=clock_timestamp()`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM portal_auth_throttle`).Scan(&count); err != nil {
			return err
		}
		for _, bucket := range []struct {
			name  string
			limit int
		}{
			{"global", 60}, {"ip:" + clientAddress(r), 20}, {"user:" + username, 10},
		} {
			hash := sha256.Sum256([]byte(bucket.name))
			var attempts, remaining int
			err := tx.QueryRow(r.Context(), `SELECT attempts,GREATEST(1,ceil(extract(epoch FROM expires-clock_timestamp())))::integer
				FROM portal_auth_throttle WHERE bucket=$1`, hash[:]).Scan(&attempts, &remaining)
			if errors.Is(err, pgx.ErrNoRows) {
				if count >= 4096 {
					retry = 60
					return errThrottled
				}
				if _, err := tx.Exec(r.Context(), `INSERT INTO portal_auth_throttle(bucket,attempts,expires) VALUES($1,1,clock_timestamp()+interval '1 minute')`, hash[:]); err != nil {
					return err
				}
				count++
				continue
			}
			if err != nil {
				return err
			}
			if attempts >= bucket.limit {
				retry = remaining
				return errThrottled
			}
			if _, err := tx.Exec(r.Context(), `UPDATE portal_auth_throttle SET attempts=attempts+1 WHERE bucket=$1`, hash[:]); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return true
	}
	<-h.passwordSlots
	if errors.Is(err, errThrottled) {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		fail(w, 429, "rate_limit_error", "too many authentication attempts; try again shortly")
	} else {
		unavailable(w)
	}
	return false
}
