package egressproxy

import (
	"context"
	"strings"
	"testing"
)

// TestVettedDialContext_RefusesInternalTargets pins the gate host-side
// fetchers depend on. egressrelay follows redirects with the HOST's network
// reach, so the addresses below are exactly what a hostile or compromised
// upstream would redirect at: the cloud metadata endpoint that hands out the
// host's identity, the operator's private network, the sandbox pool, and the
// host's own listeners. Each must be refused before a connection is made.
//
// IP literals rather than hostnames so the test asserts the denylist, not
// the resolver, and needs no network.
func TestVettedDialContext_RefusesInternalTargets(t *testing.T) {
	for _, addr := range []string{
		"169.254.169.254:80",    // cloud metadata
		"127.0.0.1:8080",        // host's own listeners
		"10.42.0.1:443",         // sandbox pool / sibling gateway
		"192.168.1.1:443",       // operator private network
		"172.16.0.5:443",        // operator private network
		"[::1]:443",             // loopback, v6
		"[::ffff:10.0.0.1]:443", // v4-mapped-v6 smuggling of an RFC1918 address
	} {
		conn, err := VettedDialContext(context.Background(), "tcp", addr)
		if err == nil {
			conn.Close()
			t.Errorf("VettedDialContext(%q) succeeded; the internal denylist must refuse it", addr)
			continue
		}
		if !strings.Contains(err.Error(), "internal/denied") {
			t.Errorf("VettedDialContext(%q) error = %v; want the policy denial, not a transport error", addr, err)
		}
	}
}

// TestVettedDialContext_RejectsMalformedTarget keeps a bare hostname (no
// port) from reaching the resolver as if it were a host:port pair.
func TestVettedDialContext_RejectsMalformedTarget(t *testing.T) {
	if _, err := VettedDialContext(context.Background(), "tcp", "proxy.golang.org"); err == nil {
		t.Error("VettedDialContext with no port succeeded; want a parse error")
	}
}
