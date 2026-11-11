package auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errUnknownEndUser  = errors.New("unknown end user")
	errDisabledEndUser = errors.New("end user is disabled")
	errEndUserConflict = errors.New("end-user headers name different users")
)

type endUserEntry struct {
	principal Principal
	until     time.Time
}

func endUserKey(attribute, value string) string { return attribute + "\x00" + value }

// lookupEndUser finds the one enabled user whose attribute equals value.
// Email matches without regard to case. A value that matches no user, or
// several (an email is not unique), is unknown.
func (a *Authenticator) lookupEndUser(ctx context.Context, attribute, value string) (Principal, error) {
	a.mu.Lock()
	generation := a.generation
	entry, ok := a.endUsers[endUserKey(attribute, value)]
	if ok && a.listening && a.now().Before(entry.until) {
		a.mu.Unlock()
		return entry.principal.clone(), nil
	}
	a.mu.Unlock()
	if a.pool == nil {
		return Principal{}, errUnknownEndUser
	}
	where := map[string]string{"id": "id = $1::uuid", "username": "username = $1", "email": "lower(email) = lower($1) AND email <> ''"}[attribute]
	if where == "" {
		return Principal{}, errUnknownEndUser
	}
	if attribute == "id" {
		var id pgtype.UUID
		if id.Scan(value) != nil || !id.Valid {
			return Principal{}, errUnknownEndUser
		}
	}
	rows, err := a.pool.Query(ctx, `SELECT id, username, groups, enabled,
		(SELECT jsonb_object_agg(o.field, o.value) FROM quota_overrides o WHERE o.user_id = users.id)
		FROM users WHERE `+where+` LIMIT 2`, value)
	if err != nil {
		return Principal{}, errors.New("end-user lookup failed")
	}
	var found []Principal
	var enabled bool
	for rows.Next() {
		var p Principal
		if err := rows.Scan(&p.UserID, &p.Username, &p.Groups, &enabled, &p.Overrides); err != nil {
			rows.Close()
			return Principal{}, errors.New("end-user lookup failed")
		}
		found = append(found, p)
	}
	rows.Close()
	if rows.Err() != nil {
		return Principal{}, errors.New("end-user lookup failed")
	}
	if len(found) != 1 {
		return Principal{}, errUnknownEndUser
	}
	if !enabled {
		return Principal{}, errDisabledEndUser
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation == a.generation && a.listening {
		if a.endUsers == nil || len(a.endUsers) >= maxCachedKeys {
			a.endUsers = make(map[string]endUserEntry)
		}
		a.endUsers[endUserKey(attribute, value)] = endUserEntry{principal: found[0], until: a.now().Add(cacheTTL)}
	}
	return found[0].clone(), nil
}

// forward resolves the end user a trusted forwarder names. It returns the
// forwarder itself when no end-user header is set, or when the user is
// unknown and the policy is forwarder_policy. The headers are removed from
// r in every case: they never reach a backend, and from a key that is not a
// trusted forwarder they are ignored.
func (a *Authenticator) forward(r *http.Request, p Principal) (Principal, error) {
	type named struct{ attribute, value string }
	var names []named
	var multiple bool
	for _, header := range slices.Sorted(maps.Keys(a.Forwarders.HeaderMap)) {
		values := r.Header.Values(header)
		if len(values) > 1 {
			multiple = true
		}
		if len(values) == 1 && values[0] != "" {
			names = append(names, named{a.Forwarders.HeaderMap[header], values[0]})
		}
		r.Header.Del(header)
	}
	if !p.TrustedForwarder || (len(names) == 0 && !multiple) {
		return p, nil
	}
	if multiple {
		return Principal{}, errEndUserConflict
	}
	var end Principal
	for i, n := range names {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		user, err := a.lookupEndUser(ctx, n.attribute, n.value)
		cancel()
		if errors.Is(err, errUnknownEndUser) && a.Forwarders.UnknownUser == config.UnknownForwarderPolicy {
			return p, nil
		}
		if err != nil {
			return Principal{}, err
		}
		if i > 0 && user.UserID != end.UserID {
			return Principal{}, errEndUserConflict
		}
		end = user
	}
	end.KeyID, end.KeyClass, end.ForwardedBy = p.KeyID, p.KeyClass, p.UserID
	return end, nil
}
