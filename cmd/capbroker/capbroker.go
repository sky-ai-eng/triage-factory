// Package capbroker implements the privilege-separation split described in
// docs/specs/privsep/README.md: a `cap-broker` subcommand that holds the
// host's netns/iptables/cgroup/rootfs capabilities in a process separate
// from the orchestrator, reached over a local unix-socket RPC. Gated
// behind TF_PRIVSEP (default off — see Enabled); off, the orchestrator's
// behavior is byte-identical to before this package existed.
//
// cli.go's dispatchCLI wires the subcommand as its own top-level case,
// deliberately never under `exec` — the agent's Bash allowlist only ever
// names `<selfBin> exec *`, so a sandboxed agent can never invoke
// cap-broker, exactly like the existing hook / snapshot-capture
// separation.
//
// Every substantive file in this package (protocol, client, server,
// socket lifecycle, the orchestrator-side spawn) carries a Linux build
// tag — the privileged operations it brokers (internal/sandbox) are
// Linux-only by construction. This file has no tag so the package still
// compiles on non-Linux dev boxes; Handle degrades to a clear
// "unsupported platform" error there (runbroker_other.go).
package capbroker

import (
	"fmt"
	"os"
	"strings"
)

// Handle is the `triagefactory cap-broker` subcommand entrypoint,
// dispatched from cli.go alongside hook / snapshot-capture — never from
// `exec`. Blocks for the broker process's lifetime.
func Handle(args []string) {
	if err := runBroker(args); err != nil {
		fmt.Fprintln(os.Stderr, "cap-broker:", err)
		os.Exit(1)
	}
}

// Enabled reports whether TF_PRIVSEP is on. Off by default: every
// existing multi-mode deployment keeps today's in-process privileged-ops
// behavior until this is explicitly opted into. Accepts the same boolean
// spellings as the rest of the codebase's TF_* flags (see
// runmode.ParsePreventOrgCreation).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TF_PRIVSEP"))) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}
