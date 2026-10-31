package credprovision

import (
	"context"
	"time"
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
// alongside the rest of the brain exactly like instance.RunRegistryGC. One sweep
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
// conversation type, so every parked engagement lands in the same list.
func sweepAwaiting(ctx context.Context, mgr *Manager) {
	parked, err := mgr.stores.ConversationQueue.ListAwaitingCredentials(ctx)
	if err != nil {
		log.Warn("list awaiting-credentials claims failed; retrying next tick", "error", err)
		return
	}
	for _, p := range parked {
		if perr := mgr.ProvisionForConversation(ctx, p.OrgID, p.ConversationID); perr != nil {
			log.Warn("backstop-sweep provision failed",
				"conversation", p.ConversationID, "type", p.ConversationType, "error", perr)
		}
	}
}

// RunRefreshSweep re-provisions every active (claimed, non-terminal)
// conversation whose sealed bundle the brain should replace now (TFAC-614): one
// older than refreshAfter — GitHub installation tokens are hour-lived,
// engagements aren't — and, between those ordinary re-seals, one whose source
// manifest names an unresolved source or a sealed credential that expires
// before the next tick could reach it. Piggybacks no new scheduler cadence
// beyond its own ticker, per the same brain-unit shape as RunAwaitingSweep.
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
			sweepRefresh(ctx, mgr, interval, refreshAfter)
		}
	}
}

// expiryMargin is how far ahead of the next tick a sealed credential's expiry
// counts as close: the re-seal has to reach the sidecar, which the executor
// relays on its own cadence, before the credential lapses.
const expiryMargin = 2 * time.Minute

// sweepRefresh runs one pass. An engagement with an unresolved source is
// re-sealed on every pass until it resolves, so a run recovers within one
// interval of its source recovering rather than at its next ordinary re-seal.
func sweepRefresh(ctx context.Context, mgr *Manager, interval, refreshAfter time.Duration) {
	now := time.Now()
	convs, err := mgr.stores.ConversationQueue.ListActiveNeedingCredentialRefresh(ctx, now.Add(-refreshAfter), now.Add(interval+expiryMargin))
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
