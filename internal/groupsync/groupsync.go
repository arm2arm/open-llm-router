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
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
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
	mu     sync.Mutex
	cursor string // resume after the last attempted user, including failures
	cfg    *config.OIDC
	pool   *pgxpool.Pool
	idp    *oidcclient.Client
	client *http.Client // carries the service account's token
	admin  string       // <base>/admin/realms/<realm>
	log    *slog.Logger
}

// Result counts what one run did.
type Result struct {
	Checked, Updated, Disabled, Failed, Remaining int
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
var errProvider = errors.New("identity provider unavailable")

type responseError struct{ status int }

func (e responseError) Error() string { return fmt.Sprintf("identity provider answered %d", e.status) }

func (s *Syncer) get(ctx context.Context, path string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.admin+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: request failed", errProvider)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errGone
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%w: authorization refused (%d)", errProvider, resp.StatusCode)
		}
		return responseError{resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(value)
}

// Sync stages remote state before writing. An unavailable admin API or an
// excessive disable count cannot partly revoke a realm. Individual user errors
// are counted without starving other users; interrupted scans resume next run.
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Result
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return result, errors.New("database unavailable")
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lock).Scan(&locked); err != nil || !locked {
		return result, err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, lock); err != nil {
			_ = conn.Hijack().Close(unlockCtx)
		}
	}()
	rows, err := s.pool.Query(ctx, `SELECT id::text,sub,groups FROM users WHERE issuer=$1 AND enabled
		ORDER BY (id::text > $2) DESC,id`, s.cfg.Issuer, s.cursor)
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
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, errors.New("list users failed")
	}
	if len(users) == 0 {
		return result, nil
	}

	// Reserve a quarter of the run deadline for applying already-read changes.
	// A slow user cannot make every run start again at the same first row.
	readCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)*3/4))
		defer cancel()
	}
	// A realm-wide proxy/admin-path 404 is an outage, never proof that each
	// local user was deleted. Require a working collection route first.
	var probe []json.RawMessage
	if err := s.get(readCtx, "/users?first=0&max=1&briefRepresentation=true", &probe); err != nil {
		return result, fmt.Errorf("identity provider admin API check failed: %w", err)
	}
	if probe == nil {
		return result, errors.New("identity provider admin API returned an invalid user list")
	}
	type change struct {
		user     local
		disabled bool
		groups   []string
	}
	var changes []change
	disables := 0
	var failureCause error
	for i, u := range users {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if readCtx.Err() != nil {
			result.Remaining = len(users) - i
			failureCause = readCtx.Err()
			break
		}
		s.cursor = u.id
		result.Checked++
		var remote struct {
			Enabled *bool `json:"enabled"`
		}
		err := s.get(readCtx, "/users/"+url.PathEscape(u.sub), &remote)
		if readCtx.Err() != nil {
			result.Failed++
			result.Remaining = len(users) - i - 1
			failureCause = readCtx.Err()
			break
		}
		if errors.Is(err, errProvider) {
			return result, err
		}
		if errors.Is(err, errGone) || (err == nil && remote.Enabled != nil && !*remote.Enabled) {
			disables++
			changes = append(changes, change{user: u, disabled: true})
			continue
		}
		if err != nil || remote.Enabled == nil {
			result.Failed++
			failureCause = err
			if failureCause == nil {
				failureCause = errors.New("identity provider omitted user enabled state")
			}
			continue
		}
		var memberships []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		err = s.get(readCtx, "/users/"+url.PathEscape(u.sub)+"/groups?briefRepresentation=true&max=1000", &memberships)
		if readCtx.Err() != nil {
			result.Failed++
			result.Remaining = len(users) - i - 1
			failureCause = readCtx.Err()
			break
		}
		if errors.Is(err, errProvider) {
			return result, err
		}
		if err != nil || memberships == nil {
			result.Failed++
			failureCause = err
			if failureCause == nil {
				failureCause = errors.New("identity provider returned invalid memberships")
			}
			continue
		}
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
			changes = append(changes, change{user: u, groups: groups})
		}
	}
	limit := s.cfg.SyncMaxDisable
	if limit == 0 {
		limit = 10
	}
	if disables > limit {
		return result, errors.New("identity sync disable limit exceeded; no users changed")
	}
	admin := auth.Admin{Pool: s.pool, Actor: audit.Actor{Type: "identity_provider_sync"}}
	for _, change := range changes {
		if change.disabled {
			if err := admin.SetEnabled(ctx, change.user.id, false); err != nil {
				return result, errors.New("disable user failed")
			}
			result.Disabled++
		} else {
			if err := admin.SetGroups(ctx, change.user.id, change.groups); err != nil {
				return result, errors.New("update groups failed")
			}
			result.Updated++
		}
	}
	if result.Failed > 0 || result.Remaining > 0 {
		return result, fmt.Errorf("identity sync incomplete; failed or deferred users will be retried: %w", failureCause)
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
			s.log.Error("group sync failed", "error", err, "checked", result.Checked, "updated", result.Updated, "disabled", result.Disabled, "failed", result.Failed, "remaining", result.Remaining)
		} else if err == nil {
			s.log.Info("group sync", "checked", result.Checked, "updated", result.Updated, "disabled", result.Disabled, "failed", result.Failed, "remaining", result.Remaining)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
