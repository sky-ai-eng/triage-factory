package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	"github.com/sky-ai-eng/triage-factory/cmd/gitssh"
	"github.com/sky-ai-eng/triage-factory/cmd/hook"
	"github.com/sky-ai-eng/triage-factory/cmd/install"
	"github.com/sky-ai-eng/triage-factory/cmd/instance"
	"github.com/sky-ai-eng/triage-factory/cmd/jwkinit"
	"github.com/sky-ai-eng/triage-factory/cmd/migrate"
	"github.com/sky-ai-eng/triage-factory/cmd/operator"
	"github.com/sky-ai-eng/triage-factory/cmd/runsidecar"
	"github.com/sky-ai-eng/triage-factory/cmd/snapshotcapture"
	"github.com/sky-ai-eng/triage-factory/cmd/uninstall"
)

// dispatchCLI runs the argv-dispatched subcommands for a process on a real
// host — the host-cli boot identity, plus the server identity's fall-through.
// args is the argument slice with the program name stripped (os.Args[1:], after
// the applet's implicit `exec` prefix is applied — see resolveCLIArgs). It
// returns handled=true when args named a subcommand — the caller should return
// err and exit — and handled=false to fall through to server mode.
//
// This is the full surface, including everything that needs local state or host
// capabilities. A process inside a run sandbox never reaches it; see
// dispatchJailCLI for the narrow surface that does apply there.
//
// The subcommands manage their own exit; they're listed here purely to
// route the two audiences (delegated Claude Code agents vs. human users)
// before the heavier server boot.
func dispatchCLI(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "exec":
		exec.Handle(args[1:])
	case "hook":
		// Internal git-hook callbacks (e.g. `hook record-push`), fired by
		// the TF-controlled git hooks. Deliberately a separate namespace
		// from `exec`: the agent's Bash allowlist is `exec *`, so keeping
		// these off `exec` makes them un-invokable by the agent while the
		// hook (a git subprocess, not an agent tool call) can still reach
		// them. Undocumented in --help for the same reason.
		hook.Handle(args[1:])
	case "status":
		exec.HandleStatus(args[1:])
	case "git-ssh":
		// Internal: the GIT_SSH_COMMAND dispatcher a managed local run's git
		// execs in place of ssh, bridging org-host sessions onto the run's git
		// proxy and passing every other host through to real ssh. git chooses
		// when to run it, with git-generated argv — never a human or the
		// agent. Off the `exec` namespace and undocumented in --help for the
		// same reason as `hook`.
		gitssh.Handle(args[1:])
	case "snapshot-capture":
		// Internal: run a parked run's git-delta capture, emitting the delta as
		// JSON. Spawned by the delegate spawner as the sandbox uid so the
		// capture's filter-honoring git never runs as root over agent-writable
		// config. Undocumented in --help, like `hook`.
		snapshotcapture.Handle(args[1:])
	case "cap-broker":
		// Internal: the privilege-separation broker. Holds the host's
		// netns/iptables/cgroup/rootfs capabilities in a process
		// separate from the orchestrator; only spawned by the orchestrator
		// itself (never by a delegated agent). Deliberately off the agent
		// `exec` allowlist, same separation as `hook` / `snapshot-capture` —
		// a sandboxed `Bash(<bin> exec *)` can never reach it. Undocumented
		// in --help, like the others.
		capbroker.Handle(args[1:])
	case "run-sidecar":
		// Internal: the per-run credential-sidecar harness. Broker-spawned
		// only — the cap-broker execs this at a per-run uid it derives,
		// never the orchestrator or a delegated agent. Same off-`exec`
		// separation as cap-broker. Undocumented in --help, like the others.
		runsidecar.Handle(args[1:])
	case "install":
		install.Handle(args[1:])
	case "uninstall":
		uninstall.Handle(args[1:])
	case "migrate":
		migrate.Handle(args[1:])
	case "jwk-init":
		jwkinit.Handle(args[1:])
	case "instance":
		instance.Handle(args[1:])
	case "operator":
		operator.Handle(args[1:])
	case "-h", "--help", "help":
		printTopLevelHelp()
	case "-v", "--version", "version":
		fmt.Println(Version)
	default:
		// A leading flag (e.g. --port) is server-mode config that LoadConfig
		// parses, so fall through. Anything else is a mistyped subcommand —
		// surface it instead of silently booting the server.
		if strings.HasPrefix(args[0], "-") {
			return false, nil
		}
		return true, fmt.Errorf("unknown subcommand %q; run 'triagefactory --help' for usage", args[0])
	}
	return true, nil
}

// dispatchJailCLI is the whole subcommand surface of the jail-cli boot
// identity. It runs the exec verbs and prints help; every other subcommand is
// refused, because none of them can work in a jail and each would fail in a
// more confusing way than a refusal. install/uninstall/migrate/jwk-init/
// instance/operator all reach for local state that isn't there (and would
// create some by reaching), a leading flag means server mode, and the internal
// namespaces — hook, snapshot-capture, cap-broker, run-sidecar — are all
// host-side callbacks the jail never legitimately invokes.
//
// Refusing rather than attempting is what keeps the missing machinery
// structural: nothing on this path can construct db.Stores, so a jailed
// invocation cannot mint a state directory inside the agent's worktree no
// matter which subcommand it names.
//
// The agenthost client is dialed HERE, at boot, before any subcommand logic
// runs — a jail with no exec-verb socket has nothing that can serve a verb, so
// the fail-closed error belongs at the door rather than several frames into a
// verb body. The cost is that a verb's own `--help` also needs the socket in a
// jail; help text is not worth pretending the CLI is functional when it isn't.
func dispatchJailCLI(args []string) error {
	// No subcommand at all means server mode, which is the one thing a jail
	// must least be able to boot. Identity resolution never routes that shape
	// here today (it resolves to the server identity), so this is the same kind
	// of guard dispatchCLI's own empty check is: the function must answer for
	// its whole input domain, not for what its current caller happens to pass.
	if len(args) == 0 {
		return errNotAvailableInSandbox("server mode")
	}

	switch args[0] {
	case "exec":
		// Fall through to the client below.
	case "status":
		// Needs no client (and no state) today.
		exec.HandleStatus(args[1:])
		return nil
	case "-h", "--help", "help":
		printTopLevelHelp()
		return nil
	case "-v", "--version", "version":
		fmt.Println(Version)
		return nil
	default:
		return errNotAvailableInSandbox(args[0])
	}

	client, err := agenthost.DialSandbox(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	exec.HandleSandboxed(client, args[1:])
	return nil
}

// errNotAvailableInSandbox is the jailed CLI's refusal. It names what was asked
// for and then the one command that lists what CAN be run, under the prefix
// this process was actually invoked as — a reader who hits this has already
// guessed wrong once, and a refusal that only says no costs another turn.
func errNotAvailableInSandbox(what string) error {
	return fmt.Errorf("%s is not available inside a run sandbox; only the exec verbs are (run %q for the list)",
		what, prog.Prefix()+" --help")
}

// printTopLevelHelp routes the two audiences (delegated Claude Code agents
// vs. human users) to the right surface. Agents almost always reach this
// through accidental invocation when trying to run a scoped subcommand, so
// the first thing they see is the `exec` pointer; humans typically want the
// server flags.
func printTopLevelHelp() {
	fmt.Println(`triagefactory — local-first AI triage for engineering backlogs.

Run with no arguments to start the server (port 3000, opens browser).

USER COMMANDS
  triagefactory                            start the server
  triagefactory --port N                   start on a custom port
  triagefactory --host <addr>              bind address (default 127.0.0.1;
                                           use 0.0.0.0 for LAN access)
  triagefactory --no-browser               start without opening a browser
  triagefactory --version                  print the binary's version
  triagefactory install [--dest <path>]    symlink the binary onto PATH
  triagefactory uninstall [--yes]          wipe local state (db, config,
                                           keychain); leaves the binary
                                           itself in place
  triagefactory migrate up                 bring the schema to head
  triagefactory migrate status             list applied + pending migrations
  triagefactory instance list              show every registered fleet instance
  triagefactory instance drain <id>        stop new claims, quiesce
  triagefactory instance undrain <id>      resume claims
  triagefactory instance placement ...     inspect / steer run placement
  triagefactory operator add <email>       grant the fleet-console operator flag
  triagefactory operator remove <email>    revoke it
  triagefactory operator list              show deployment operators

AGENT COMMANDS
  Used by delegated Claude Code agents inside their worktree, not
  meant for direct invocation by humans.

  triagefactory exec <subcommand> ...      scoped GitHub / Jira ops
                                           (run "triagefactory exec --help"
                                           for the full list)
  triagefactory status <run-id>            check a delegated run's status

For configuration, polling, and feature details, see docs/local-mode/.`)
}
