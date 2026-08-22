package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
)

// checkModelProviders is the admission gate for the models a delegation would
// actually run on: the team's default and every step pin in the blueprint about
// to fire. Delegate calls it once the plan is resolved and before the firing's
// commit point.
//
// It refuses when a model's provider is one the org never connected, or one an
// org admin restricted this team from spending against. Both are dispatch-time
// checks because both describe state that can change after a model was stored: a
// credential can be disconnected and a restriction imposed, and neither rewrites
// the defaults and pins already saved. The save-time check in the settings
// handler catches new selections; this one catches everything else.
//
// It refuses BEFORE any write — no blueprint_run is minted, no conversation is
// enqueued — so there is nothing to retry and nothing to reap. That is the
// point: TF ships no fallback model, so the alternative to refusing is spending
// the org's money on a model nobody chose.
//
// Fails OPEN on a read error, matching the cost caps: a transient settings read
// must not wedge all delegation, and the credential resolver is the backstop —
// it refuses the same run by name a moment later.
func (s *Spawner) checkModelProviders(ctx context.Context, orgID, teamID string, models []string) error {
	if s.orgs == nil {
		return nil // no stores wired (test fixtures) → nothing to check against
	}
	orgSet, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("model provider check: read org settings failed; allowing delegation", "org", orgID, "error", err)
		return nil
	}

	// An unowned task has no team, so it carries no restriction — the org's
	// connected providers are the whole answer for it.
	var allowed []string
	if teamID != "" && s.teams != nil {
		teamSet, err := s.teams.GetSettingsSystem(ctx, teamID)
		if err != nil {
			delegateLog.Warn("model provider check: read team settings failed; allowing delegation", "org", orgID, "team", teamID, "error", err)
			return nil
		}
		allowed = teamSet.AllowedProviders
	}

	// Whether the org can authenticate anything at all, asked once before the
	// per-model questions. A local org that has bound nothing and did not choose
	// to run on the machine's credentials would otherwise dispatch and quietly
	// spend against whatever the operator's shell holds.
	creds := modelaccess.ForOrg(orgSet)
	if err := creds.Ready(); err != nil {
		return fmt.Errorf("cannot dispatch: %w", err)
	}

	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if err := creds.Check(m, allowed); err != nil {
			return fmt.Errorf("cannot dispatch: %w", err)
		}
	}
	return nil
}

// blueprintModels lists the models a firing could run on: the inherited default
// plus every step's own pin. A step is unset (and inherits) or pinned, so this
// is the whole set — nothing at dispatch substitutes a third thing.
func blueprintModels(inherited string, steps []domain.BlueprintPlanStep) []string {
	out := make([]string, 0, len(steps)+1)
	out = append(out, inherited)
	for _, st := range steps {
		out = append(out, st.Model)
	}
	return out
}
