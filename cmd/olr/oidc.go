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

// OIDC services initialize independently of listening/local authentication.
// Failed discovery is retried; API keys remain usable throughout an IdP outage.
func startOIDCServices(cfg *config.Config, pool *pgxpool.Pool, authenticator *auth.Authenticator, log *slog.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if cfg.OIDC != nil && cfg.OIDC.Audience != "" {
		var active atomic.Pointer[apitoken.Verifier]
		authenticator.Tokens = func(ctx context.Context, raw string) (auth.Principal, error) {
			if verifier := active.Load(); verifier != nil {
				return verifier.Authenticate(ctx, raw)
			}
			return auth.Principal{}, errors.New("OIDC API authentication is unavailable")
		}
		wg.Go(func() {
			retryOIDC(ctx, log, "API tokens", func(ctx context.Context) bool {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				verifier, err := apitoken.New(attempt, cfg.OIDC, pool)
				cancel()
				if err != nil {
					return false
				}
				defer verifier.Close()
				active.Store(verifier)
				log.Info("OIDC API tokens available")
				<-ctx.Done()
				return true
			})
		})
	}
	if cfg.OIDC != nil && cfg.OIDCSyncSecret != "" {
		wg.Go(func() {
			retryOIDC(ctx, log, "group sync", func(ctx context.Context) bool {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				syncer, err := groupsync.New(attempt, cfg.OIDC, cfg.OIDCSyncSecret, pool, log)
				cancel()
				if err != nil {
					return false
				}
				defer syncer.Close()
				syncer.Run(ctx, time.Duration(cfg.OIDC.SyncIntervalS)*time.Second)
				return true
			})
		})
	}
	return func() { cancel(); wg.Wait() }
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
