package app

import (
	"context"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// currentUID is the process-uid seam orchestratorUIDCollidesWithSidecarBand
// reads through — os.Getuid() by default, overridable in tests so the
// collision path is exercisable without a real setuid drop.
var currentUID = os.Getuid

// orchestratorUIDCollidesWithSidecarBand reports whether this process's
// own uid falls inside the reserved sidecar band — sandbox.SidecarUIDBase's
// own init() assertion only pins the DEFAULT orchestrator uid (10001) as
// disjoint from that band; it can't see an operator-repointed
// TF_ORCHESTRATOR_UID. This is the authoritative check: it reads the uid
// the kernel actually gave this process after whatever privilege drop ran
// (setpriv, in the shipped container) — not a flag or env var's claim
// about it. Independent of the broker's own matching check on its
// --orchestrator-uid flag (cmd/capbroker/cli_linux.go): that flag and this
// process's real uid are configured on two different command lines and
// could drift, so this backstop holds even if they do.
func orchestratorUIDCollidesWithSidecarBand() bool {
	return sandbox.IsSidecarUID(currentUID())
}

// capBrokerHandle is the cross-platform seam for the spawned broker
// process's lifecycle. The real value is *capbroker.Process on Linux; nil
// everywhere else, since startCapBrokerIfSandboxing's WillSandbox gate
// below means startCapBroker (privsep_linux.go / privsep_other.go) is
// never actually reached off Linux. Declared as an interface — rather than
// importing *capbroker.Process directly into App — because cmd/capbroker's
// Linux-only types (it wraps sandbox.PrivilegedOps, itself Linux-only)
// would otherwise leak into this cross-platform struct field.
type capBrokerHandle interface {
	Close() error
}

// startCapBrokerFn is the broker-spawn seam — the platform startCapBroker
// (privsep_linux.go / privsep_other.go) by default, overridable in tests to
// exercise the fail-closed path without a real broker. It returns the
// broker handle plus a Ping seam (nil off Linux / when unsupported) the
// executor healthz's broker_ok check round-trips against the socket.
var startCapBrokerFn = startCapBroker

// startCapBrokerIfSandboxing starts the cap-broker subprocess and routes
// internal/sandbox's privileged-ops and launch implementation through it
// whenever this host will actually run the native runtime's resident
// tool-host jail (multi mode + Linux — agentproc.WillSandbox) AND this role
// hosts sandboxes. The broker is the ONLY sandbox launch path, so a broker
// that can't start is FATAL: the error propagates to app.New rather than
// falling back to a less-isolated in-process launch. Local mode (and
// non-Linux) never sandboxes — the SDK loop (agentproc.Run/RunInteractive)
// always spawns directly, jail or no — so this is a no-op there and no
// broker is needed.
//
// The control role is excluded even in multi mode on Linux: a control pod
// never launches a sandbox — sandboxed work homes to executors and the
// brain's own LLM work is toolless direct API calls — so it needs no
// broker and holds no privileged ops. Leaving the broker unstarted is
// deliberately fail-closed: SetPrivilegedOps stays uninstalled, so a stray
// sandbox launch that somehow reached a control pod (a bug, a misconfig)
// fails loudly at construction ("cap-broker not started") rather than
// silently running unjailed in the capless orchestrator. The compose
// override drops the container caps to match.
//
// Safe to call unconditionally from any platform — the actual spawn
// (startCapBroker) is build-tag split and the WillSandbox gate never lets
// the non-Linux stub fire in practice.
func (a *App) startCapBrokerIfSandboxing(ctx context.Context) error {
	if !agentproc.WillSandbox() {
		return nil
	}
	if a.plan.role == runmode.RoleControl {
		appLog.Info("control role: skipping cap-broker (control pods never launch sandboxes)")
		return nil
	}
	// A collision here would let the orchestrator and some run's sidecar
	// share a uid, defeating the per-run-uid isolation the sidecar design
	// relies on entirely (same-uid processes can signal each other and,
	// subject to the host's Yama ptrace_scope, ptrace each other) — refuse
	// to serve rather than boot into that.
	if orchestratorUIDCollidesWithSidecarBand() {
		return fmt.Errorf("orchestrator uid %d falls inside the reserved sidecar uid band [%d, %d) — repoint TF_ORCHESTRATOR_UID outside that range",
			currentUID(), sandbox.SidecarUIDBase, sandbox.SidecarUIDBase+sandbox.MaxSandboxes)
	}
	handle, ping, err := startCapBrokerFn(ctx)
	if err != nil {
		return fmt.Errorf("start cap-broker: %w", err)
	}
	a.capBroker = handle
	a.brokerPing = ping
	appLog.Info("cap-broker started; privileged sandbox ops now route through it")
	return nil
}
