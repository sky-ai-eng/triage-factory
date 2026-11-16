package delegate

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
)

// checkModelProvider reports whether orgID can authenticate model, as the
// modelaccess refusal itself: ErrNoCredentials for an org that can reach no
// provider at all, asked first, then ErrProviderUnconfigured for a model whose
// provider the org never connected.
//
// It is asked wherever work is about to start on a model, because it describes
// state that can change after the model was stored: a credential can be
// disconnected, and disconnecting one rewrites none of the defaults, pins, and
// message rows already saved. A delegation asks it of every model in its plan
// (checkModelProviders), a person's message of the model it would run on
// (resolveTurnSettings), and a claim of the model it resolved (modelForClaim).
//
// Whether a model is one this TEAM may pick is a separate question with its own
// gate (the enable-set). Kept apart because the remedies differ: one is a
// credential nobody bound, the other a decision somebody made, and a caller
// told the wrong one goes to the wrong settings page.
//
// Fails OPEN on a read error, matching the cost caps: a transient settings read
// must not stop every firing, message, and claim, and the credential resolver
// is the backstop — it refuses the same work by name a moment later.
func (s *Spawner) checkModelProvider(ctx context.Context, orgID, model string) error {
	if s.orgs == nil {
		return nil // no stores wired (test fixtures) → nothing to check against
	}
	orgSet, err := s.orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("model provider check: read org settings failed; allowing the model", "org", orgID, "model", model, "error", err)
		return nil
	}

	// Whether the org can authenticate anything at all, asked before the
	// per-model question. A local org that has bound nothing and did not choose
	// to run on the machine's credentials would otherwise start work and quietly
	// spend against whatever the operator's shell holds.
	creds := modelaccess.ForOrg(orgSet)
	if err := creds.Ready(); err != nil {
		return err
	}
	return creds.Check(model)
}

// checkModelProviders is the credential admission gate for the models a
// delegation would actually run on: the team's default and every step pin in
// the blueprint about to fire. PlanDelegation calls it once the steps' pins are
// resolved.
//
// It refuses BEFORE any write — no blueprint_run is minted, no conversation is
// enqueued, and a person's delegation has not yet claimed the task — so there
// is nothing to retry and nothing to reap. That is the point: TF ships no
// fallback model, so the alternative to refusing is spending the org's money
// on a model nobody chose.
//
// An unset step is asked as the empty model, which passes the per-model
// question and still asks the org-level one.
func (s *Spawner) checkModelProviders(ctx context.Context, orgID string, models []string) error {
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if seen[m] {
			continue
		}
		seen[m] = true
		if err := s.checkModelProvider(ctx, orgID, m); err != nil {
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
