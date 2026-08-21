package app

import (
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
// Two fixes, both named, because the two failures are different: bubblewrap
// missing is an install, and a namespace refused (Ubuntu 23.10+ restricts
// unprivileged user namespaces via AppArmor, and it is the distro package's
// own profile that permits them; a container that blocks nested userns does
// the same thing) is often not fixable at all — which is exactly what the
// opt-out is for.
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
func localSandboxRefusal(probeErr error) error {
	return fmt.Errorf(
		"refusing to start: local agent runs are sandboxed with bubblewrap, but the sandbox probe failed: %w. "+
			"Install bubblewrap (`sudo apt install bubblewrap`, or your distro's equivalent) — the distro package is "+
			"what carries the AppArmor profile permitting unprivileged user namespaces. If this host cannot run it "+
			"(a container that blocks nested user namespaces, for instance), set TF_LOCAL_SANDBOX=off to run agents "+
			"with your full user's powers instead",
		probeErr)
}
