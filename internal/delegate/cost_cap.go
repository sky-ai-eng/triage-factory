package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDailyCostCapReached is returned by Delegate when the org's spend for the
// current UTC calendar day has reached its configured max_daily_cost_usd cap
// (TFAC-477). It blocks every new spawn — manual and autonomous alike — at the
// single delegation choke point, a runaway-spend fuse most relevant for a
// misconfigured autonomous trigger looping. Callers surface it sensibly: the
// HTTP delegate handlers return it as a delegate_error in a 200 response; the
// event router logs it and leaves the task queued for a later retry (once the
// UTC day rolls over, or the admin clears the cap). The wrapped message carries
// the actual today-vs-cap figures for the user-facing error.
var ErrDailyCostCapReached = errors.New("org daily spend cap reached")

// checkDailyCostCap is the admission gate at Delegate entry. It returns
// ErrDailyCostCapReached (wrapped with the figures) when the org's settled LLM
// spend for today (UTC calendar day, summed across EVERY category — autonomous
// + manual + curator + system overhead) is at or above the org's configured
// cap, and nil otherwise.
//
// Design (all locked in TFAC-477):
//
//   - "Today" is the UTC calendar day: time.Now().UTC().Truncate(24h). Per-org
//     timezone is a future refinement; UTC matches Anthropic bill reconciliation.
//   - NULL / 0 cap = no cap. Trip condition is today_spend >= cap.
//   - Best-effort fuse: two concurrent delegates can both pass and slightly
//     overshoot — acceptable, no locking. In-flight runs are never affected
//     (this only gates NEW spawns).
//   - Fail OPEN on any read error (settings or spend) — log + allow. A transient
//     read failure must not wedge all delegation. A nil store (test fixtures that
//     pass a partial bundle) is treated the same as "no cap".
//   - Spend is read via the admin-pool SpendByCategorySystem, NOT the app-pool
//     SpendByCategory: Delegate runs under context.Background() with no JWT
//     claims, so in multi-mode an app-pool read has no tf.current_org_id() and
//     would return nothing — the cap would never trip.
func (s *Spawner) checkDailyCostCap(ctx context.Context, orgID string) error {
	if s.orgs == nil || s.spend == nil {
		return nil // no stores wired (test fixtures) → no cap to enforce
	}

	settings, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		// Fail open: a settings read failure must not block delegation.
		delegateLog.Warn("daily cost cap: read org settings failed; allowing delegation", "org", orgID, "error", err)
		return nil
	}
	if settings.MaxDailyCostUSD <= 0 {
		return nil // NULL / 0 → no cap
	}

	since := time.Now().UTC().Truncate(24 * time.Hour)
	buckets, err := s.spend.SpendByCategorySystem(ctx, orgID, since, time.Time{})
	if err != nil {
		// Fail open: a spend read failure must not block delegation.
		delegateLog.Warn("daily cost cap: read today's spend failed; allowing delegation", "org", orgID, "error", err)
		return nil
	}
	var total float64
	for _, b := range buckets {
		total += b.TotalCostUSD
	}

	if total >= settings.MaxDailyCostUSD {
		return fmt.Errorf("%w: $%.2f of $%.2f today", ErrDailyCostCapReached, total, settings.MaxDailyCostUSD)
	}
	return nil
}
