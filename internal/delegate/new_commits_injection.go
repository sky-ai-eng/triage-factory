// New-commits review-freshness injection (TFAC-501). The first producer on the generic
// staged-injection queue: when a PR a run is reviewing advances under it (a head-SHA
// change), the drafting agent is told — delivered live if its process is warm,
// else staged for the next resume — so it re-pulls and reconciles its pending
// review comments against the new diff before finalizing.
//
// This is the agent-facing half of the freshness loop TFAC-494's design calls
// out (the other half being the finalize gate that reconciles to the current
// head). It never blocks the blueprint (async sidecar, TFAC-379 #2): a behind
// agent gets an injection, not a park.

package delegate

import (
	"context"
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// HandlePRNewCommits is the eventbus handler for domain.EventGitHubPRNewCommits
// (wired in internal/app). For the PR whose head advanced, it finds every
// in-flight review draft anchored to that PR and, for each whose run can still
// receive an injection (live or resumable) AND is actually behind the new head, fires
// the freshness injection through the generic staged-injection queue (StageOrDeliverInjection:
// live → steer, parked → stage).
//
// Best-effort throughout: a parse/lookup failure is logged and dropped (the next
// head change re-diffs and re-fires), never fatal. Org-scoped by evt.OrgID.
func (s *Spawner) HandlePRNewCommits(evt domain.Event) {
	if evt.EventType != domain.EventGitHubPRNewCommits {
		return
	}
	if s.artifacts == nil || s.agentRuns == nil {
		return
	}
	var meta events.GitHubPRNewCommitsMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		delegateLog.Warn("new-commits injection: parse metadata failed", "event", evt.ID, "error", err)
		return
	}
	// Need the PR coordinates + the new head to name the injection and gate on
	// freshness. PrevHeadSHA is deliberately not guarded: the tracker emits
	// pr:new_commits only on a prev→curr head transition guarded by
	// prev.HeadSHA != "" (tracker/diff.go), so a reached event always carries it
	// and the "advanced from <old>" half of the copy is never blank.
	if meta.Repo == "" || meta.PRNumber == 0 || meta.HeadSHA == "" {
		return
	}

	ctx := context.Background()
	target := domain.ReviewTarget(meta.Repo, meta.PRNumber)
	reviews, err := s.artifacts.ListPendingReviewsByTargetSystem(ctx, evt.OrgID, target)
	if err != nil {
		delegateLog.Warn("new-commits injection: lookup pending reviews failed", "target", target, "error", err)
		return
	}

	for i := range reviews {
		a := reviews[i]
		if a.ConversationID == "" {
			continue // detached artifact (run purged) — no agent to tell
		}
		// Freshness gate: the agent isn't behind if its review already recorded
		// this head (the start anchor or a finalize-reconciled comment). Skip —
		// "no injection when the checkout is already at the new head".
		if domain.ReviewAnchoredAtHead(a, meta.HeadSHA) {
			continue
		}
		// Only deliver where an injection can land: a warm process (steered live)
		// or a conversation something is already coming back to
		// (injectionWillFlush). A conversation that concluded is skipped even
		// though a person could now wake it — a row that waits on a follow-up
		// that may never come is a leak, and the freshness notice is stale by
		// the time one arrives anyway.
		live := s.getProc(a.ConversationID) != nil
		if !live {
			run, err := s.agentRuns.GetSystem(ctx, evt.OrgID, a.ConversationID)
			if err != nil {
				delegateLog.Warn("new-commits injection: load run failed", "run", a.ConversationID, "error", err)
				continue
			}
			if run == nil || !injectionWillFlush(run.Status, run.Outcome) {
				continue
			}
		}
		body := domain.PRNewCommitsInjection(meta.Repo, meta.PRNumber, meta.PrevHeadSHA, meta.HeadSHA)
		s.StageOrDeliverInjection(evt.OrgID, a.ConversationID, domain.StagedInjectionProducerPRNewCommits, body)
	}
}
