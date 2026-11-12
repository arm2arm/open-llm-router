// Package backendpolicy confines Admin-configured destinations and credentials.
package backendpolicy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

type Policy struct {
	trusted  map[string]bool
	networks []netip.Prefix
	tokens   map[string]string
}

func New(cfg *config.Config, lookup func(string) (string, bool)) (*Policy, error) {
	p := &Policy{trusted: map[string]bool{}, tokens: map[string]string{}}
	for _, model := range cfg.Models {
		for _, r := range model.Pool {
			u, _ := url.Parse(r.URL)
			p.trusted[address(u)] = true
			if r.BearerTokenEnv != "" {
				p.tokens[r.BearerTokenEnv] = r.BearerToken
			}
		}
	}
	if raw, _ := lookup("OLR_BACKEND_ALLOWED_CIDRS"); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			network, err := netip.ParsePrefix(strings.TrimSpace(item))
			if err != nil {
				return nil, errors.New("OLR_BACKEND_ALLOWED_CIDRS must contain comma-separated IP prefixes")
			}
			p.networks = append(p.networks, network)
		}
	}
	if raw, _ := lookup("OLR_BACKEND_TOKEN_ENVS"); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			name := strings.TrimSpace(item)
			if name == "" {
				continue
			}
			token, ok := lookup(name)
			if !ok || token == "" || strings.ContainsAny(token, "\r\n") {
				return nil, errors.New("OLR_BACKEND_TOKEN_ENVS must name available single-line backend tokens")
			}
			p.tokens[name] = token
		}
	}
	return p, nil
}

func address(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (p *Policy) Token(name string) (string, bool) { token, ok := p.tokens[name]; return token, ok }
func (p *Policy) References() []string {
	names := []string{}
	for name := range p.tokens {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (p *Policy) Networks() []string {
	networks := []string{}
	for _, network := range p.networks {
		networks = append(networks, network.String())
	}
	return networks
}

func (p *Policy) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		v4 := ip.As4()
		// Unspecified/reserved and protocol-assignment networks may expose
		// host or cloud control services; they are not GPU network grants.
		if v4[0] == 0 || v4[0] >= 240 || (v4[0] == 192 && v4[1] == 0 && v4[2] == 0) {
			return false
		}
	}
	if ip == netip.MustParseAddr("100.100.100.200") || ip == netip.MustParseAddr("fd00:ec2::254") {
		return false
	}
	// These remain forbidden even when an operator permits a broad CIDR.
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if !ip.IsPrivate() {
		return true
	}
	for _, network := range p.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *Policy) addresses(ctx context.Context, destination string) ([]string, error) {
	if p.trusted[destination] {
		return []string{destination}, nil
	}
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		return nil, errors.New("invalid backend destination")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("backend hostname could not be resolved")
	}
	out := []string{}
	for _, ip := range ips {
		if !p.allowed(ip) {
			return nil, errors.New("backend destination is not permitted; private GPU networks require OLR_BACKEND_ALLOWED_CIDRS")
		}
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	return out, nil
}

func (p *Policy) Check(ctx context.Context, origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("backend URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	_, err = p.addresses(ctx, address(u))
	return err
}

// DialContext checks DNS answers on every new connection and dials the checked
// IP, preventing DNS rebinding between validation and inference/health traffic.
func (p *Policy) DialContext(ctx context.Context, network, destination string) (net.Conn, error) {
	addresses, err := p.addresses(ctx, destination)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	for _, target := range addresses {
		conn, err := dialer.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("backend connection failed")
}
