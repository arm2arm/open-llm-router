// Package adminaccess implements credentials dedicated to service administration.
package adminaccess

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// Networks validates an explicit allowlist; unrestricted networks are forbidden.
func Networks(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 64 {
		return nil, errors.New("choose 1 to 64 IP addresses or CIDR networks")
	}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		var p netip.Prefix
		if ip, err := netip.ParseAddr(value); err == nil && ip.Zone() == "" {
			ip = ip.Unmap()
			p = netip.PrefixFrom(ip, ip.BitLen())
		} else {
			var err error
			p, err = netip.ParsePrefix(value)
			if err != nil || p.Bits() == 0 || p.Addr().Is4In6() {
				return nil, errors.New("invalid or unrestricted IP network")
			}
			p = p.Masked()
		}
		out = append(out, p.String())
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
func Contains(networks []string, ip netip.Addr) bool {
	for _, value := range networks {
		p, err := netip.ParsePrefix(value)
		if err == nil && p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}
func Subnets(child, parent []string) bool {
	for _, value := range child {
		c, err := netip.ParsePrefix(value)
		if err != nil {
			return false
		}
		ok := false
		for _, value := range parent {
			p, err := netip.ParsePrefix(value)
			if err == nil && p.Addr().BitLen() == c.Addr().BitLen() && p.Bits() <= c.Bits() && p.Contains(c.Addr()) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return len(child) > 0
}

// ClientIP trusts X-Forwarded-For only from explicitly trusted socket peers.
func ClientIP(r *http.Request, trusted []string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, errors.New("invalid peer address")
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return netip.Addr{}, errors.New("invalid peer address")
	}
	peer = peer.Unmap()
	if !Contains(trusted, peer) {
		return peer, nil
	}
	headers := r.Header.Values("X-Forwarded-For")
	if len(headers) != 1 || len(headers[0]) > 4096 {
		return netip.Addr{}, errors.New("trusted proxy must supply one valid X-Forwarded-For chain")
	}
	parts := strings.Split(headers[0], ",")
	if len(parts) > 32 {
		return netip.Addr{}, errors.New("proxy chain too long")
	}
	chain := make([]netip.Addr, len(parts))
	for i, part := range parts {
		ip, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || ip.Zone() != "" {
			return netip.Addr{}, errors.New("invalid proxy chain")
		}
		chain[i] = ip.Unmap()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		peer = chain[i]
		if !Contains(trusted, peer) {
			return peer, nil
		}
	}
	return netip.Addr{}, errors.New("proxy chain has no untrusted client")
}
