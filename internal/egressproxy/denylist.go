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

// nat64Prefix is the well-known DNS64/NAT64 translation prefix
// (RFC 6052): an address inside it embeds an IPv4 host in its low 32
// bits, and a NAT64 gateway will deliver the connection to that v4
// host. A DNS64 resolver can therefore hand back
// 64:ff9b::a9fe:a9fe — the metadata endpoint wearing a public-looking
// v6 coat that no prefix above matches. We don't deny the whole
// prefix (that would fail closed for legitimate registry traffic on a
// NAT64-only network); instead ipDenied extracts the embedded v4 and
// applies the denylist to it, mirroring the Unmap treatment of
// ::ffff: mapped forms. Typical Fly/cloud-VM resolvers don't do DNS64
// synthesis, so this is defense-in-depth, not a live hole.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// ipDenied reports whether addr falls in the operator denylist. Two
// v4-in-v6 encodings are normalized before matching so the v4 ranges
// can't be smuggled past in a v6 coat (§3.6 alternate-encodings gate):
// Unmap strips the IPv4-mapped form (::ffff:10.0.0.1 → 10.0.0.1), and
// NAT64 addresses (64:ff9b::/96) are additionally judged by their
// embedded v4 host.
func ipDenied(addr netip.Addr) bool {
	a := addr.Unmap()
	for _, p := range deniedRanges {
		if p.Contains(a) {
			return true
		}
	}
	if nat64Prefix.Contains(a) {
		raw := a.As16()
		embedded := netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]})
		for _, p := range deniedRanges {
			if p.Contains(embedded) {
				return true
			}
		}
	}
	return false
}
