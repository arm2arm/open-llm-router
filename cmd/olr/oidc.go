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
	"github.com/jackc/pgx/v5/pgxpool"
)

// oidcServices runs the OIDC-dependent background services, API-token
// verification and group sync, for one configuration at a time. Switch stops
// the previous configuration's jobs before starting the next, so a replaced
// verifier and its principal cache are never used again. Services initialize
// independently of listening/local authentication; failed discovery is
// retried and API keys remain usable throughout an IdP outage.
type oidcServices struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	mu       sync.Mutex
	verifier atomic.Pointer[apitoken.Verifier]
	audience atomic.Bool // the active configuration accepts IdP tokens
	stop     func()
}

func newOIDCServices(cfg *config.Config, pool *pgxpool.Pool, authenticator *auth.Authenticator, log *slog.Logger) *oidcServices {
	s := &oidcServices{pool: pool, log: log, stop: func() {}}
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
	s.audience.Store(false)
	s.verifier.Store(nil)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if cfg.OIDC != nil && cfg.OIDC.Audience != "" {
		s.audience.Store(true)
		oidcCfg := cfg.OIDC
		wg.Go(func() {
			retryOIDC(ctx, s.log, "API tokens", func(ctx context.Context) bool {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				verifier, err := apitoken.New(attempt, oidcCfg, s.pool)
				cancel()
				if err != nil {
					return false
				}
				defer verifier.Close()
				s.verifier.Store(verifier)
				s.log.Info("OIDC API tokens available")
				<-ctx.Done()
				s.verifier.CompareAndSwap(verifier, nil)
				return true
			})
		})
	}
	if cfg.OIDC != nil && cfg.OIDCSyncSecret != "" {
		oidcCfg, secret := cfg.OIDC, cfg.OIDCSyncSecret
		wg.Go(func() {
			retryOIDC(ctx, s.log, "group sync", func(ctx context.Context) bool {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				syncer, err := groupsync.New(attempt, oidcCfg, secret, s.pool, s.log)
				cancel()
				if err != nil {
					return false
				}
				defer syncer.Close()
				syncer.Run(ctx, time.Duration(oidcCfg.SyncIntervalS)*time.Second)
				return true
			})
		})
	}
	s.stop = func() { cancel(); wg.Wait() }
}

// Close stops the services.
func (s *oidcServices) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
	s.stop = func() {}
}

func retryOIDC(ctx context.Context, log *slog.Logger, service string, initialize func(context.Context) bool) {
	for ctx.Err() == nil {
		if initialize(ctx) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("OIDC service unavailable; retrying", "service", service)
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
