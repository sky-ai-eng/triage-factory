package app

import (
	"github.com/sky-ai-eng/triage-factory/internal/credprovision"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// buildCredProvisioner constructs the brain-side sealed-credential-bundle
// provisioner (TFAC-614) for brain-capable roles in multi mode — the
// counterpart to buildReaper, same nil-elsewhere shape (local mode,
// TF_ROLE=executor never build one). Runs after openStores/buildExecution
// so a.stores is the real, secret-bearing bundle (never the disabled one
// an executor holds — a.plan.brain is false there anyway).
func (a *App) buildCredProvisioner() error {
	if runmode.Current() != runmode.ModeMulti || !a.plan.brain {
		return nil
	}
	a.credProvisioner = credprovision.NewManager(a.stores, a.llmResolver)
	return nil
}
