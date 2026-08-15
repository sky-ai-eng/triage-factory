package app

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/instance"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// ensureIdentity resolves this process's persistent instance identity — the
// id the fleet registry keys every instances row on. Called first thing in
// New, before any other subsystem touches the state root:
// the identity file's exclusive lock is the two-process guard, and it's
// most valuable held as early in boot as possible.
func (a *App) ensureIdentity() error {
	root, err := paths.StateRootErr()
	if err != nil {
		return fmt.Errorf("resolve state root for instance identity: %w", err)
	}
	id, err := instance.EnsureIdentity(root)
	if err != nil {
		return err
	}
	a.identity = id
	return nil
}

// registerInstance performs the boot-registration upsert against the fleet
// registry: mint boot_epoch=1 on a fresh id, or bump it on a restart of the
// same id. Called once openStores has a live db.Stores bundle. Every role
// registers (the registry is fleet-wide deployment visibility, not just an
// executor concern); the role + build version it stamps here are what make
// version skew and the control/executor split visible in the registry.
//
// The instances row's pubkey is always empty (TFAC-631): the orchestrator
// holds no sealing key and never unseals a run's bundle. Each run's credential
// sidecar mints its own per-run keypair and publishes its public half onto the
// run's claim (claims.cred_pubkey) at bring-up, and the brain seals to that.
func (a *App) registerInstance(ctx context.Context) error {
	role := string(a.plan.role)

	// The orchestrator publishes no sealing pubkey: it holds no private key and
	// never unseals a run's bundle. Each run's credential sidecar mints its own
	// per-run keypair and publishes the public half onto the claim
	// (claims.cred_pubkey) at bring-up, and the brain seals to that — so the
	// instances row's pubkey is always empty now (TFAC-631).
	epoch, err := a.stores.Instances.Register(ctx, a.identity.ID, role, a.cfg.Version, "")
	if err != nil {
		return fmt.Errorf("register instance: %w", err)
	}
	a.bootEpoch = epoch
	appLog.Info("instance registered", "id", a.identity.ID, "role", role, "boot_epoch", epoch, "version", a.cfg.Version)
	return nil
}
