// Package capbroker implements the privilege-separation split: a
// `cap-broker` subcommand that holds the host's
// netns/iptables/cgroup/rootfs capabilities in a process separate from the
// orchestrator, reached over a local unix-socket RPC. It is the only
// sandbox launch path on any host that sandboxes (multi mode + Linux); the
// orchestrator's own capabilities are dropped at exec, so nothing that
// parses hostile input or holds credentials retains root-equivalent power.
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
