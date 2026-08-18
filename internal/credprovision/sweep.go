package credprovision

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// minRefreshSweepInterval floors the derived sweep cadence so a
// misconfigured (tiny) TTL can't spin the sweep into a hot loop.
const minRefreshSweepInterval = 30 * time.Second

// RefreshCadenceForTTL derives the refresh sweep's interval + bundle-age
// threshold from the configured LLM-credential TTL (TFAC-616). Role-mode
// STS session creds must be re-minted before they expire — so the age
// threshold shrinks to half the TTL (the resolver re-mints at half-life)
// and the sweep interval to a quarter, guaranteeing a re-seal well inside
// the credential's life. A bundle still older than the GitHub 30-min
// heuristic keeps installation tokens fresh either way. At the default 1h
// TTL the threshold is 30m and the interval 5m — byte-for-byte the PS-H5
// cadence, so non-role deployments see no change. Shortening the TTL
// re-mints role material (and, as an accepted tradeoff, re-mints git tokens)
// more often; STS call volume stays bounded by the resolver's per-org
// half-life cache.
func RefreshCadenceForTTL(llmTTL time.Duration) (interval, refreshAfter time.Duration) {
	refreshAfter = DefaultRefreshAfter
	if half := llmTTL / 2; half > 0 && half < refreshAfter {
		refreshAfter = half
	}
	interval = DefaultRefreshSweepInterval
	if quarter := llmTTL / 4; quarter > 0 && quarter < interval {
		interval = quarter
	}
	if interval < minRefreshSweepInterval {
		interval = minRefreshSweepInterval
	}
	return interval, refreshAfter
}

// RunAwaitingSweep is the backstop for every conversation whose active claim
// is parked in phase='awaiting_credentials' but whose cred_request tf_ctl
// notification the lossy relay dropped — leader-gated, started/stopped
// alongside the rest of the brain exactly like reaper.RunReaper. One sweep
// serves both surfaces (see sweepAwaiting). mgr nil is a no-op (the same
// nil-checked shape every other brain-unit member uses).
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

// sweepAwaiting drives ONE scan for every surface: parking a claim in
// phase='awaiting_credentials' is a property of the engagement, not of the
// conversation type, so both a delegated conversation's sidecar bring-up and a
// curator turn's land in the same list. Only the resolution differs, and the
// conversation type on each row is what selects it.
func sweepAwaiting(ctx context.Context, mgr *Manager) {
	parked, err := mgr.stores.ConversationQueue.ListAwaitingCredentials(ctx)
	if err != nil {
		log.Warn("list awaiting-credentials claims failed; retrying next tick", "error", err)
		return
	}
	for _, p := range parked {
		var perr error
		if p.ConversationType == domain.ConversationTypeCurator {
			perr = mgr.ProvisionForCuratorTurn(ctx, p.OrgID, p.ConversationID)
		} else {
			perr = mgr.ProvisionForConversation(ctx, p.OrgID, p.ConversationID)
		}
		if perr != nil {
			log.Warn("backstop-sweep provision failed",
				"conversation", p.ConversationID, "type", p.ConversationType, "error", perr)
		}
	}
}

// RunRefreshSweep re-mints and re-seals the GitHub tokens of every active
// (claimed, non-terminal) conversation whose sealed bundle is older than
// refreshAfter (TFAC-614) — GitHub installation tokens are hour-lived,
// engagements
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
	convs, err := mgr.stores.ConversationQueue.ListActiveNeedingCredentialRefresh(ctx, time.Now().Add(-refreshAfter))
	if err != nil {
		log.Warn("list conversations needing credential refresh failed; retrying next tick", "error", err)
		return
	}
	for _, r := range convs {
		if err := mgr.ProvisionForConversation(ctx, r.OrgID, r.ConversationID); err != nil {
			log.Warn("refresh-sweep provision failed", "conversation", r.ConversationID, "error", err)
		}
	}
}
