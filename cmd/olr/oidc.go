package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arm2arm/open-llm-router/internal/apitoken"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

// oidcServices runs the OIDC-dependent background services, API-token
// verification and group sync, for one configuration at a time. Switch stops
// the previous configuration's jobs before starting the next, so a replaced
// verifier and its principal cache are never used again. Services initialize
// independently of listening/local authentication; failed discovery is
// retried and API keys remain usable throughout an IdP outage.
type oidcServices struct {
	stats       *groupsync.Stats
	pool        *pgxpool.Pool
	credentials *pgxpool.Pool
	work        *auth.WorkLimiter
	fresh       func() (uint64, bool)
	log         *slog.Logger
	mu          sync.Mutex
	verifier    atomic.Pointer[apitoken.Verifier]
	groupSync   atomic.Pointer[groupsync.Syncer]
	audience    atomic.Bool // the active configuration accepts IdP tokens
	stop        func()
}

func newOIDCServices(cfg *config.Config, pool, credentials *pgxpool.Pool, authenticator *auth.Authenticator, log *slog.Logger, stats *groupsync.Stats) *oidcServices {
	if stats == nil {
		stats = &groupsync.Stats{}
	}
	s := &oidcServices{stats: stats, pool: pool, credentials: credentials, work: authenticator.WorkBudget(), log: log, fresh: authenticator.Freshness, stop: func() {}}
	authenticator.Tokens = func(ctx context.Context, raw string) (auth.Principal, error) {
		if !s.audience.Load() {
			return auth.Principal{}, auth.ErrInvalidKey // not a key, and IdP tokens are off
		}
		if verifier := s.verifier.Load(); verifier != nil {
			return verifier.Authenticate(ctx, raw)
		}
		return auth.Principal{}, errors.New("OIDC API authentication is unavailable")
	}
	s.Switch(cfg)
	return s
}

// Switch replaces the running services with those of cfg.
func (s *oidcServices) Switch(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
	s.groupSync.Store(nil)
	s.stats.Enabled.Store(false)
	s.stats.IntervalSeconds.Store(0)
	s.stats.LastSuccess.Store(0)
	s.audience.Store(false)
	s.verifier.Store(nil)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if cfg.OIDC != nil && cfg.OIDC.Audience != "" {
		s.audience.Store(true)
		oidcCfg := cfg.OIDC
		wg.Go(func() {
			retryOIDC(ctx, s.log, "API tokens", func(ctx context.Context) error {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				verifier, err := apitoken.New(attempt, oidcCfg, s.credentials, s.fresh, s.work)
				cancel()
				if err != nil {
					return err
				}
				defer verifier.Close()
				s.verifier.Store(verifier)
				s.log.Info("OIDC API tokens available")
				<-ctx.Done()
				s.verifier.CompareAndSwap(verifier, nil)
				return nil
			})
		})
	}
	if cfg.OIDC != nil && cfg.OIDCSyncSecret != "" {
		s.stats.Enabled.Store(true)
		s.stats.IntervalSeconds.Store(int64(cfg.OIDC.SyncIntervalS))
		oidcCfg, secret := cfg.OIDC, cfg.OIDCSyncSecret
		wg.Go(func() {
			retryOIDC(ctx, s.log, "group sync", func(ctx context.Context) error {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				syncer, err := groupsync.New(attempt, oidcCfg, secret, s.pool, s.log)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						s.stats.Record(groupsync.Result{}, err)
					}
					return err
				}
				syncer.Stats = s.stats
				defer syncer.Close()
				s.groupSync.Store(syncer)
				defer s.groupSync.CompareAndSwap(syncer, nil)
				syncer.Run(ctx, time.Duration(oidcCfg.SyncIntervalS)*time.Second)
				return nil
			})
		})
	}
	s.stop = func() { cancel(); wg.Wait() }
}

func (s *oidcServices) GroupSyncEnabled() bool { return s.stats.Enabled.Load() }

// SyncGroups uses the running configuration, never an unactivated draft.
// Hold the lifecycle lock so switching settings cannot close this syncer's
// HTTP client or start a replacement job during a manual run.
func (s *oidcServices) SyncGroups(ctx context.Context) (groupsync.Result, error) {
	if !s.mu.TryLock() {
		return groupsync.Result{Skipped: true}, nil
	}
	defer s.mu.Unlock()
	if !s.GroupSyncEnabled() {
		return groupsync.Result{}, groupsync.ErrDisabled
	}
	syncer := s.groupSync.Load()
	if syncer == nil {
		return groupsync.Result{}, groupsync.ErrUnavailable
	}
	return syncer.RunOnce(ctx)
}

// LookupIdentity reads one user through the running group-sync service
// account. It never uses an unactivated draft or another realm's account.
func (s *oidcServices) LookupIdentity(ctx context.Context, issuer, subject string) (groupsync.Identity, error) {
	if !s.GroupSyncEnabled() {
		return groupsync.Identity{}, groupsync.ErrDisabled
	}
	syncer := s.groupSync.Load()
	if syncer == nil {
		return groupsync.Identity{}, groupsync.ErrUnavailable
	}
	if syncer.Issuer() != issuer {
		return groupsync.Identity{}, groupsync.ErrDisabled
	}
	return syncer.Lookup(ctx, subject)
}

// Close stops the services.
func (s *oidcServices) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
	s.stop = func() {}
}

func retryOIDC(ctx context.Context, log *slog.Logger, service string, initialize func(context.Context) error) {
	for ctx.Err() == nil {
		err := initialize(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		cause := oidcclient.DiagnosticCause(err)
		if errors.Is(err, groupsync.ErrNotKeycloak) {
			cause = "group sync issuer is not a Keycloak realm"
		}
		log.Warn("OIDC service unavailable; retrying", "service", service, "cause", cause)
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
