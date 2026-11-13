package adminaccess

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIPTrustBoundary(t *testing.T) {
	for _, tc := range []struct {
		peer, xff string
		trust     []string
		want      string
	}{
		{"192.0.2.4:8000", "203.0.113.7", nil, "192.0.2.4"},
		{"[::ffff:192.0.2.4]:8000", "", nil, "192.0.2.4"},
		{"10.0.0.1:8000", "203.0.113.7, 10.0.0.2", []string{"10.0.0.0/24"}, "203.0.113.7"},
		{"10.0.0.1:8000", "198.51.100.6, 203.0.113.7", []string{"10.0.0.0/24"}, "203.0.113.7"},
		{"10.0.0.1:8000", "", []string{"10.0.0.0/24"}, ""},
		{"10.0.0.1:8000", "malformed, 203.0.113.7", []string{"10.0.0.0/24"}, ""},
		{"10.0.0.1:8000", "10.0.0.2", []string{"10.0.0.0/24"}, ""},
	} {
		r := httptest.NewRequest("POST", "http://router", nil)
		r.RemoteAddr = tc.peer
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		ip, err := ClientIP(r, tc.trust)
		if tc.want == "" {
			if err == nil {
				t.Fatal("accepted invalid chain")
			}
		} else if err != nil || ip.String() != tc.want {
			t.Fatalf("got %v %v want %s", ip, err, tc.want)
		}
	}
}
func TestNetworksAndDelegation(t *testing.T) {
	for _, values := range [][]string{nil, {"0.0.0.0/0"}, {"::/0"}, {"bad"}, {"fe80::1%en0"}, {"::ffff:192.0.2.0/120"}} {
		if _, err := Networks(values); err == nil {
			t.Fatalf("accepted invalid networks %v", values)
		}
	}
	normalized, err := Networks([]string{"192.0.2.1", "192.0.2.1/32", "2001:db8::9/64"})
	if err != nil || len(normalized) != 2 {
		t.Fatal("normalization", normalized, err)
	}
	if !Contains(normalized, netip.MustParseAddr("::ffff:192.0.2.1")) {
		t.Fatal("mapped IPv4 failed")
	}
	if !Subnets([]string{"192.0.2.1/32"}, []string{"192.0.2.0/24"}) || Subnets([]string{"192.0.0.0/16"}, []string{"192.0.2.0/24"}) {
		t.Fatal("delegation expanded authority")
	}
}
