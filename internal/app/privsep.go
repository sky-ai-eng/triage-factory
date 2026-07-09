package app

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

// capBrokerHandle is the cross-platform seam for the spawned broker
// process's lifecycle. The real value is *capbroker.Process on Linux; nil
// everywhere else, since startCapBrokerIfEnabled's WillSandbox gate below
// means startCapBroker (privsep_linux.go / privsep_other.go) is never
// actually reached off Linux. Declared as an interface — rather than
// importing *capbroker.Process directly into App — because
// cmd/capbroker's Linux-only types (it wraps sandbox.PrivilegedOps,
// itself Linux-only) would otherwise leak into this cross-platform
// struct field.
type capBrokerHandle interface {
	Close() error
}

// startCapBrokerIfEnabled starts the cap-broker subprocess and routes
// internal/sandbox's privileged-ops implementation through it, when
// TF_PRIVSEP=1 AND this host will actually sandbox runs (multi mode +
// Linux — agentproc.WillSandbox). Local mode, and TF_PRIVSEP unset,
// leave sandbox.defaultOps as hostOps{}: today's in-process behavior,
// byte-identical — local mode never sandboxes, so it's unaffected either
// way.
//
// Safe to call unconditionally from any platform — the actual spawn
// (startCapBroker) is build-tag split and this method's own gates never
// let the non-Linux stub fire in practice.
func (a *App) startCapBrokerIfEnabled(ctx context.Context) error {
	if !capbroker.Enabled() {
		return nil
	}
	if !agentproc.WillSandbox() {
		appLog.Warn("TF_PRIVSEP=1 set but this host never sandboxes runs (local mode, or non-Linux); ignoring")
		return nil
	}
	handle, err := startCapBroker(ctx)
	if err != nil {
		return fmt.Errorf("start cap-broker: %w", err)
	}
	a.capBroker = handle
	appLog.Info("cap-broker started; privileged sandbox ops now route through it", "privsep", true)
	return nil
}
