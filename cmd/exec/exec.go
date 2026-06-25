package exec

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/gh"
	gitexec "github.com/sky-ai-eng/triage-factory/cmd/exec/git"
	jiraexec "github.com/sky-ai-eng/triage-factory/cmd/exec/jira"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/workspace"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// Handle dispatches exec subcommands.
func Handle(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}

	// Open DB for local state (pending reviews, run worktrees, etc.). The
	// open is unconditional even when the sandboxed agenthost path wins
	// below, because the local-mode LocalClient that AutoDetect may return
	// reads this DB for its state and host-side credential resolution. In
	// the sandbox the IPC client ignores it — every state access and every
	// GitHub/Jira credential resolves on the host daemon, not here. No exec
	// subcommand loads a credential from the keychain anymore: gh and jira
	// both route their API calls host-side (Property B — the jail never
	// holds a token, never touches dbus).
	conn, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	// Silence goose's per-invocation logging ("no migrations to run…")
	// — exec runs on every delegated-agent tool call and the noise
	// drowns out the actual command output. Migration errors still
	// surface via the returned error.
	goose.SetLogger(goose.NopLogger())
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		fmt.Fprintf(os.Stderr, "error running migrations: %v\n", err)
		os.Exit(1)
	}
	stores := sqlite.New(conn)

	cmd := args[0]
	cmdArgs := args[1:]

	// AutoDetect returns the right state-access seam for the current
	// process: IPCClient when /run/tf.sock is bind-mounted in (the
	// sandboxed-agent path), LocalClient otherwise (the local-mode CLI
	// and the local-mode delegated-agent path). Help routes skip this
	// because the help output doesn't need run identity resolution.
	buildAgentHost := func() agenthost.Client {
		ctx := context.Background()
		client, derr := agenthost.AutoDetect(ctx, stores)
		if derr != nil {
			// runident-derived errors (env unset, unknown run) get a
			// clean stderr message rather than the wrapping AutoDetect
			// would otherwise apply.
			if errors.Is(derr, runident.ErrRunIdentityMissing) || errors.Is(derr, runident.ErrRunIdentityNotFound) {
				fmt.Fprintln(os.Stderr, derr.Error())
			} else {
				fmt.Fprintf(os.Stderr, "agenthost: %v\n", derr)
			}
			os.Exit(1)
		}
		return client
	}

	switch cmd {
	case "gh":
		// GitHub API calls route through the agenthost client. In the sandbox
		// the IPC client ships each call to the host daemon, which resolves the
		// org's App-installation-or-PAT credential (github.Resolver.ClientForRepo)
		// and makes the request; the jail never reads a token, the keychain, or
		// dbus. In local mode AutoDetect returns the in-process LocalClient,
		// which builds the same client directly on the user's machine — the
		// unchanged local path. Like the jira branch, this never loads a
		// credential here: it resolves host-side (or in-process via the
		// LocalClient), so the DB opened at the top of Handle is consulted on
		// the gh path only by the local-mode LocalClient; the sandbox IPC path
		// ignores it.
		if isHelp(cmdArgs) {
			gh.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		gh.Handle(host, cmdArgs)

	case "jira":
		// Jira API calls route through the agenthost client. In the sandbox
		// the IPC client ships each call to the host daemon, which builds the
		// org's bot-attributed system client (ForSystem) and holds the
		// credential; the jail never reads a token, the keychain, or dbus. In
		// local mode AutoDetect returns the in-process LocalClient, which
		// builds the same ForSystem client directly — the unchanged local
		// path. Bot-authored writes by design; no per-user routing in the
		// sandbox (user-attributed Jira writes are the server-side handlers).
		// The credential resolves host-side (or in-process via the
		// LocalClient), so the DB opened at the top of Handle is consulted
		// here only by the local-mode LocalClient; the sandbox IPC path
		// ignores it.
		if isHelp(cmdArgs) {
			jiraexec.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		jiraexec.Handle(host, cmdArgs)

	case "workspace":
		// No credentials needed — workspace acts on the agenthost client
		// (DB + filesystem in local mode, IPC + filesystem in sandbox).
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		workspace.Handle(host, cmdArgs)

	case "git":
		// No credentials needed — git record-push only upserts an artifact
		// through the agenthost client (DB in local mode, IPC in sandbox).
		// Invoked by the pre-push hook; resolves the run via the same
		// socket/env path as every other exec verb.
		if isHelp(cmdArgs) {
			gitexec.Handle(nil, cmdArgs)
			return
		}
		host := buildAgentHost()
		defer func() { _ = host.Close() }()
		gitexec.Handle(host, cmdArgs)

	default:
		fmt.Fprintf(os.Stderr, "unknown exec command: %s\nRun 'triagefactory exec --help' for usage.\n", cmd)
		os.Exit(1)
	}
}

// HandleStatus processes status update commands from the agent.
func HandleStatus(args []string) {
	fmt.Fprintln(os.Stderr, "not implemented: status")
}

func isHelp(args []string) bool {
	return len(args) == 0 || args[0] == "--help" || args[0] == "-h"
}

func printHelp() {
	// gitexec.HelpText is intentionally omitted here. `git record-push` is
	// internal plumbing the pre-push hook invokes automatically — never a
	// caller-of-intent action. A delegated agent that gets stuck reaches for
	// `exec --help`, and since the agent's Bash allowlist is `exec *` it
	// could then call record-push directly and fabricate/duplicate a branch
	// artifact. Keeping it out of this aggregate menu (it's still documented
	// under `exec git --help` for human operators debugging the hook) keeps
	// the agent-facing surface to real agent actions.
	fmt.Printf("Usage: triagefactory exec <command> [args]\n\n%s\n\n%s\n\n%s\n\nCommands print their result to stdout on success and errors to stderr. Most commands print JSON; workspace add prints a raw path.\n", gh.HelpText, jiraexec.HelpText, workspace.HelpText)
}
