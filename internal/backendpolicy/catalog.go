package backendpolicy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// CatalogClient is a short-lived capability for one checked backend. It builds
// request URLs from resolved IP addresses, never from the administrator's URL.
// The original authority is used only for HTTP Host and TLS verification.
// Its fixed catalog paths cannot be used to probe arbitrary backend resources.
type CatalogClient struct {
	client    *http.Client
	transport *http.Transport
	target    url.URL
	authority string
}

func (p *Policy) NewCatalogClient(ctx context.Context, origin string, private bool) (*CatalogClient, error) {
	if err := p.CheckBackend(ctx, origin, private); err != nil {
		return nil, err
	}
	u, _ := url.Parse(origin) // CheckBackend validated the origin.
	if private {
		ctx = WithPrivateNetwork(ctx, origin)
	}
	addresses, err := p.addresses(ctx, address(u))
	if err != nil {
		return nil, err
	}
	// File-configured destinations are trusted by the operator and may still
	// contain DNS names. Resolve those too, so the transport never resolves a
	// hostname after this capability has been created.
	var pinned []string
	for _, destination := range addresses {
		host, port, err := net.SplitHostPort(destination)
		if err != nil {
			return nil, errors.New("invalid backend destination")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("backend hostname could not be resolved")
		}
		for _, ip := range ips {
			pinned = append(pinned, net.JoinHostPort(ip.String(), port))
		}
	}
	host, _, _ := net.SplitHostPort(pinned[0])
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, errors.New("invalid backend address")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("backend port must be between 1 and 65535")
		}
	}
	scheme := "http"
	if u.Scheme == "https" {
		scheme = "https"
	}
	target := url.URL{Scheme: scheme, Host: net.JoinHostPort(ip.String(), strconv.Itoa(port))}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// Preserve configured trust roots and verify the original hostname even
	// though the URL and socket use a pinned IP (including virtual hosts).
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ServerName = u.Hostname()
	transport.TLSClientConfig.InsecureSkipVerify = false
	transport.DialContext = func(ctx context.Context, network, destination string) (net.Conn, error) {
		if destination != target.Host {
			return nil, errors.New("catalog destination does not match the checked backend")
		}
		dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
		var failures []error
		for _, destination := range pinned {
			conn, err := dialer.DialContext(ctx, network, destination)
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("backend connection failed: %w", errors.Join(failures...))
	}
	return &CatalogClient{
		client: &http.Client{Transport: transport, Timeout: 3 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		transport: transport, target: target, authority: u.Host,
	}, nil
}

// Get permits only the two catalog resources supported by backend discovery.
func (c *CatalogClient) Get(ctx context.Context, path, token string) (*http.Response, error) {
	if c == nil || c.client == nil || (path != "/v1/models" && path != "/api/tags") {
		return nil, errors.New("invalid model catalog resource")
	}
	target := c.target
	target.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errors.New("invalid model catalog request")
	}
	req.Host = c.authority
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.client.Do(req)
}

func (c *CatalogClient) Close() { c.transport.CloseIdleConnections() }
