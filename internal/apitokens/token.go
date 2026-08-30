package apitokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Wire format of an API token: "tf_" followed by 32 crypto/rand bytes in
// unpadded base64url (43 characters), for 46 characters total. The recipe is
// the invite token's, one entropy budget for every bearer secret TF mints.
//
// The stored hash covers the FULL literal, "tf_" included, so a hash is only
// ever matched against something that was already shaped like a token. There is
// no constant-time compare on the way in: the lookup is an equality probe on a
// unique index over a 256-bit value, and an attacker who could grind that
// timing would need the hash itself, not the token.
const (
	// Prefix is the literal every token starts with. Exported so a caller can
	// recognize a token by shape, without spending a lookup on a string that
	// was never one.
	Prefix = "tf_"

	// secretBytes is the entropy behind the base64url body.
	secretBytes = 32

	// prefixLen is how much of the plaintext token_prefix keeps: "tf_" plus
	// eight body characters. Enough for a human to tell two of their own
	// tokens apart in a list, far short of anything that narrows a search of
	// the remaining 35.
	prefixLen = len(Prefix) + 8

	// MaxPerUserOrg bounds how many live tokens one user may hold in one org.
	// It is a blast-radius bound, not a resource limit — rotation needs an
	// overlap window, automation needs a token per deployment, and past that a
	// growing pile is a sign nobody is deleting what they stopped using.
	MaxPerUserOrg = 50

	// MaxAllowedCIDRs is the largest IP allowlist a token may carry. Mirrors
	// the table's cardinality CHECK, which is the real enforcement.
	MaxAllowedCIDRs = 20
)

// ErrInvalidCIDR reports an allowlist entry that is not a canonical IP range.
// It is a bad-field fault, not a server error: the value came from whoever
// asked for the token.
var ErrInvalidCIDR = errors.New("allowed_cidrs entry is not a valid IP range")

// ErrTooManyCIDRs reports an allowlist longer than MaxAllowedCIDRs.
var ErrTooManyCIDRs = fmt.Errorf("a token may carry at most %d allowed CIDRs", MaxAllowedCIDRs)

// generate mints one token: the plaintext the caller hands over exactly once,
// its sha256 (what the row stores), and the display prefix.
func generate() (plaintext string, hash []byte, prefix string, err error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, "", fmt.Errorf("generate token: %w", err)
	}
	plaintext = Prefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:], plaintext[:prefixLen], nil
}

// hashOf is generate's read side: the value LookupSystem probes the unique
// index with.
func hashOf(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// normalizeCIDRs parses an allowlist into the canonical textual form the cidr
// column stores, or returns nil for an empty list — a token with no allowlist
// stores NULL, and an empty array would be a token that can never be used.
//
// A bare address is accepted and read as a single host (the /32 or /128 the
// column would store anyway), because "let this one machine in" is the common
// case and making someone spell the mask would only invite them to guess at it.
// A prefix with bits set below its mask is REFUSED rather than masked: 10.0.0.1/8
// is either a typo for a host or a typo for a network, and silently picking one
// is how an allowlist ends up admitting sixteen million addresses nobody meant.
func normalizeCIDRs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAllowedCIDRs {
		return nil, ErrTooManyCIDRs
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, fmt.Errorf("%w: empty entry", ErrInvalidCIDR)
		}
		if !strings.Contains(s, "/") {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("%w: %q", ErrInvalidCIDR, raw)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()).String())
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidCIDR, raw)
		}
		if p.Masked() != p {
			return nil, fmt.Errorf("%w: %q has bits set below its mask (did you mean %s or %s?)",
				ErrInvalidCIDR, raw, p.Masked(), netip.PrefixFrom(p.Addr(), p.Addr().BitLen()))
		}
		out = append(out, p.String())
	}
	return out, nil
}
