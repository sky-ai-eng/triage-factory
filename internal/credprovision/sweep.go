package credprovision

import (
	"context"
	"time"
)

// RunAwaitingSweep is the backstop for runs parked in status=
// 'awaiting_credentials' whose cred_request tf_ctl notification the lossy
// relay dropped (TFAC-614) — leader-gated, started/stopped alongside the
// rest of the brain exactly like reaper.RunReaper. mgr nil is a no-op (the
// same nil-checked shape every other brain-unit member uses).
func RunAwaitingSweep(ctx context.Context, mgr *Manager, interval time.Duration) {
	if mgr == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepAwaiting(ctx, mgr)
		}
	}
}

func sweepAwaiting(ctx context.Context, mgr *Manager) {
	runs, err := mgr.stores.RunQueue.ListAwaitingCredentials(ctx)
	if err != nil {
		log.Warn("list awaiting-credentials runs failed; retrying next tick", "error", err)
		return
	}
	for _, r := range runs {
		if err := mgr.ProvisionForRun(ctx, r.OrgID, r.RunID); err != nil {
			log.Warn("backstop-sweep provision failed", "run", r.RunID, "error", err)
		}
	}
}

// RunRefreshSweep re-mints and re-seals the GitHub tokens of every active
// (claimed, non-terminal) run whose sealed bundle is older than
// refreshAfter (TFAC-614) — GitHub installation tokens are hour-lived, runs
// aren't. Piggybacks no new scheduler cadence beyond its own ticker, per the
// same brain-unit shape as RunAwaitingSweep.
func RunRefreshSweep(ctx context.Context, mgr *Manager, interval, refreshAfter time.Duration) {
	if mgr == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepRefresh(ctx, mgr, refreshAfter)
		}
	}
}

func sweepRefresh(ctx context.Context, mgr *Manager, refreshAfter time.Duration) {
	runs, err := mgr.stores.RunQueue.ListActiveNeedingCredentialRefresh(ctx, time.Now().Add(-refreshAfter))
	if err != nil {
		log.Warn("list runs needing credential refresh failed; retrying next tick", "error", err)
		return
	}
	for _, r := range runs {
		if err := mgr.ProvisionForRun(ctx, r.OrgID, r.RunID); err != nil {
			log.Warn("refresh-sweep provision failed", "run", r.RunID, "error", err)
		}
	}
}
