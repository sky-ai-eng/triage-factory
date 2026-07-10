package app

import (
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/lease"
)

// buildLease constructs the background-brain lease elector for a control
// pod in multi mode. Called from New, after buildExecution (the spawner's
// IdentityFenced exists to wire as the fence check) and before
// buildRouting (buildLease doesn't need router/poller — its OnAcquire/
// OnDemote callbacks close over the App and resolve a.router/a.pollerMgr
// lazily, at call time, once Run actually starts the elector).
func (a *App) buildLease() error {
	renewInterval, ttl, demoteDeadline, err := lease.KnobsFromEnv()
	if err != nil {
		return fmt.Errorf("lease knobs: %w", err)
	}
	store := lease.NewPostgresStore(a.database)
	mgr, err := lease.NewManager(
		store, lease.BackgroundBrainName, a.identity.ID,
		renewInterval, ttl, demoteDeadline,
		// Identity-fence interaction (#624 invariant): a fenced instance
		// must stop renewing and demote immediately, the same hazard as a
		// partitioned one.
		a.spawner.IdentityFenced,
		func(term int64) { a.startBrain(term) },
		func(reason string) { a.stopBrain(reason) },
	)
	if err != nil {
		return fmt.Errorf("lease manager: %w", err)
	}
	a.leaseElector = mgr
	// GET /readyz's lease-conditionality (spec §8.3) — role=control always
	// builds a server (serveHTTP == true for that role), so this is
	// unconditional here.
	a.srv.SetLeaseStatus(a.leaseStatusForReadyz)
	return nil
}

// leaseStatusForReadyz adapts the lease elector's Status into GET
// /readyz's lease-conditionality contract (TFAC-583, spec §8.3). Wired
// only for role=control (see buildServer / SetLeaseStatus caller) — at
// role=all the field is never wired at all, so /readyz omits it entirely,
// preserving the frozen TFAC-573 contract there.
func (a *App) leaseStatusForReadyz() (name, holderID string, term int64, isHolder bool) {
	if a.leaseElector == nil {
		return lease.BackgroundBrainName, "", 0, true
	}
	st := a.leaseElector.Status()
	return st.Name, st.HolderID, st.Term, st.IsHolder
}
