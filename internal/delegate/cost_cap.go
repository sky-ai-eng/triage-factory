package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// ErrDailyCostCapReached is what a delegation refused by a daily spend cap
// satisfies: spend for the current UTC calendar day has reached a configured
// max_daily_cost_usd cap, the org-wide one or a per-team one. The refusal itself
// is a *CostCapError, which says which. Callers test errors.Is: the delegate
// route answers 409 CONFLICT with a message naming the cap and who can raise
// it, and the event router logs it and leaves the task queued for a later
// retry once the UTC day rolls over or an admin raises the cap.
var ErrDailyCostCapReached = errors.New("daily spend cap reached")

// CostCapScope is which daily spend cap refused a delegation.
type CostCapScope string

const (
	CostCapScopeOrg  CostCapScope = "org"
	CostCapScopeTeam CostCapScope = "team"
)

// CostCapError is a delegation refused by a daily spend cap. It carries the
// figures separately from the scope because they are an org admin's read: a
// caller that answers a person who may not see spend names the cap and leaves
// them out.
type CostCapError struct {
	Scope CostCapScope
	// TeamName is the capped team's display name, or its id when the name
	// can't be read; empty for the org cap.
	TeamName string
	SpentUSD float64
	CapUSD   float64
}

func (e *CostCapError) Error() string {
	if e.Scope == CostCapScopeTeam {
		return fmt.Sprintf("%v: team %q at $%.2f of $%.2f today", ErrDailyCostCapReached, e.TeamName, e.SpentUSD, e.CapUSD)
	}
	return fmt.Sprintf("%v: org at $%.2f of $%.2f today", ErrDailyCostCapReached, e.SpentUSD, e.CapUSD)
}

func (e *CostCapError) Unwrap() error { return ErrDailyCostCapReached }

// checkDailyCostCap is the org's admission gate in PlanDelegation. It returns
// a *CostCapError of scope CostCapScopeOrg when the org's settled LLM
// spend for today (UTC calendar day, summed across EVERY category — autonomous
// + manual + system overhead) is at or above the org's configured
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
//     SpendByCategory: PlanDelegation runs under context.Background() with no JWT
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
		return &CostCapError{Scope: CostCapScopeOrg, SpentUSD: total, CapUSD: settings.MaxDailyCostUSD}
	}
	return nil
}

// checkTeamDailyCostCap is the per-team admission gate (TFAC-482) — the team-
// scoped sibling of checkDailyCostCap, run right after it in PlanDelegation
// with the run's team. It returns a *CostCapError of scope CostCapScopeTeam
// when the team's settled LLM spend for today is at or above the team's
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
//     team_id, so system overhead (NULL team_id) never
//     counts toward a team cap — it's the org cap's concern only, never
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
		// Resolve the team's display name for the user-facing message — a raw
		// UUID reads as noise. Only on the cap-tripped path, so the extra read
		// never touches the happy path; fall back to the teamID if the lookup
		// fails or the name is empty.
		label := teamID
		if t, lerr := s.teams.GetSystem(ctx, orgID, teamID); lerr == nil && t != nil && t.Name != "" {
			label = t.Name
		}
		return &CostCapError{Scope: CostCapScopeTeam, TeamName: label, SpentUSD: total, CapUSD: settings.MaxDailyCostUSD}
	}
	return nil
}
