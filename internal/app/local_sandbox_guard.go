package app

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// checkLocalSandbox refuses to boot when local agent runs are supposed to be
// sandboxed and bubblewrap cannot actually build a namespace on this host.
//
// The alternative — log a warning and run agents unsandboxed — is the one
// behavior a default-on safety feature must never have. An operator who has
// not opted out is entitled to assume the isolation is there; discovering it
// silently wasn't, after an agent has already been reading their home
// directory, is strictly worse than a refused boot with a one-line fix.
//
// The refusal names the fix for the failure that HAPPENED, not both fixes for
// both failures — see localSandboxRefusal.
func (a *App) checkLocalSandbox() error {
	if !runmode.LocalSandboxEnabled() {
		return nil
	}
	if err := localsandbox.Probe(); err != nil {
		return localSandboxRefusal(err)
	}
	appLog.Info("local agent sandbox active", "isolation", "bubblewrap mount namespace")
	return nil
}

// localSandboxRefusal is the boot-refusal message, kept pure so its wording —
// the part an operator actually acts on — is asserted on every host rather
// than only on the ones where the probe happens to fail.
//
// It branches, because the two failures have disjoint fixes and a message
// carrying both leads with the wrong one half the time. Someone who installs
// the package a message told them to install, and then hits a wall that still
// says "install the package", has been actively misdirected — the install was
// never going to help, and the message hid that behind identical prose.
//
// What it deliberately does NOT explain is which bubblewrap to use: Resolve
// already tries the installed ones and picks a working one, so by the time
// this is reached there is no PATH left to audit. Fixing the system is what
// let the message get shorter.
//
// Neither branch suggests running TF as root, and that is deliberate rather
// than an omission. bubblewrap creating an unprivileged user namespace IS the
// design; a host that would need privilege has a policy refusing it, and
// running the whole server as root to satisfy that policy would hand the
// agent's sandbox — and everything else TF does — far more than it removes.
func localSandboxRefusal(probeErr error) error {
	const preamble = "refusing to start: local agent runs are sandboxed with bubblewrap, but "

	switch {
	case errors.Is(probeErr, localsandbox.ErrNotInstalled):
		return fmt.Errorf(preamble+"%w. "+
			"Install it with `sudo apt install bubblewrap` (or your distro's equivalent), or set "+
			"TF_LOCAL_SANDBOX=off to run agents with your full user's powers instead", probeErr)

	case errors.Is(probeErr, localsandbox.ErrNamespaceRefused):
		return fmt.Errorf(preamble+"%w.\n"+
			"  It is installed, so re-installing will not help — and neither will running Triage Factory\n"+
			"  as root, since an unprivileged user namespace is exactly what is being refused.\n"+
			"  Usual causes: this host restricts unprivileged user namespaces (Ubuntu 23.10+ gates them\n"+
			"  behind AppArmor — try `sudo systemctl reload apparmor` if bubblewrap was installed after\n"+
			"  AppArmor started), or Triage Factory is itself inside a container that blocks nested ones.\n"+
			"  Set TF_LOCAL_SANDBOX=off to run agents with your full user's powers instead", probeErr)
	}

	// An unclassified probe failure: report it as-is rather than guessing at a
	// fix. The opt-out still applies, and is the one thing that is true of
	// every failure this function can be handed.
	return fmt.Errorf(preamble+"the sandbox probe failed: %w. "+
		"Set TF_LOCAL_SANDBOX=off to run agents with your full user's powers instead", probeErr)
}
