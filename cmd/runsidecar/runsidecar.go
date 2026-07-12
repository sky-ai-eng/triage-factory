// Package runsidecar implements the `triagefactory run-sidecar` subcommand:
// the per-run credential-sidecar harness. It is a capless process the
// cap-broker execs at a per-run uid derived from the run's subnet index
// (internal/sandbox.SidecarUID), with its stdio wired to a socket the
// orchestrator dials — the same fd-passthrough pattern the runsc launch
// uses.
//
// Phase 1 ships an inert skeleton on purpose: no credential logic lives
// here yet (that's Phase 2/3 of the epic). This phase only has to start,
// idle, and die correctly, so a later phase can move real work into an
// already-proven process lifecycle. It never runs unsandboxed (local mode,
// or a non-sandboxing host) — only the cap-broker spawns it, and the broker
// only exists on a multi-mode Linux executor.
//
// Deliberately off the `exec` surface (like hook / snapshot-capture /
// cap-broker): a sandboxed agent's Bash allowlist only ever names
// `<selfBin> exec *`, so this subcommand is unreachable from inside a jail
// even if discovered. Undocumented in --help for the same reason.
package runsidecar

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/procname"
)

// commName is this process's kernel-visible name (procname.SetTitle's
// target, /proc/<pid>/comm). Fixed, not per-run — TASK_COMM_LEN truncates
// to 15 usable bytes, too short for a run id, and the boot-time orphan
// sweep (internal/sandbox's reapOrphanSidecars) only needs to recognize
// "this is a sidecar", not which run it belonged to.
const commName = "tf-sidecar"

var sidecarLog = logging.Component("sidecar")

// Handle is the `triagefactory run-sidecar` subcommand entrypoint,
// dispatched from cli.go — never spawned directly by a human or an agent,
// only by the cap-broker's LaunchSidecar RPC handler. Blocks for the
// sidecar's lifetime.
func Handle(args []string) {
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "run-sidecar:", err)
		os.Exit(1)
	}
}

// run is Handle's testable body.
func run(args []string) error {
	fs := flag.NewFlagSet("run-sidecar", flag.ContinueOnError)
	containerID := fs.String("container-id", "", "this sidecar's broker registry key (logging only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	procname.SetTitle(commName)
	logging.SetProcess("sidecar")
	logBootLine(*containerID)

	// Idle until the run ends. The broker's stdio fd-passthrough gives this
	// process a live connection back to the orchestrator — unused for
	// content in Phase 1 (there is no credential or proxy logic here yet),
	// but reading it blocks correctly either way: the orchestrator closing
	// its end (normal teardown) delivers EOF and this returns cleanly, and
	// the broker's KillSidecar (SIGKILL, sent on every teardown path
	// regardless) can't be caught or deferred, so there is no separate
	// signal-handling branch to add for that case. The signal watcher below
	// only covers the case this process ever runs interactively outside the
	// broker's launch (a manual `triagefactory run-sidecar` invocation) —
	// production teardown never depends on it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stdinDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, os.Stdin)
		stdinDone <- err
	}()

	select {
	case err := <-stdinDone:
		return err
	case <-ctx.Done():
		return nil
	}
}

// logBootLine emits one legibility boot line, the same shape cap-broker and
// the orchestrator each log at startup: this process's uid and effective
// capability set, so a security reviewer can confirm "capless, at the
// expected per-run uid" by reading logs instead of trusting the code.
func logBootLine(containerID string) {
	names, err := capinfo.Effective()
	if err != nil {
		sidecarLog.Warn("boot: failed to read own effective capabilities", "error", err)
		return
	}
	sidecarLog.Info("boot",
		"uid", os.Getuid(),
		"CapEff", capinfo.Describe(names),
		"container_id", containerID,
	)
}
