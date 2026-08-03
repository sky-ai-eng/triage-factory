package sandbox

import "strings"

// The per-run socket-root derivation the trusted host-side socket paths in
// this package are all built from: the agenthost socket, the gh-injector
// cert beside it, and the tool-host socket directory.
//
// Platform-independent on purpose. The broker that validates against these
// derivations only ever runs on Linux, but the orchestrator that PRODUCES
// what they name is ordinary cross-platform code — so a derivation parked
// behind a linux build tag compiles for the validator and not for the
// producer, which breaks the build everywhere else the module is built.

// trustedAgentHostSocketRoot mirrors cmd/exec/agenthost's private
// hostSocketRoot constant. Duplicated for the same reason as the
// destination constants in launchspec_linux.go. A var (not const) — like
// cmd/capbroker's brokerSocketPath — so this package's own tests can
// redirect it away from the real, root-owned /run/tf, which a non-root
// `go test` invocation can't create; production never reassigns it.
var trustedAgentHostSocketRoot = "/run/tf"

// sanitizeRunIDForSocket mirrors cmd/exec/agenthost's private
// sanitizeSocketName exactly (character-for-character); a drift test in
// that package cross-checks the two functions agree.
func sanitizeRunIDForSocket(s string) string {
	r := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if len(r) > 64 {
		r = r[:64]
	}
	return r
}
