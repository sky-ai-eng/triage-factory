package sandbox

import (
	"path/filepath"
	"strings"
)

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

// reservedSocketRootBasenames are the entries under the socket root that are
// NOT per-run state and must survive every per-run sweep. Today that is the
// cap-broker's control socket, which shares this directory with the per-run
// sockets (see cmd/capbroker's socketDir) and is the executor's single point
// of failure: nothing launches a jail, a sidecar or a network without it.
//
// The literal is duplicated from cmd/capbroker rather than imported — that
// package imports this one, so the dependency cannot run the other way — and
// a drift test over there pins the two spellings together, exactly as the
// destination constants in launchspec_linux.go are pinned.
var reservedSocketRootBasenames = map[string]struct{}{
	"cap-broker.sock": {},
}

// IsReservedSocketRootPath reports whether path names an entry under the
// per-run socket root that no per-run cleanup may remove.
//
// It exists because the per-run paths are DERIVED from a run id, and the
// sanitizer that derives them maps any input onto a flat name in this one
// directory — so a run id of "cap-broker" derives onto the broker's own
// control socket. Nothing reachable produces that today (run ids are
// conversation UUIDs), and the socket root's sticky bit already stops every
// SIDECAR from touching it. What it does not stop is the orchestrator, which
// owns that directory and is the process that runs the per-run sweeps: for it,
// the collision would be a permitted unlink of the broker's socket, taking
// every subsequent run on the host down with it.
//
// So the rule is checked rather than argued from the id format, and it belongs
// at every point that unlinks a run-id-derived path under this root, not only
// at the sweeps that consult it today.
func IsReservedSocketRootPath(path string) bool {
	_, reserved := reservedSocketRootBasenames[filepath.Base(path)]
	return reserved
}

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
