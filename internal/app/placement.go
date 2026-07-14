package app

import (
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// buildPlacement resolves the placement config from env, builds the
// rendezvous resolver over the fleet registry + override table, and wires it
// into the spawner (the enqueue stamp + the two-tier claim config) and, on a
// serving role, the server (the /api/fleet/placement explainer). Runs after
// buildExecution — it needs a.spawner — and after openStores (a.stores).
//
// Placement is ON by default in multi mode and OFF in local (N=1: the hash
// always returns self). Either way a resolver is built; a disabled config
// just makes Enabled() false, so the stamp is empty and the claim runs the
// global-oldest path — the "dropping the layer leaves everything green"
// contract lives in that single boolean, not in nil checks scattered around.
func (a *App) buildPlacement() error {
	cfg, err := placement.ConfigFromEnv(os.Getenv, a.local())
	if err != nil {
		return err
	}
	a.placementResolver = placement.NewResolver(a.stores.Instances, a.stores.PlacementOverrides, cfg)

	claim := db.ClaimPlacement{Enabled: cfg.Enabled, AgingInterval: cfg.Aging, Liveness: cfg.Liveness}
	a.spawner.SetPlacement(a.placementResolver, claim)
	if a.srv != nil {
		a.srv.SetPlacementResolver(a.placementResolver)
	}

	// Curator homing (spec §6.3): a control pod resolves each turn's home
	// executor over the same rendezvous resolver + curator_homes, then forwards
	// (or runs in-process as a degraded fallback). Wired here, not in
	// buildCuratorRuntime, because the resolver only exists now. role=all keeps
	// the unchanged in-process path (no Homer); executors receive homed turns
	// via the claim loop, not a Homer.
	if a.curator != nil && runmode.Current() == runmode.ModeMulti && a.plan.role == runmode.RoleControl {
		a.curator.SetHoming(curator.NewHomer(a.stores.CuratorHomes, a.placementResolver, a.identity.ID), a.identity.ID)
		a.curator.SetDoorbell(a.publishCuratorDoorbell)
	}

	if cfg.Enabled {
		appLog.Info("placement affinity enabled (capacity-weighted rendezvous)",
			"aging", cfg.Aging, "liveness", cfg.Liveness)
	} else {
		appLog.Info("placement affinity disabled (queue is global-oldest; every run claimable anywhere)")
	}
	return nil
}
