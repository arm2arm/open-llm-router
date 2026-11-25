// Package backendpolicy confines Admin-configured destinations and credentials.
package backendpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// ErrUnresolved is a reachability failure, not a destination-policy violation.
var ErrUnresolved = errors.New("backend hostname could not be resolved")

type Policy struct {
	requireTLS   bool
	trusted      map[string]bool
	networks     []netip.Prefix
	tokens       map[string]string
	tokenOrigins map[string]map[string]bool
}

type privateDestination struct{}

// WithPrivateNetwork grants this request access to one administrator-selected
// internal origin, including globally numbered hosts behind a site firewall.
// DNS is still checked and pinned on every new connection.
func WithPrivateNetwork(ctx context.Context, origin string) context.Context {
	u, err := url.Parse(origin)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, privateDestination{}, address(u))
}

func New(cfg *config.Config, lookup func(string) (string, bool)) (*Policy, error) {
	p := &Policy{requireTLS: cfg.Security.RequireTLS, trusted: map[string]bool{}, tokens: map[string]string{}, tokenOrigins: map[string]map[string]bool{}}
	for _, model := range cfg.Models {
		for _, r := range model.Pool {
			u, _ := url.Parse(r.URL)
			p.trusted[address(u)] = true
			if r.BearerTokenEnv != "" {
				p.tokens[r.BearerTokenEnv] = r.BearerToken
				p.bindToken(r.BearerTokenEnv, r.URL)
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
	// Extra references need explicit deployment-owned destinations. A name
	// alone must never authorize an editor to send a secret to a new host.
	if raw, _ := lookup("OLR_BACKEND_TOKEN_ORIGINS"); raw != "" {
		var bindings map[string][]string
		if json.Unmarshal([]byte(raw), &bindings) != nil || bindings == nil {
			return nil, errors.New("OLR_BACKEND_TOKEN_ORIGINS must map backend credential references to arrays of HTTP(S) origins")
		}
		for name, origins := range bindings {
			if _, ok := p.tokens[name]; !ok || len(origins) == 0 {
				return nil, errors.New("OLR_BACKEND_TOKEN_ORIGINS must name available backend credentials and nonempty origin lists")
			}
			for _, origin := range origins {
				if !p.bindToken(name, origin) {
					return nil, errors.New("OLR_BACKEND_TOKEN_ORIGINS contains an invalid backend origin")
				}
			}
		}
	}
	return p, nil
}

func (p *Policy) bindToken(name, raw string) bool {
	origin, err := config.NormalizeBackendURL(raw)
	if err != nil {
		return false
	}
	if p.tokenOrigins[name] == nil {
		p.tokenOrigins[name] = map[string]bool{}
	}
	p.tokenOrigins[name][origin] = true
	return true
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

// Token resolves credentials only for deployment-approved origins. File
// replicas establish bindings automatically; additional bindings are explicit.
func (p *Policy) Token(name, raw string) (string, bool) {
	origin, err := config.NormalizeBackendURL(raw)
	if err != nil || !p.tokenOrigins[name][origin] {
		return "", false
	}
	token, ok := p.tokens[name]
	return token, ok
}
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

var (
	metadataV4 = netip.MustParseAddr("100.100.100.200")
	metadataV6 = netip.MustParseAddr("fd00:ec2::254")
	shared     = netip.MustParsePrefix("100.64.0.0/10")
	// A translator delivers NAT64 (RFC 6052) and deprecated IPv4-compatible
	// addresses to the IPv4 address in their last 32 bits.
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
	ipv4Compatible = netip.MustParsePrefix("::/96")
	// Local-use NAT64 (RFC 8215) embeds IPv4 where the site's translator
	// chooses; it needs an operator grant like a private network.
	localNAT64 = netip.MustParsePrefix("64:ff9b:1::/48")
)

// Reached returns the address that finally receives traffic sent to ip, so a
// translated form of a private or metadata IPv4 address gets its verdict:
// the IPv4 address of IPv4-mapped, NAT64 and IPv4-compatible forms. The
// unspecified and loopback addresses are not IPv4-compatible (RFC 4291).
func Reached(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if nat64.Contains(ip) || ipv4Compatible.Contains(ip) && ip != netip.IPv6Unspecified() && ip != netip.IPv6Loopback() {
		b := ip.As16()
		return netip.AddrFrom4([4]byte(b[12:]))
	}
	return ip
}

func (p *Policy) allowed(ip netip.Addr) bool {
	// A zone scopes only link-local addresses, which are never permitted. On
	// any other address it would defeat the exact comparisons below.
	if ip.Zone() != "" {
		return false
	}
	ip = Reached(ip)
	if ip.Is4() {
		v4 := ip.As4()
		// Unspecified/reserved and protocol-assignment networks may expose
		// host or cloud control services; they are not GPU network grants.
		if v4[0] == 0 || v4[0] >= 240 || (v4[0] == 192 && v4[1] == 0 && v4[2] == 0) {
			return false
		}
	}
	if ip == metadataV4 || ip == metadataV6 {
		return false
	}
	// These remain forbidden even when an operator permits a broad CIDR.
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if !ip.IsPrivate() && !shared.Contains(ip) && !localNAT64.Contains(ip) {
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
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		return nil, errors.New("invalid backend destination")
	}
	destination = net.JoinHostPort(strings.ToLower(host), port)
	if p.trusted[destination] {
		return []string{destination}, nil
	}
	// Zones scope link-local addresses, which are never permitted. Resolvers
	// keep or drop an unknown zone depending on the host's interfaces.
	if strings.Contains(host, "%") {
		return nil, errors.New("backend destination is not permitted; IPv6 zone identifiers are not supported")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, ErrUnresolved
	}
	out := []string{}
	for _, ip := range ips {
		private, _ := ctx.Value(privateDestination{}).(string)
		cluster := private == destination
		// Internal networks can use RFC1918/ULA or globally numbered site
		// addresses. The admin's HTTP opt-in is scoped to this origin; control
		// destinations remain forbidden regardless of the address class.
		permitted := p.allowed(ip)
		if cluster && ip.Zone() == "" {
			target := Reached(ip)
			permitted = permitted || target.IsPrivate() && target.IsGlobalUnicast() && target != metadataV6
		}
		ip = ip.Unmap()
		if !permitted {
			if cluster {
				return nil, errors.New("internal backend destination is not permitted; loopback, link-local and metadata addresses are blocked")
			}
			return nil, errors.New("backend destination is not permitted; private GPU networks require OLR_BACKEND_ALLOWED_CIDRS")
		}
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	return out, nil
}

func (p *Policy) Check(ctx context.Context, origin string) error {
	return p.CheckBackend(ctx, origin, false)
}

func (p *Policy) CheckBackend(ctx context.Context, origin string, private bool) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("backend URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if p.requireTLS && u.Scheme != "https" && !private {
		return errors.New("production backend requires HTTPS")
	}
	if private {
		ctx = WithPrivateNetwork(ctx, origin)
	}
	_, err = p.addresses(ctx, address(u))
	return err
}

// CheckConfiguredBackend validates configuration without requiring a currently
// resolvable host. DialContext still validates every resolved address before
// traffic, and live probes report unresolved replicas as unhealthy.
func (p *Policy) CheckConfiguredBackend(ctx context.Context, origin string, private bool) error {
	err := p.CheckBackend(ctx, origin, private)
	if errors.Is(err, ErrUnresolved) {
		return nil
	}
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
	var failures []error
	for _, target := range addresses {
		conn, err := dialer.DialContext(ctx, network, target)
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
