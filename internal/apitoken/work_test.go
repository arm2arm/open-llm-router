package apitoken

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/oidcclient"
	"github.com/coreos/go-oidc/v3/oidc"
)

type keySetFunc func(context.Context, string) ([]byte, error)

func (f keySetFunc) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	return f(ctx, raw)
}

func TestTokenMissesShareWorkBudgetAndCacheFailures(t *testing.T) {
	work := auth.NewWorkLimiter()
	started := make(chan struct{}, auth.MaxCredentialWork)
	release := make(chan struct{})
	var calls atomic.Int64
	keys := keySetFunc(func(ctx context.Context, _ string) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return nil, errors.New("invalid signature")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	v := &Verifier{cfg: &config.OIDC{Issuer: "https://idp.example"}, now: time.Now, work: work,
		fresh: func() (uint64, bool) { return 1, true }, cache: map[[32]byte]entry{},
		idp:      &oidcclient.Client{HTTP: http.DefaultClient},
		verifier: oidc.NewVerifier("https://idp.example", keys, &oidc.Config{ClientID: "api"})}
	expires := time.Now().Add(time.Hour).Unix()
	token := func(i int) string {
		header := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"alg":"RS256","kid":"%d"}`, i))
		claims := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"iss":"https://idp.example","aud":"api","sub":"user","exp":%d}`, expires))
		return header + "." + claims + ".c2lnbmF0dXJl"
	}
	var wg sync.WaitGroup
	for i := range auth.MaxCredentialWork - 1 {
		wg.Go(func() { _, _ = v.Authenticate(t.Context(), token(i)) })
	}
	for range auth.MaxCredentialWork - 1 {
		<-started
	}
	// The remaining slot represents an in-flight API-key lookup.
	if !work.TryAcquire() {
		t.Fatal("token requests took too many slots")
	}
	for i := range 256 {
		if _, err := v.Authenticate(t.Context(), token(i+100)); !errors.Is(err, auth.ErrAuthBusy) {
			t.Fatal("unique token did not fail fast on shared overload", err)
		}
	}
	work.Release()
	close(release)
	wg.Wait()
	if calls.Load() != auth.MaxCredentialWork-1 {
		t.Fatal("verification concurrency exceeded budget", calls.Load())
	}
	for range 5 {
		if _, err := v.Authenticate(t.Context(), token(0)); !errors.Is(err, auth.ErrInvalidKey) {
			t.Fatal(err)
		}
	}
	if calls.Load() != auth.MaxCredentialWork-1 {
		t.Fatal("negative token cache repeated verification")
	}
	// All slots were returned; none of the overload failures poisoned tokens.
	if _, err := v.Authenticate(t.Context(), token(100)); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatal(err)
	}
}
