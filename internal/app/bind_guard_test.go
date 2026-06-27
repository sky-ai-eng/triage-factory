package app

import (
	"strings"
	"testing"
)

// TestAssertLocalBindSafe pins the TFAC-409 item-1 guardrail: in local mode
// (zero auth) a loopback bind is fine, a non-loopback bind or a non-local
// TF_PUBLIC_URL refuses to boot, and the explicit override downgrades the
// refusal to a (logged) pass-through.
func TestAssertLocalBindSafe(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		publicURL string
		override  string
		wantErr   bool
	}{
		{"loopback ipv4", "127.0.0.1", "", "", false},
		{"loopback ipv4 in /8", "127.0.0.5", "", "", false},
		{"loopback ipv6", "::1", "", "", false},
		{"localhost name", "localhost", "", "", false},
		{"localhost public url ok", "127.0.0.1", "http://localhost:3000", "", false},

		{"all interfaces empty host", "", "", "", true},
		{"all interfaces 0.0.0.0", "0.0.0.0", "", "", true},
		{"all interfaces ipv6", "::", "", "", true},
		{"specific lan ip", "192.168.1.20", "", "", true},
		{"dns hostname", "tf.internal.example.com", "", "", true},
		{"loopback host but public url remote", "127.0.0.1", "https://tf.example.com", "", true},

		{"override allows non-loopback", "0.0.0.0", "", "true", false},
		{"override allows remote public url", "127.0.0.1", "https://tf.example.com", "1", false},
		{"override typo still fails closed", "0.0.0.0", "", "ture", true},
		{"override false still fails", "0.0.0.0", "", "false", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertLocalBindSafe(tc.host, tc.publicURL, tc.override)
			if tc.wantErr != (err != nil) {
				t.Fatalf("assertLocalBindSafe(%q,%q,%q) err=%v, wantErr=%v",
					tc.host, tc.publicURL, tc.override, err, tc.wantErr)
			}
			// The refusal must name the override env so the operator knows the
			// escape hatch without reading the source.
			if err != nil && !strings.Contains(err.Error(), envAllowPublicLocal) {
				t.Errorf("refusal error should mention %s, got %q", envAllowPublicLocal, err.Error())
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.255.255.254", "::1", "localhost", "LocalHost", " 127.0.0.1 "}
	notLoopback := []string{"", "0.0.0.0", "::", "192.168.0.1", "10.0.0.1", "example.com", "1.2.3.4"}

	for _, h := range loopback {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range notLoopback {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}
