package main

import (
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/install"
	"github.com/sky-ai-eng/triage-factory/cmd/jwkinit"
	"github.com/sky-ai-eng/triage-factory/cmd/migrate"
	"github.com/sky-ai-eng/triage-factory/cmd/resume"
	"github.com/sky-ai-eng/triage-factory/cmd/uninstall"
)

// dispatchCLI runs the argv-dispatched subcommands. args is the argument
// slice with the program name stripped (os.Args[1:]). It returns
// handled=true when args named a subcommand — the caller should return err
// and exit — and handled=false to fall through to server mode.
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
	case "status":
		exec.HandleStatus(args[1:])
	case "resume":
		resume.Handle(args[1:])
	case "install":
		install.Handle(args[1:])
	case "uninstall":
		uninstall.Handle(args[1:])
	case "migrate":
		migrate.Handle(args[1:])
	case "jwk-init":
		jwkinit.Handle(args[1:])
	case "-h", "--help", "help":
		printTopLevelHelp()
	case "-v", "--version", "version":
		fmt.Println(Version)
	default:
		return false, nil
	}
	return true, nil
}

// printTopLevelHelp routes the two audiences (delegated Claude Code agents
// vs. human users) to the right surface. Agents almost always reach this
// through accidental invocation when trying to run a scoped subcommand, so
// the first thing they see is the `exec` pointer; humans typically want the
// server flags and the takeover-resume shortcuts.
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
                                           keychain, takeovers); leaves
                                           the binary itself in place
  triagefactory resume [<short-id>]        resume a taken-over session
                                           (auto-resumes when there's only
                                           one; picker otherwise)
  triagefactory migrate up                 bring the schema to head
  triagefactory migrate status             list applied + pending migrations

AGENT COMMANDS
  Used by delegated Claude Code agents inside their worktree, not
  meant for direct invocation by humans.

  triagefactory exec <subcommand> ...      scoped GitHub / Jira ops
                                           (run "triagefactory exec --help"
                                           for the full list)
  triagefactory status <run-id>            check a delegated run's status

For configuration, polling, and feature details, see docs/usage.md.`)
}
