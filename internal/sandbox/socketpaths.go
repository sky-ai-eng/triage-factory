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

// agentHostSocketName and ghInjectorCertName are the files a run's sidecar
// creates inside the run's socket directory. Mirrored by cmd/exec/agenthost,
// which creates them; a drift test there cross-checks the derived paths.
const (
	agentHostSocketName = "agenthost.sock"
	ghInjectorCertName  = "gh-injector.crt"
)

// toolHostSocketDirSuffix names a run's tool-host socket directory beside its
// socket directory. A dot, because sanitizeSocketName maps every dot in a run
// id to a dash: no run's socket directory can be named like another run's
// tool-host directory, and neither can be named like the broker's socket.
const toolHostSocketDirSuffix = ".tools"

// RunSocketDir returns conversationID's per-run socket directory under the
// socket root: the one place its sidecar may create anything. The cap-broker
// creates it for each engagement (PrepareRunSockets), owned by the run's
// sidecar uid with the run's jail gid as its group and no access for anyone
// else, and removes it when the engagement's index is released — so the
// socket root itself stays writable by root alone, and a sidecar holds no
// write access anywhere but here.
func RunSocketDir(conversationID string) string {
	return filepath.Join(trustedAgentHostSocketRoot, sanitizeSocketName(conversationID))
}

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
// The per-run entries are derived from a run id, and the derivations cannot
// land on the broker's socket today: a sanitized name carries no dot, and the
// tool-host suffix is not ".sock". The rule is checked rather than argued from
// those spellings anyway, at every point that removes an entry under this root
// — the broker's per-run prepare and release, and its boot sweep — because a
// removal of the broker's socket takes every subsequent run on the host down
// with it.
func IsReservedSocketRootPath(path string) bool {
	_, reserved := reservedSocketRootBasenames[filepath.Base(path)]
	return reserved
}

// sanitizeSocketName mirrors cmd/exec/agenthost's private
// sanitizeSocketName exactly (character-for-character); a drift test in
// that package cross-checks the two functions agree.
func sanitizeSocketName(s string) string {
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
