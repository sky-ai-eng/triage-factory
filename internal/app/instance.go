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
func (a *App) registerInstance(ctx context.Context) error {
	role := string(a.plan.role)
	epoch, err := a.stores.Instances.Register(ctx, a.identity.ID, role, a.cfg.Version)
	if err != nil {
		return fmt.Errorf("register instance: %w", err)
	}
	a.bootEpoch = epoch
	appLog.Info("instance registered", "id", a.identity.ID, "role", role, "boot_epoch", epoch, "version", a.cfg.Version)
	return nil
}
