package oidcclient

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

const jwksRefreshInterval = 10 * time.Second

var errJWKSRefreshThrottled = errors.New("JWKS refresh temporarily throttled")

// jwksThrottle complements go-oidc's singleflight: sequential unknown kids
// (or forged signatures with a known kid) cannot keep refreshing the IdP.
// Failed fetches consume the interval too. Rejections never extend it, so a
// real signing-key rotation can be discovered on the next allowed attempt.
type jwksThrottle struct {
	next     http.RoundTripper
	endpoint string
	now      func() time.Time
	mu       sync.Mutex
	until    time.Time
	active   bool
}

func (t *jwksThrottle) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.String() != t.endpoint {
		return t.next.RoundTrip(r)
	}
	t.mu.Lock()
	now := t.now()
	if t.active || now.Before(t.until) {
		t.mu.Unlock()
		return nil, errJWKSRefreshThrottled
	}
	t.until, t.active = now.Add(jwksRefreshInterval), true
	t.mu.Unlock()
	release := func() {
		t.mu.Lock()
		t.active = false
		t.mu.Unlock()
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &jwksBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// Keep the single network slot until the response is consumed or closed.
type jwksBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *jwksBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
