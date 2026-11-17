package audit

import (
	"bytes"
	"strings"
	"testing"
)

func TestSourceNetwork(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"141.33.4.201", "141.33.4.0/24"},
		{"141.33.4.0/24", "141.33.4.0/24"},
		{"::ffff:141.33.4.201", "141.33.4.0/24"},
		{"2001:db8:abcd:1234::42", "2001:db8:abcd::/48"},
		{"fe80::1234%eth0", "fe80::/48"},
		{"invalid", ""}, {"", ""},
	} {
		if got := SourceNetwork(tc.input); got != tc.want {
			t.Errorf("SourceNetwork(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	var log bytes.Buffer
	Write(&log, Event{Actor: Actor{SourceIP: "141.33.4.201"}})
	if strings.Contains(log.String(), "141.33.4.201") || !strings.Contains(log.String(), "141.33.4.0/24") {
		t.Fatal("audit log retained a precise host address")
	}
}
