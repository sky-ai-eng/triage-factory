package curator

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
)

// homeResolver is the narrow view the Homer needs of *placement.Resolver: the
// computed candidate Plan for a key. It uses Explain, not Resolve, so the
// candidate order is evaluated even when TF_PLACEMENT is off — homing to *an*
// executor is structural in a multi-split deployment (a control pod is capless
// and cannot run a curator turn), while the rendezvous ordering it layers on is
// the optional cache-warmth optimization. Kept as an interface so a test can
// inject a fake.
type homeResolver interface {
	Explain(ctx context.Context, orgID, keyKind, keyValue string) (placement.Plan, error)
}

// Homer resolves the executor that runs a project's curator turns (spec §6.3).
// It is wired ONLY on multi-mode control pods — the pods that serve chat POSTs
// but are capless (zero sandbox users, the §7 payoff of homing), so a curator
// turn always executes on an executor, never in-process on control. Executors
// never construct one (they receive already-homed turns off the claim loop);
// local / role=all never construct one (the curator runs in-process, unchanged).
//
// The doctrine mirrors placement's: the rendezvous hash is the initial map;
// curator_homes pins the winner until it dies, so a benign fleet reshuffle
// never thrashes a project's warm cache, and only a heartbeat-stale home
// triggers a re-home. The one hard requirement is that at least one executor be
// eligible — a control pod cannot run the turn itself, so a total execution-
// plane outage fails the turn fast and legibly (ok=false) rather than being
// silently absorbed.
type Homer struct {
	homes    db.CuratorHomeStore
	resolver homeResolver

	// selfID is this control pod's instance id — used only to compute forward
	// (home != self). A control pod is never an eligible executor, so a wired
	// Homer's home is always a remote executor and forward is always true; the
	// comparison is kept for defensiveness.
	selfID string
}

// NewHomer builds a Homer over the curator_homes store and the placement
// resolver. selfID is the serving pod's instance id.
func NewHomer(homes db.CuratorHomeStore, resolver homeResolver, selfID string) *Homer {
	return &Homer{homes: homes, resolver: resolver, selfID: selfID}
}

// Resolve returns the home executor for (orgID, projectID), whether the turn is
// forwarded there (home != this pod — always true on a control pod), and ok:
// false when no executor is eligible to run it. A control pod is capless, so
// ok=false is a hard "cannot run" — the caller fails the turn legibly rather
// than sandboxing it locally.
//
// Sticky-until-death: an existing home that is still an eligible live executor
// is reused as-is (even if it is no longer the rendezvous winner); only a home
// that has dropped out of the eligible set is re-minted to the current
// rendezvous winner — one cold blobless clone on the new executor, rare and
// self-healing.
func (h *Homer) Resolve(ctx context.Context, orgID, projectID string) (home string, forward, ok bool) {
	// Explain (not Resolve) so the eligible candidate set is evaluated even when
	// TF_PLACEMENT is off: homing to an executor is structural here, not the
	// optional affinity tuning.
	plan, err := h.resolver.Explain(ctx, orgID, domain.PlacementKindProject, projectID)
	if err != nil {
		curatorLog.Warn("curator home placement resolve failed; turn cannot be homed", "org", orgID, "project", projectID, "error", err)
		return "", false, false
	}

	// Eligible id -> boot epoch, from the plan's live candidate set.
	eligible := make(map[string]int64, len(plan.Candidates))
	for _, c := range plan.Candidates {
		if c.Eligible {
			eligible[c.InstanceID] = c.BootEpoch
		}
	}
	if len(eligible) == 0 {
		// No executor can run this turn and control cannot run it itself.
		return "", false, false
	}

	// Sticky: keep the current home while it is still an eligible live
	// executor. A read failure is non-fatal — fall through to mint.
	if existing, err := h.homes.Get(ctx, orgID, projectID); err != nil {
		curatorLog.Warn("curator home lookup failed; re-resolving", "org", orgID, "project", projectID, "error", err)
	} else if existing != nil {
		if _, live := eligible[existing.HomeInstanceID]; live {
			return existing.HomeInstanceID, existing.HomeInstanceID != h.selfID, true
		}
	}

	// Mint / re-home to the rendezvous winner among eligible instances. Prefer
	// the plan's top preferred (which honors an eligible pin or hot-key order)
	// when it is eligible; otherwise fall to the top eligible rendezvous
	// candidate, so a pin to a since-dead instance never strands the session.
	winner := ""
	for _, id := range plan.PreferredSet {
		if _, ok := eligible[id]; ok {
			winner = id
			break
		}
	}
	if winner == "" {
		for _, c := range plan.Candidates {
			if c.Eligible {
				winner = c.InstanceID
				break
			}
		}
	}

	if err := h.homes.Upsert(ctx, orgID, projectID, winner, eligible[winner]); err != nil {
		// Persisting the mapping failed, but the placement decision stands —
		// route this turn to the winner anyway; the next turn re-attempts the
		// upsert. Worst case is a cold clone if the winner changes meanwhile.
		curatorLog.Warn("curator home upsert failed; routing turn without persisting home", "org", orgID, "project", projectID, "home", winner, "error", err)
	}
	return winner, winner != h.selfID, true
}
