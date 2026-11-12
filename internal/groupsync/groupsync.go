// Package groupsync refreshes users' groups and enabled state from Keycloak,
// so that changes reach API keys without the user signing in to the portal.
package groupsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2/clientcredentials"
)

// lock keeps one router syncing at a time.
const lock int64 = 0x4f4c5253594e43 // "OLRSYNC"

// Syncer reads users through the Keycloak admin REST API with a read-only
// service account (client credentials; realm-management view-users).
type Syncer struct {
	cfg    *config.OIDC
	pool   *pgxpool.Pool
	idp    *oidcclient.Client
	client *http.Client // carries the service account's token
	admin  string       // <base>/admin/realms/<realm>
	log    *slog.Logger
}

// Result counts what one run did.
type Result struct {
	Checked, Updated, Disabled int
}

// New discovers the provider and derives the admin API from the issuer,
// which must have the Keycloak form <base>/realms/<realm>.
func New(ctx context.Context, cfg *config.OIDC, secret string, pool *pgxpool.Pool, log *slog.Logger) (*Syncer, error) {
	admin, err := AdminURL(cfg)
	if err != nil {
		return nil, err
	}
	idp, err := oidcclient.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	credentials := clientcredentials.Config{ClientID: cfg.SyncClientID, ClientSecret: secret, TokenURL: idp.Provider.Endpoint().TokenURL}
	client := credentials.Client(idp.Context(context.Background()))
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Syncer{cfg: cfg, pool: pool, idp: idp, client: client, admin: admin, log: log}, nil
}

// ErrNotKeycloak reports an issuer without the Keycloak realm path.
var ErrNotKeycloak = errors.New("group sync needs a Keycloak issuer of the form <base>/realms/<realm>")

// AdminURL derives the Keycloak admin API of the issuer's realm. It lives next
// to the realm and is reached the way the router reaches the issuer
// (OIDC_BACKCHANNEL_URL when set).
func AdminURL(cfg *config.OIDC) (string, error) {
	reach := cfg.Issuer
	if cfg.BackchannelURL != "" {
		reach = cfg.BackchannelURL
	}
	issuer, err := url.Parse(reach)
	if err != nil {
		return "", errors.New("invalid issuer")
	}
	base, realm, ok := strings.Cut(strings.TrimSuffix(issuer.Path, "/"), "/realms/")
	if !ok || realm == "" || strings.Contains(realm, "/") {
		return "", ErrNotKeycloak
	}
	admin := *issuer
	admin.Path = base + "/admin/realms/" + realm
	return admin.String(), nil
}

func (s *Syncer) Close() { s.idp.Close() }

var errGone = errors.New("user not found in the identity provider")

func (s *Syncer) get(ctx context.Context, path string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.admin+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("identity provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errGone
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("identity provider answered %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(value)
}

// Sync checks every enabled user of the configured issuer: a user removed or
// disabled in Keycloak is disabled here, with their keys and sessions; a
// changed group membership is stored. A user disabled here is not enabled
// again automatically. Group names are used, as the groups mapper sends
// them without the full path.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	var result Result
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return result, errors.New("database unavailable")
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lock).Scan(&locked); err != nil || !locked {
		return result, err // another router is syncing
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lock)
	rows, err := s.pool.Query(ctx, `SELECT id, sub, groups FROM users WHERE issuer = $1 AND enabled ORDER BY id`, s.cfg.Issuer)
	if err != nil {
		return result, errors.New("list users failed")
	}
	type local struct {
		id, sub string
		groups  []string
	}
	var users []local
	for rows.Next() {
		var u local
		if err := rows.Scan(&u.id, &u.sub, &u.groups); err != nil {
			rows.Close()
			return result, errors.New("list users failed")
		}
		users = append(users, u)
	}
	rows.Close()
	admin := auth.Admin{Pool: s.pool}
	for _, u := range users {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Checked++
		var remote struct {
			Enabled bool `json:"enabled"`
		}
		err := s.get(ctx, "/users/"+url.PathEscape(u.sub), &remote)
		if errors.Is(err, errGone) || (err == nil && !remote.Enabled) {
			if err := admin.SetEnabled(ctx, u.id, false); err != nil {
				return result, errors.New("disable user failed")
			}
			result.Disabled++
			continue
		}
		if err != nil {
			return result, err
		}
		var memberships []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		if err := s.get(ctx, "/users/"+url.PathEscape(u.sub)+"/groups?briefRepresentation=true&max=1000", &memberships); err != nil {
			return result, err
		}
		// Full paths let the shared naming rule find ambiguous leaf names.
		paths := make([]string, 0, len(memberships))
		for _, m := range memberships {
			if m.Path == "" {
				m.Path = "/" + m.Name
			}
			paths = append(paths, m.Path)
		}
		groups := auth.GroupNames(paths, s.cfg.GroupNames)
		current := slices.Clone(u.groups)
		slices.Sort(current)
		if !slices.Equal(groups, current) {
			if err := admin.SetGroups(ctx, u.id, groups); err != nil {
				return result, errors.New("update groups failed")
			}
			result.Updated++
		}
	}
	return result, nil
}

// Run syncs at start and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	for {
		runCtx, cancel := context.WithTimeout(ctx, interval)
		result, err := s.Sync(runCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.log.Error("group sync failed", "checked", result.Checked, "updated", result.Updated, "disabled", result.Disabled)
		} else if err == nil {
			s.log.Info("group sync", "checked", result.Checked, "updated", result.Updated, "disabled", result.Disabled)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
