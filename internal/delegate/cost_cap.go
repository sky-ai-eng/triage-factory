package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ErrDailyCostCapReached is returned by Delegate when spend for the current UTC
// calendar day has reached a configured max_daily_cost_usd cap — the org-wide
// cap (TFAC-477) or a per-team cap (TFAC-482). One sentinel covers both: a
// blocked spawn is a blocked spawn, so callers test errors.Is and surface it
// identically (the HTTP delegate handlers return it as a delegate_error in a 200
// response; the event router logs it and leaves the task queued for a later
// retry once the UTC day rolls over or the admin raises the cap). The wrapped
// message names the scope (org / team) and carries the actual today-vs-cap
// figures for the user-facing error.
var ErrDailyCostCapReached = errors.New("daily spend cap reached")

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
		return fmt.Errorf("%w: org at $%.2f of $%.2f today", ErrDailyCostCapReached, total, settings.MaxDailyCostUSD)
	}
	return nil
}

// checkTeamDailyCostCap is the per-team admission gate (TFAC-482) — the team-
// scoped sibling of checkDailyCostCap, run right after it in Delegate with the
// task's resolved team. It returns ErrDailyCostCapReached (wrapped, naming the
// team) when the team's settled LLM spend for today is at or above the team's
// configured cap, and nil otherwise. Both caps apply: a spawn is blocked if the
// org cap OR the team cap trips.
//
// Same semantics as the org cap (UTC calendar day, NULL/0 = no cap, trip at
// >=, in-flight unaffected, best-effort, fail OPEN on read error) with two
// differences that fall out of the design:
//
//   - EE-gated enforcement (FeatureGovernance). When governance is unlicensed or
//     lapsed, per-team caps go dormant and the org cap (checkDailyCostCap, core)
//     remains the safety net — "governance works iff licensed". The entitlement
//     check is first so an unlicensed deployment does zero extra store work.
//   - System spend is excluded automatically. The spend read is keyed on
//     team_id, so system overhead and non-team curator (both NULL team_id) never
//     count toward a team cap — they're the org cap's concern only, never
//     apportioned to a team.
//
// The caller passes the already-resolved teamID and skips this entirely for an
// unowned task (teamID == ""), which is uncappable by construction.
func (s *Spawner) checkTeamDailyCostCap(ctx context.Context, orgID, teamID string) error {
	// EE gate first: dormant unless governance is licensed.
	if !entitlements.For(orgID).Has(entitlements.FeatureGovernance) {
		return nil
	}
	if s.teams == nil || s.spend == nil {
		return nil // no stores wired (test fixtures) → no cap to enforce
	}

	settings, err := s.teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		// Fail open: a settings read failure must not block delegation.
		delegateLog.Warn("team daily cost cap: read team settings failed; allowing delegation", "org", orgID, "team", teamID, "error", err)
		return nil
	}
	if settings.MaxDailyCostUSD <= 0 {
		return nil // NULL / 0 → no cap
	}

	since := time.Now().UTC().Truncate(24 * time.Hour)
	buckets, err := s.spend.SpendByCategorySystemForTeam(ctx, orgID, teamID, since, time.Time{})
	if err != nil {
		// Fail open: a spend read failure must not block delegation.
		delegateLog.Warn("team daily cost cap: read today's team spend failed; allowing delegation", "org", orgID, "team", teamID, "error", err)
		return nil
	}
	var total float64
	for _, b := range buckets {
		total += b.TotalCostUSD
	}

	if total >= settings.MaxDailyCostUSD {
		// Resolve the team's display name for the user-facing message (this error
		// surfaces in the delegate_error payload) — a raw UUID reads as noise. Only
		// on the cap-tripped path, so the extra read never touches the happy path;
		// fall back to the teamID if the lookup fails or the name is empty.
		label := teamID
		if t, lerr := s.teams.GetSystem(ctx, orgID, teamID); lerr == nil && t != nil && t.Name != "" {
			label = t.Name
		}
		return fmt.Errorf("%w: team %q at $%.2f of $%.2f today", ErrDailyCostCapReached, label, total, settings.MaxDailyCostUSD)
	}
	return nil
}
