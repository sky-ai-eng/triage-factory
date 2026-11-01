// Package publicip decides whether an address is reachable over the public
// Internet. It is the one SSRF predicate behind the avatar proxy's dial-time
// guard and the GitHub App manifest's webhook reachability check, so the two
// cannot disagree about what counts as public.
package publicip

import "net/netip"

// nonPublicPrefixes are address ranges that are not reachable over the
// public Internet but that IsGlobalUnicast / IsPrivate don't already
// exclude: carrier-grade NAT, documentation/test ranges, benchmarking,
// and the reserved-for-future block.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT / shared
	netip.MustParsePrefix("192.0.2.0/24"),    // RFC 5737 TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"), // RFC 5737 TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // RFC 5737 TEST-NET-3
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC 2544 benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),     // RFC 1112 reserved (future use)
	netip.MustParsePrefix("2001:db8::/32"),   // RFC 3849 documentation
}

// Is reports whether ip is a globally-routable public unicast address —
// outside loopback/link-local/multicast/unspecified (IsGlobalUnicast), RFC
// 1918 / ULA (IsPrivate), and the CGNAT/doc/benchmark/reserved ranges in
// nonPublicPrefixes.
func Is(ip netip.Addr) bool {
	ip = ip.Unmap()
	// IsGlobalUnicast rejects loopback, unspecified, multicast, and link-local
	// in one predicate; IsPrivate adds RFC 1918 / unique-local.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
