package app

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

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
// exercise the fail-closed path without a real broker.
var startCapBrokerFn = startCapBroker

// startCapBrokerIfSandboxing starts the cap-broker subprocess and routes
// internal/sandbox's privileged-ops and launch implementation through it
// whenever this host will actually sandbox runs (multi mode + Linux —
// agentproc.WillSandbox). The broker is the ONLY sandbox launch path, so a
// broker that can't start is FATAL: the error propagates to app.New rather
// than falling back to a less-isolated in-process launch. Local mode (and
// non-Linux) never sandboxes, so this is a no-op there — the sandbox is
// never reached, and no broker is needed.
//
// Safe to call unconditionally from any platform — the actual spawn
// (startCapBroker) is build-tag split and the WillSandbox gate never lets
// the non-Linux stub fire in practice.
func (a *App) startCapBrokerIfSandboxing(ctx context.Context) error {
	if !agentproc.WillSandbox() {
		return nil
	}
	handle, err := startCapBrokerFn(ctx)
	if err != nil {
		return fmt.Errorf("start cap-broker: %w", err)
	}
	a.capBroker = handle
	appLog.Info("cap-broker started; privileged sandbox ops now route through it")
	return nil
}
