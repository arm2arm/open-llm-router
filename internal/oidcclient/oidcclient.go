// Package oidcclient builds the HTTP client and discovery for the configured
// identity provider, shared by portal login, API tokens and group sync.
package oidcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Client talks to the identity provider. With OIDC_BACKCHANNEL_URL set,
// requests to the public issuer's subtree go to that internal URL instead;
// the expected issuer stays the public one. Redirects are never followed.
type Client struct {
	HTTP      *http.Client
	Provider  *oidc.Provider
	transport *http.Transport
}

// ErrIssuerMismatch reports discovery metadata naming a different issuer.
var ErrIssuerMismatch = errors.New("the provider reports a different issuer")

// Options restrict a client used for administrator-supplied settings.
type Options struct {
	// Allow is called with every address before connecting; an error
	// refuses it. Checking at dial time also covers DNS rebinding.
	// Environment proxies are not used when Allow is set.
	Allow func(netip.Addr) error
}

// New discovers the provider. Close releases idle connections.
func New(ctx context.Context, cfg *config.OIDC) (*Client, error) {
	client, err := NewWith(ctx, cfg, Options{})
	if err != nil {
		return nil, &discoveryError{cause: err}
	}
	return client, nil
}

// NewWith is New with connection restrictions; its error distinguishes an
// issuer mismatch from other discovery failures.
func NewWith(ctx context.Context, cfg *config.OIDC, options Options) (*Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if options.Allow != nil {
		transport.Proxy = nil
		dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			return options.Allow(addr.Unmap())
		}}
		transport.DialContext = dialer.DialContext
	}
	var rt http.RoundTripper = transport
	if cfg.RequireTLS {
		rt = httpsOnly{next: rt}
	}
	if cfg.BackchannelURL != "" {
		public, err := url.Parse(cfg.Issuer)
		if err != nil {
			return nil, errors.New("invalid issuer")
		}
		internal, err := url.Parse(cfg.BackchannelURL)
		if err != nil {
			return nil, errors.New("invalid OIDC backchannel URL")
		}
		rt = backchannel{public, internal, rt}
	}
	var responseStatus atomic.Int32
	rt = responseLimit{next: rt, status: &responseStatus}
	client := &http.Client{Transport: rt, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		transport.CloseIdleConnections()
		var mismatch *oidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			return nil, ErrIssuerMismatch
		}
		if status := int(responseStatus.Load()); status >= 300 {
			return nil, &providerStatusError{status: status}
		}
		// The cause may name URLs; New and callers expose fixed messages only.
		return nil, fmt.Errorf("OIDC discovery failed: %w", err)
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		transport.CloseIdleConnections()
		return nil, errors.New("invalid OIDC discovery metadata")
	}
	// Install before exposing the client. Provider retains this HTTP client
	// for its shared RemoteKeySet, including detached refresh goroutines.
	client.Transport = &jwksThrottle{next: rt, endpoint: metadata.JWKS, now: time.Now}
	return &Client{HTTP: client, Provider: provider, transport: transport}, nil
}

// Context carries the client for go-oidc and x/oauth2 calls.
func (c *Client) Context(ctx context.Context) context.Context { return oidc.ClientContext(ctx, c.HTTP) }

func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Discovery, JWKS and token responses are metadata, not unbounded downloads.
// Bound reads even when Content-Length is absent, incorrect or compressed.
const maxResponseBytes = 1 << 20

var errResponseTooLarge = errors.New("identity provider response exceeds 1 MiB")

type responseLimit struct {
	next   http.RoundTripper
	status *atomic.Int32
}

func (t responseLimit) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if t.status != nil {
		t.status.Store(int32(resp.StatusCode))
	}
	if resp.ContentLength > maxResponseBytes {
		resp.Body.Close()
		return nil, errResponseTooLarge
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, remaining: maxResponseBytes}
	return resp, nil
}

type limitedBody struct {
	io.ReadCloser
	remaining int
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var extra [1]byte
		if n, err := b.ReadCloser.Read(extra[:]); n == 0 {
			return 0, err
		}
		return 0, errResponseTooLarge
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}

type backchannel struct {
	public, internal *url.URL
	next             http.RoundTripper
}

func (b backchannel) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := strings.TrimSuffix(b.public.Path, "/")
	if r.URL.Scheme == b.public.Scheme && r.URL.Host == b.public.Host && (r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/")) {
		r = r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = b.internal.Scheme, b.internal.Host
		u.Path = strings.TrimSuffix(b.internal.Path, "/") + strings.TrimPrefix(u.Path, prefix)
		u.RawPath = ""
		r.URL = &u
		r.Host = ""
	}
	return b.next.RoundTrip(r)
}

// Validate the actual destination, including provider-advertised token/JWKS URLs
// and rewritten back-channel requests, before sending any credentials.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, errors.New("production identity-provider requests require HTTPS")
	}
	return h.next.RoundTrip(r)
}
