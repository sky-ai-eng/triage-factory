// Package hook implements the internal git-hook callbacks the TF-controlled
// git hooks (F2's hooks dir) invoke around an agent's raw git operations:
// `hook record-push` (A·3, TFAC-460), which records each pushed branch as a
// durable artifact, and `hook check-push`, which answers whether the team's
// base-branch push policy permits the push about to happen — the only verb
// here that can refuse, and local mode's enforcement point for that policy.
//
// These are deliberately NOT part of the `triagefactory exec` surface. They
// are machine-to-host callbacks fired by a git hook with git-generated
// arguments — never something a human or the agent invokes by hand. Keeping
// them off `exec` also keeps them off the delegated agent's Bash allowlist,
// which permits `Bash(<bin> exec *)` but nothing under `hook`, so the agent
// cannot call them even if it discovered them (the hook itself runs as a git
// subprocess, not an agent tool call, so it is unaffected by that allowlist).
// They appear in no `--help` output for the same reason.
//
// Like the exec subcommands, the host-side work routes through agenthost
// (the per-run IPC socket in the sandbox, an in-process LocalClient locally),
// so RunInfo + Stores are in scope and credentials never enter the jail.
package hook

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/pressly/goose/v3"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// Handle dispatches `triagefactory hook <verb> ...`. Recording verbs are
// best-effort by contract: the calling git hook swallows their failures, so a
// recording miss never blocks the push. A bug in our own hook (unknown verb,
// missing verb) exits non-zero to surface it; operational failures (no run
// context, DB/daemon trouble) warn and return so the verb stays best-effort.
//
// check-push is the one verb that can refuse, and it says so with the
// dedicated ExitRefused status — never with the exit codes above, which the
// pre-push hook deliberately treats as "allow" so a bug or an outage here can
// only fail open.
func Handle(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "triagefactory hook: internal git-hook callbacks; not for manual use")
		os.Exit(2)
	}
	switch args[0] {
	case "record-push":
		host, _, conn, ok := buildHost()
		if !ok {
			return // operational setup failure already warned; best-effort
		}
		defer func() { _ = host.Close() }()
		defer func() { _ = conn.Close() }()
		runRecordPush(host, args[1:])
	case "check-push":
		// The one verb that can REFUSE. Everything else here is a recorder; this
		// is the local-mode enforcement point for the team's base-branch push
		// policy (multi mode enforces it at the git proxy's ref gate instead).
		// A setup failure allows the push: the guard exists to catch a mistaken
		// agent, and one that blocks pushes whenever the local DB is unhappy
		// turns a convenience into a broken tool.
		host, stores, conn, ok := buildHost()
		if !ok {
			return // operational setup failure already warned; allow the push
		}
		code := runCheckPush(host, stores, args[1:], os.Stdin)
		_ = host.Close()
		_ = conn.Close()
		if code != 0 {
			os.Exit(code)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown hook verb: %s\n", args[0])
		os.Exit(2)
	}
}

// buildHost opens the local SQLite state, migrates it, and resolves the
// agenthost client the same way cmd/exec does: AutoDetect returns the IPC
// client when the per-run socket is bind-mounted (sandbox) and an in-process
// LocalClient otherwise (local mode). Returns ok=false on any setup failure
// with a warning on stderr — best-effort, never fatal to the push. The conn
// is returned so the caller closes it after the verb runs (the LocalClient
// reads it; the IPC client ignores it).
//
// The stores are returned alongside for the verbs that read policy directly
// rather than through a host round-trip (check-push). Those reads are
// local-mode-only by construction: under a sandbox the pre-push hook stands
// down before invoking any verb, and there is no DB in the jail to open.
func buildHost() (agenthost.Client, db.Stores, *sql.DB, bool) {
	conn, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook: open database: %v\n", err)
		return nil, db.Stores{}, nil, false
	}
	// Silence goose's per-invocation logging — the hook fires on every push
	// and the migration noise would drown the agent's git output.
	goose.SetLogger(goose.NopLogger())
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "hook: run migrations: %v\n", err)
		return nil, db.Stores{}, nil, false
	}
	stores := sqlite.New(conn)
	client, derr := agenthost.AutoDetect(context.Background(), stores)
	if derr != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "hook: agenthost: %v\n", derr)
		return nil, db.Stores{}, nil, false
	}
	return client, stores, conn, true
}
