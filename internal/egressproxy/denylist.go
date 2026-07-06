package egressproxy

import "net/netip"

// deniedRanges is the operator-owned, customer-immutable internal
// denylist (TFAC-567; sandbox-fleet spec §3.1). The proxy refuses to
// dial any resolved address inside these ranges no matter what the
// allowlist says — today's compiled-in allowlist or the per-profile
// customer allowlists TFAC-408 adds later. It exists because the proxy
// runs on the host, which has more network reach than the sandbox: a
// permissive proxy would be a confused deputy handing the sandbox the
// host's view of cloud metadata, the operator's private network, and
// every sibling run's gateway. Compiled in (not config) so no
// deployment knob can widen it.
//
// Checked against netip.Addr.Unmap()'d addresses (see ipDenied) so an
// IPv4 range can't be smuggled past as its IPv4-mapped-IPv6 form —
// the §3.6 alternate-encodings gate.
var deniedRanges = []netip.Prefix{
	// Loopback — the host's own listeners (TF API, Postgres in some
	// deployments, anything bound to localhost).
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),

	// RFC1918 — the operator's private network, and the sandbox pool
	// 10.42.0.0/16 with every sibling run's gateway inside it.
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),

	// Link-local — the cloud metadata endpoint (169.254.169.254) that
	// hands out the HOST's cloud identity lives here.
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),

	// CGNAT — Fly's private 6PN-adjacent v4 addressing uses this range.
	netip.MustParsePrefix("100.64.0.0/10"),

	// IPv6 unique-local — covers Fly's internal fdaa::/16 network
	// (shared Postgres, Vault, other tenants' Machines).
	netip.MustParsePrefix("fc00::/7"),

	// Unspecified / "this network" — 0.0.0.0 connects to localhost on
	// Linux; deny the whole legacy 0/8 block.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),

	// Multicast + limited broadcast — never legitimate CONNECT targets.
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

// ipDenied reports whether addr falls in the operator denylist. The
// Unmap strips the IPv4-mapped-IPv6 form (::ffff:10.0.0.1 → 10.0.0.1)
// before matching, so the v4 prefixes above apply to a resolver that
// returned the mapped form — without it, a AAAA record of
// ::ffff:169.254.169.254 would sail past every v4 prefix.
func ipDenied(addr netip.Addr) bool {
	a := addr.Unmap()
	for _, p := range deniedRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
