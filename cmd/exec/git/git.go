// Package git implements the `triagefactory exec git` CLI surface — the
// agent-callable verbs that record durable artifacts for raw `git`
// operations the other exec CLIs don't mediate.
//
// Today that is exactly one verb: `git record-push`, invoked by the
// TF-controlled `pre-push` git hook (F2's hooks dir, A·3/TFAC-460). A
// branch pushed with raw `git push` — and never opened as a PR — is
// otherwise invisible to TF; the hook hands each pushed ref to this verb,
// which upserts a `branch` artifact host-side (so RunInfo + Stores are in
// scope, the same choke point gh/jira route through).
//
// Best-effort by contract: the hook swallows this verb's failures and
// always exits 0, so a record failure never blocks the agent's push.
package git

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// HelpText is the help block for `git` commands, surfaced both from
// `git --help` and the top-level `exec --help`.
const HelpText = `Git Commands:
  git record-push --remote <url> --ref <remote-ref> --sha <local-sha> --new <bool>
      Record a pushed branch as a durable artifact. Invoked by the TF
      pre-push hook for each ref a 'git push' carries; not meant to be
      called by hand. Best-effort — failures are reported on stderr but
      never block the push.`

// Handle dispatches git subcommands. host is the agenthost.Client the
// record path routes through (local SQLite or daemon IPC, chosen by
// agenthost.AutoDetect at the top of cmd/exec/exec.go). host is nil on
// the help route, which returns before any call.
func Handle(host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}
	switch args[0] {
	case "record-push":
		runRecordPush(host, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown git command: %s\n", args[0])
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec git <command> [flags]\n\n%s\n", HelpText)
}
