package server

// Client-address resolution: the single answer to "which IP made this request",
// shared by every consumer that keys on one. It sits in its own file rather
// than beside any of them because the policy below — when X-Forwarded-For may
// be believed at all — is the same policy for all four, and a second
// implementation of it would be a second, quieter policy.

import (
	"net"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// maxXFFHops bounds how many X-Forwarded-For entries clientIP examines when
// walking a trusted proxy's chain right→left. In a proxied deployment the
// header is attacker-controlled, so without a bound a caller could pad it
// with commas to force proportional work on a pre-auth route. A real
// trusted-proxy chain (CDN → LB → app, plus health-check / internal hops) is
// a handful deep; 64 sits far above any sane topology while keeping the walk
// O(1) in the header's length. Past the bound, clientIP falls back to the
// trusted peer (degraded but safe).
const maxXFFHops = 64

// clientIP best-effort extracts the requesting client's IP for the four
// consumers that key on it — the session forensics row (sessions.ip_addr),
// the SOC2 auth audit log (auth_events.ip_address), the pre-auth per-IP rate
// limiters, and an API token's optional CIDR allowlist. All four must agree on
// who the caller is: a token allowlist checked against a different notion of
// the client address than the limiter charges would let one of them be right
// about a request the other was wrong about. The return value is stored as
// Postgres `inet`, so it must be a valid `inet` literal (or "" → NULL);
// net.SplitHostPort unwraps bracketed IPv6 and drops the port so a value like
// `[2001:db8::1]` can't fail the `::inet` cast and 500 the OAuth callback.
//
// X-Forwarded-For is client-*appended* (each proxy adds a hop, none
// overwrites), so its leftmost entry is attacker-controlled. It is trusted
// ONLY when the direct peer is a configured trusted proxy. The policy
// (configured via runmode / TF_TRUSTED_PROXY_CIDR + TF_CAPTURE_CLIENT_IP):
//
//   - Capture disabled → "" (the IP columns store NULL).
//   - No trusted-proxy allowlist, or the peer is not in it (direct
//     exposure) → RemoteAddr only; XFF ignored (forgeable). Secure
//     default: never returns an attacker-chosen value.
//   - Peer IS a trusted proxy → walk XFF right→left, skip trusted hops,
//     return the first untrusted entry (the real client). If every entry
//     is trusted (or XFF is absent), fall back to the peer — degraded but
//     safe.
func clientIP(r *http.Request) string {
	if !runmode.CaptureClientIP() {
		return ""
	}

	peer := remoteHost(r.RemoteAddr)

	trusted := runmode.TrustedProxies()
	if len(trusted) == 0 || !ipInCIDRs(peer, trusted) {
		// No allowlist, or the direct peer isn't a trusted proxy: its XFF
		// is forgeable, so ignore it and attribute the peer itself.
		return peer
	}

	// Peer is a trusted proxy. XFF is append-order (leftmost = original
	// caller, rightmost = nearest hop), so walking right→left past the
	// trusted hops lands on the first IP the chain didn't vouch for — the
	// real client. Anything the caller pre-seeded sits to the LEFT of the
	// address the first trusted proxy appended, so it's never reached.
	//
	// Scan with LastIndexByte rather than strings.Split: the header is
	// attacker-controlled here, and Split would allocate a slice
	// proportional to its comma count — a cheap pre-auth memory/CPU
	// amplifier. This slices one field at a time (substring headers into the
	// original, no copy) from the right, bounded by maxXFFHops, and returns
	// as soon as it finds the client — typically on the first iteration,
	// since the proxy appends the real (untrusted) peer at the far right.
	rest := r.Header.Get("X-Forwarded-For")
	for hops := 0; rest != "" && hops < maxXFFHops; hops++ {
		field := rest
		if i := strings.LastIndexByte(rest, ','); i >= 0 {
			field, rest = rest[i+1:], rest[:i]
		} else {
			rest = ""
		}
		entry := normalizeXFFEntry(field)
		if entry == "" {
			continue // empty / unparseable hop — skip
		}
		if ipInCIDRs(entry, trusted) {
			continue // another trusted proxy in the chain
		}
		return entry // first untrusted from the right → the client
	}

	// Every forwarded hop was trusted (or none present): the closest
	// attributable address is the trusted peer.
	return peer
}

// remoteHost extracts the host portion of a RemoteAddr, unwrapping a
// bracketed IPv6 and dropping the port. Preserves the historical fallback:
// when no port is present (degenerate — RemoteAddr almost always carries
// one in HTTP), the value is returned as-is so a downstream inet cast
// surfaces a clear error rather than a silent empty.
func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// normalizeXFFEntry trims one X-Forwarded-For element to a canonical bare-IP
// string, dropping any port some proxies append and unwrapping a bracketed
// IPv6. Returns "" for an empty or non-IP token so the right→left walk skips
// it rather than treating garbage as a client.
func normalizeXFFEntry(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host // had a port (incl. bracketed "[v6]:port")
	} else {
		s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[") // bare "[v6]"
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return ""
	}
	return ip.String()
}

// ipInCIDRs reports whether ipStr parses to an IP contained in any of nets.
// A non-IP string is never in the set.
//
// IPv4-mapped IPv6 (::ffff:a.b.c.d) is normalized to its 4-byte v4 form
// first. Dual-stack load balancers (nginx on Linux, AWS ALB, and many
// others) deliver connections to Go as the mapped address, so without this
// an operator who sets an IPv4 allowlist (TF_TRUSTED_PROXY_CIDR=10.0.0.0/8)
// for such an LB would silently fail the match — net.IPNet.Contains compares
// equal-length addresses, so a 16-byte mapped peer never matches a 4-byte v4
// net — and XFF would be quietly ignored, collapsing the rate limiter and
// recording the LB in the audit log with no signal why. Genuine cross-family
// pairs (a real v6 peer against a v4 CIDR) still don't match, which is right.
func ipInCIDRs(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
