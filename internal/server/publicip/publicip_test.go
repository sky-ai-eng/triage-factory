package publicip

import (
	"net/netip"
	"testing"
)

// TestIs pins the SSRF predicate: only globally-routable public unicast is
// allowed.
func TestIs(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"::ffff:8.8.8.8", true}, // v4-mapped public, unmapped before the check
		{"127.0.0.1", false},     // loopback
		{"::1", false},           // loopback
		{"0.0.0.0", false},       // unspecified
		{"10.0.0.5", false},      // RFC1918
		{"192.168.1.10", false},  // RFC1918
		{"172.16.4.2", false},    // RFC1918
		{"169.254.10.1", false},  // link-local
		{"fe80::1", false},       // link-local v6
		{"fd00::1", false},       // unique-local v6
		{"100.64.0.1", false},    // CGNAT
		{"192.0.2.5", false},     // TEST-NET-1
		{"240.0.0.1", false},     // reserved
		{"2001:db8::1", false},   // IPv6 documentation
		{"ff02::1", false},       // multicast
	}
	for _, tc := range cases {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ip, err)
		}
		if got := Is(ip); got != tc.want {
			t.Errorf("Is(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
