package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
)

// placementResolver is the narrow view the spawner needs of
// *placement.Resolver (TFAC-587): report whether placement is on, and compute
// a plan for a key. Kept as an interface so a test can inject a fake and so a
// nil resolver reads cleanly as "placement off".
type placementResolver interface {
	Enabled() bool
	Resolve(ctx context.Context, orgID, keyKind, keyValue string) (placement.Plan, error)
}

// SetPlacement wires the placement affinity layer (TFAC-587): the resolver
// that computes the (org, repo) rendezvous stamp at enqueue, and the claim
// config the dispatcher passes to ClaimNextRun. Called once at startup
// (internal/app) before RunDispatcher starts. A nil resolver or a disabled
// claim leaves the whole layer off — enqueue stamps nothing and the claim
// runs the global-oldest path, byte-identical to the pre-placement
// dispatcher. Guarded by s.mu like the other startup-set seams.
func (s *Spawner) SetPlacement(resolver placementResolver, claim db.ClaimPlacement) {
	s.mu.Lock()
	s.placementResolver = resolver
	s.placementClaim = claim
	s.mu.Unlock()
}

// claimPlacement returns the claim-time placement config (nil-safe zero value
// = disabled). Read under s.mu, mirroring executorIdentity().
func (s *Spawner) claimPlacement() db.ClaimPlacement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.placementClaim
}

// preferredExecutorFor computes the placement stamp for a run about to be
// enqueued: the rendezvous winner for the run's (org, repo) key, spread
// across the top-K when a hot-key replica override is in effect. Empty (no
// affinity) whenever placement is off, the task has no resolvable GitHub repo
// (Jira/Slack tasks, or an unparseable source id), or the placement read
// fails — a failed or absent stamp only costs a cold clone, never a claim, so
// this never blocks minting work. runID is the id of the row being enqueued;
// it selects which replica owns the run under a K>1 override.
func (s *Spawner) preferredExecutorFor(ctx context.Context, orgID string, task domain.Task, runID string) string {
	s.mu.Lock()
	resolver := s.placementResolver
	s.mu.Unlock()
	if resolver == nil || !resolver.Enabled() {
		return ""
	}
	owner, repo := ownerRepoForTask(task)
	if owner == "" || repo == "" {
		return ""
	}
	plan, err := resolver.Resolve(ctx, orgID, domain.PlacementKindRepo, owner+"/"+repo)
	if err != nil {
		delegateLog.Warn("placement resolve failed; enqueuing with no affinity",
			"org", orgID, "repo", owner+"/"+repo, "error", err)
		return ""
	}
	return plan.PreferredForRun(runID)
}
