// Package workspace implements the `triagefactory exec workspace` CLI
// surface — agent-callable commands for materializing per-repo worktrees
// inside a delegation run.
//
// The flow: a run that starts without a codebase (a Jira or taskless run is
// spawned at the run-root — a throwaway dir holding only _scratch/)
// decides which repo(s) it needs and calls `workspace add <owner/repo>` to
// materialize a worktree. By default the repo's default branch is checked out
// (detached); `--ref <branch>` checks out a named branch and `--pr <N>` checks
// out a PR head (fork-aware). The CLI prints the absolute worktree path; the
// agent `cd`s in and drives git itself (creating its own branch before
// pushing). `workspace list` returns the JSON inventory of worktrees
// materialized so far.
//
// GitHub PR delegations get an eagerly-materialized worktree from the spawner,
// but the surface is run-agnostic (TFAC-498) — it serves any run, not just
// Jira ones.
package workspace

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
)

// HelpText is the help block for `workspace` commands, surfaced both
// from `workspace --help` and the top-level `exec --help`.
const HelpText = `Workspace Commands:
  workspace list                       Print configured + materialized repos for this run (JSON)
  workspace add <owner/repo>           Materialize a worktree on the repo's default branch (detached)
  workspace add <owner/repo> --ref B   ... on an existing branch B (detached at its tip)
  workspace add <owner/repo> --pr N    ... on PR #N's head (fork-aware; no fork credential needed)

Discovery:
  Run 'workspace list' first when you're not sure which repo to materialize.
  Output has two sections:
    - "available": configured repos you could materialize via 'add'
    - "materialized": worktrees you've already added for this run, with
      absolute paths and the checkout each landed on

Usage notes:
  - Conversation id is read from $TRIAGE_FACTORY_CONVERSATION_ID (set by the delegation spawner).
  - The checkout is DETACHED — no branch is prescribed. Create your own
    working branch with 'git checkout -b <name>' before committing; the push
    gate authorizes whatever branch your checkout is on (base/protected refs
    are still refused). Your team's branch-name convention is suggested in
    the run's envelope.
  - 'add' is idempotent per (run, repo): a second 'add' for the same repo
    prints the existing path without re-creating.
  - '--ref' and '--pr' are mutually exclusive.
  - 'add' rejects unconfigured / untracked repos; use 'list' to enumerate
    options before guessing.`

// Handle dispatches workspace subcommands. host is the agenthost.Client
// every DB-touching path routes through (local SQLite or daemon IPC,
// chosen by agenthost.AutoDetect at the top of cmd/exec/exec.go).
func Handle(host agenthost.Client, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}
	switch args[0] {
	case "add":
		runAdd(host, args[1:])
	case "list":
		runList(host, args[1:])
	default:
		exitErr("unknown workspace command: " + args[0])
	}
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec workspace <command> [args]\n\n%s\n", HelpText)
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
