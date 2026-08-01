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
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/procname"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

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

	procname.SetTitle(sandbox.SidecarCommName)
	logging.SetProcess("sidecar")
	logBootLine(*containerID)

	// Mint this run's X25519 keypair — the private half is born here and
	// never leaves the process; the public half is announced to the
	// orchestrator so the brain seals this run's bundle to it. A generation
	// failure is fatal: without a keypair the sidecar can never unseal, so
	// there is nothing to idle for.
	rt, err := newCredRuntime()
	if err != nil {
		return fmt.Errorf("mint per-run keypair: %w", err)
	}

	// The run's shared fake-GHE origin listener, bound by the broker on a
	// privileged port this capless process cannot bind itself and passed down as
	// an extra descriptor. Adopted here rather than in startProxies so its
	// address is known before the proxies build their git config: the sandbox's
	// git has to be routed at this very host for gh's repo inference to resolve.
	// Absent (an older broker, or a bind that failed) leaves the gh channel on an
	// ephemeral port of its own, which costs inference and nothing else.
	rt.setSharedOriginListener(adoptSharedOriginListener())

	// The supervision channel: the broker wired this process's stdin+stdout
	// to a single duplex socket whose other end the orchestrator holds. Serve
	// the credential protocol over it — the orchestrator relays the sealed
	// bundle down and asks for proxies; the runtime answers, and calls back
	// for git-authorize decisions.
	conn := sidecarproto.New(stdioChannel{}, rt)
	rt.setConn(conn)

	// Announce the public key first, before processing any request, so the
	// orchestrator can drive the seal before it asks the sidecar to do
	// anything. A write failure here means the channel is already dead —
	// nothing to serve, exit cleanly.
	if err := conn.Notify(sidecarproto.KindHello, rt.helloBody()); err != nil {
		return nil
	}

	// Block until the channel closes (the orchestrator's normal teardown
	// delivers EOF) or a signal arrives. The broker's KillSidecar (SIGKILL,
	// sent on every teardown path) can't be caught, so the signal watcher
	// only covers a manual `triagefactory run-sidecar` invocation outside the
	// broker — production teardown never depends on it. On the way out, drain
	// the proxies cleanly (process exit would free them regardless).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-conn.Done():
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt.shutdown(shutdownCtx)
	return nil
}

// stdioChannel adapts this process's stdin (read) + stdout (write) into one
// io.ReadWriteCloser for the supervision Conn. The broker wired both to the
// same underlying socket (LaunchSidecarProcess sets cmd.Stdin = cmd.Stdout =
// stdio), so reads and writes hit the one duplex connection to the
// orchestrator. Close is a no-op: the broker owns the fd's lifetime and
// SIGKILLs the process at teardown; closing os.Stdout here would only race
// that.
type stdioChannel struct{}

func (stdioChannel) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioChannel) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioChannel) Close() error                { return nil }

// adoptSharedOriginListener turns the descriptor the broker passed at
// sandbox.SharedOriginListenerFD into a listener this process owns. Returns nil
// when nothing was passed there, which is the only signal available — an unused
// fd number is simply closed in the child, so the FileListener call is the test.
//
// Nil is a supported outcome, not an error: without the shared origin the gh
// channel still works for `gh api` and any explicitly-scoped command, so a run
// degrades to the pre-shared-origin behaviour rather than failing.
func adoptSharedOriginListener() net.Listener {
	// Confirm the descriptor is a socket BEFORE constructing an os.File over it.
	// When the broker passed nothing, fd 3 is not merely empty — it is whatever
	// the Go runtime allocated next (its netpoll epoll instance is the likely
	// occupant, since stdio takes 0-2), and wrapping that in an os.File we then
	// Close would tear out the runtime's own poller. fstat only reads.
	var st syscall.Stat_t
	if err := syscall.Fstat(sandbox.SharedOriginListenerFD, &st); err != nil ||
		st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		sidecarLog.Info("no shared-origin listener passed; the gh channel will bind an ephemeral port and repo inference will not resolve")
		return nil
	}
	f := os.NewFile(uintptr(sandbox.SharedOriginListenerFD), "shared-origin-listener")
	ln, err := net.FileListener(f)
	// FileListener dups the descriptor, so this copy is finished with either way.
	_ = f.Close()
	if err != nil {
		sidecarLog.Warn("shared-origin descriptor is not a usable listener; the gh channel will bind an ephemeral port and repo inference will not resolve", "error", err)
		return nil
	}
	return ln
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
