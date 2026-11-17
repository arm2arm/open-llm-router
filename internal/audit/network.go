package audit

import (
	"net/netip"
	"strings"
)

// SourceNetwork removes host bits before administration attribution is stored
// or logged. Network authorization still evaluates the original address.
func SourceNetwork(value string) string {
	value = strings.TrimSpace(value)
	addr, err := netip.ParseAddr(value)
	if err != nil {
		prefix, prefixErr := netip.ParsePrefix(value)
		if prefixErr != nil {
			return ""
		}
		addr = prefix.Addr()
	}
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	return netip.PrefixFrom(addr, bits).Masked().String()
}
