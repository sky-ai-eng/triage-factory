// Package reconcile keeps the artifacts table in sync with external GitHub
// state — webhook-independent, both modes (TFAC-464). It is self-sufficient: it
// owns its fetches (RefreshPRs for PR + review state, a ref-existence probe for
// branches) rather than leaning on the tracker, whose snapshots aren't a
// reliable source (no branch discovery, throttled PR refreshes).
//
// The work splits into two tiers over one shared Reconciler:
//
//   - Tier 1 — a per-org background Manager/Runner (this package), kicked off
//     the system:poll: GitHub sentinel, mirroring scorer/profiler/classifier.
//     Each cycle reconciles the org's whole non-terminal artifact set.
//   - Tier 2 — a run-scoped refresh the frontend polls while a run view is
//     open, reconciling just that run's non-terminal artifacts (wired in
//     internal/server).
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// clientResolver is the slice of github.Resolver the reconciler needs — a
// per-(org, owner) GitHub client. github.Resolver satisfies it; tests pass a
// fake returning a stub-backed *github.Client.
type clientResolver interface {
	ClientFor(ctx context.Context, orgID, target string) (*github.Client, error)
}

// Reconciler mirrors artifacts against live GitHub state. One instance is
// shared between Tier 1 (the background Manager/Runner) and Tier 2 (the
// run-scoped refresh endpoint): both call Reconcile with a set of non-terminal
// artifacts. Writes route through the admin pool (UpsertSystem /
// AppendRunMemoryOutcomeSystem) — the reconciler has no JWT-claims context in
// either tier.
type Reconciler struct {
	resolver  clientResolver
	artifacts db.ArtifactStore
	memory    db.TaskMemoryStore
	ws        *websocket.Hub // nil-safe: broadcasts are skipped when unset (tests)
}

// NewReconciler builds the shared reconciler. ws may be nil (broadcasts become
// no-ops); the store + resolver are required.
func NewReconciler(resolver clientResolver, artifacts db.ArtifactStore, memory db.TaskMemoryStore, ws *websocket.Hub) *Reconciler {
	return &Reconciler{resolver: resolver, artifacts: artifacts, memory: memory, ws: ws}
}

// ReconcileOrg lists the org's reconcilable non-terminal artifacts (admin pool,
// org-wide) and reconciles them. The Tier-1 Runner's per-cycle body.
func (rc *Reconciler) ReconcileOrg(ctx context.Context, orgID string) error {
	arts, err := rc.artifacts.ListNonTerminalBySystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list non-terminal artifacts: %w", err)
	}
	if _, err := rc.Reconcile(ctx, orgID, arts); err != nil {
		return err
	}
	return nil
}

// Reconcile refreshes each artifact in arts against live GitHub and applies any
// state transition: PR draft/open/merged/closed, review submitted/dismissed,
// branch deleted. On a TERMINAL transition it appends a final-outcome note to
// the producing run's memory (β) and broadcasts the change over the WS hub (as
// agent_run_update on the owning run, so the run view's artifact-derived surface
// refreshes). Returns the artifacts that transitioned, carrying their new state.
//
// Best-effort per artifact: a single GitHub or write failure is logged and
// skipped, never aborting the rest. GitHub work is grouped by repo OWNER —
// RefreshPRs / BranchesExist key off a per-owner credential — and batched, so a
// cycle is a handful of GraphQL calls regardless of artifact count.
func (rc *Reconciler) Reconcile(ctx context.Context, orgID string, arts []domain.Artifact) ([]domain.Artifact, error) {
	if len(arts) == 0 {
		return nil, nil
	}

	// Pass 1 — group the GitHub fetches by owner. PR and review artifacts both
	// refresh from a PR node id (stored in details); branch artifacts probe a
	// ref. An artifact missing its handle (no node id, malformed target) can't
	// be refreshed and is left for a later cycle once a writer fills it in.
	prNodeIDsByOwner := map[string]map[string]bool{}
	branchRefsByOwner := map[string][]github.BranchRef{}
	for _, a := range arts {
		switch a.Kind {
		case domain.ArtifactKindPullRequest:
			if owner, ok := prOwner(a.Target); ok {
				if d, _ := domain.ParsePRArtifactDetails(a.DetailsJSON); d.NodeID != "" {
					addNodeID(prNodeIDsByOwner, owner, d.NodeID)
				}
			}
		case domain.ArtifactKindReview:
			if owner, ok := prOwner(a.Target); ok {
				if d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON); d.NodeID != "" {
					addNodeID(prNodeIDsByOwner, owner, d.NodeID)
				}
			}
		case domain.ArtifactKindBranch:
			if ref, ok := branchRefOf(a); ok {
				branchRefsByOwner[ref.Owner] = append(branchRefsByOwner[ref.Owner], ref)
			}
		}
	}

	// Pass 2 — one fetch per owner (PRs + branches), tolerant of a per-owner
	// credential/network failure (skip that owner's artifacts this cycle). PR
	// node ids are globally unique, so snapshots merge safely across owners.
	snapshots := map[string]domain.PRSnapshot{}
	branchExists := map[github.BranchRef]bool{}
	for _, owner := range ownerUnion(prNodeIDsByOwner, branchRefsByOwner) {
		client, err := rc.resolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			reconcileLog.Warn("resolve github client failed; skipping owner this cycle",
				"org", orgID, "owner", owner, "error", err)
			continue
		}
		if ids := sortedKeys(prNodeIDsByOwner[owner]); len(ids) > 0 {
			// includeCheckRuns=false: reconciliation needs PR lifecycle + reviews,
			// not CI — the lighter discovery fragment still carries both.
			snaps, err := client.RefreshPRs(ids, false)
			if err != nil {
				reconcileLog.Warn("refresh PRs failed", "org", orgID, "owner", owner, "count", len(ids), "error", err)
			} else {
				for k, v := range snaps {
					snapshots[k] = v
				}
			}
		}
		if refs := branchRefsByOwner[owner]; len(refs) > 0 {
			ex, err := client.BranchesExist(refs)
			if err != nil {
				reconcileLog.Warn("branch existence check failed", "org", orgID, "owner", owner, "count", len(refs), "error", err)
			} else {
				for k, v := range ex {
					branchExists[k] = v
				}
			}
		}
	}

	// Pass 3 — apply transitions.
	var transitioned []domain.Artifact
	for _, a := range arts {
		newState, ok := nextState(a, snapshots, branchExists)
		if !ok || newState == a.State {
			continue
		}
		updated, err := rc.applyTransition(ctx, orgID, a, newState)
		if err != nil {
			reconcileLog.Warn("apply artifact transition failed",
				"org", orgID, "artifact", a.ID, "kind", a.Kind, "from", a.State, "to", newState, "error", err)
			continue
		}
		transitioned = append(transitioned, updated)
	}
	return transitioned, nil
}

// applyTransition writes the new state (admin pool), captures the final-outcome
// memory on a terminal transition, and broadcasts the change. The memory + WS
// steps are best-effort: the external state already moved, so a failed note or
// a dropped broadcast must not undo the recorded transition.
func (rc *Reconciler) applyTransition(ctx context.Context, orgID string, a domain.Artifact, newState string) (domain.Artifact, error) {
	next := a
	next.State = newState
	updated, err := rc.artifacts.UpsertSystem(ctx, orgID, next)
	if err != nil {
		return domain.Artifact{}, err
	}

	if a.RunID != "" && isTerminalState(a.Kind, newState) {
		if note := outcomeNote(a, newState); note != "" {
			if merr := rc.memory.AppendRunMemoryOutcomeSystem(ctx, orgID, a.RunID, note); merr != nil {
				reconcileLog.Warn("append outcome memory failed",
					"org", orgID, "run", a.RunID, "artifact", a.ID, "error", merr)
			}
		}
	}

	rc.broadcast(orgID, updated)
	reconcileLog.Info("artifact reconciled",
		"org", orgID, "artifact", a.ID, "kind", a.Kind, "from", a.State, "to", newState)
	return updated, nil
}

// broadcast pushes the transition to the frontend as an agent_run_update on the
// owning run — the run view already refetches the run (and its artifact-derived
// pending surface) on that event, so no new event type or FE handler is needed.
// Skipped for a detached artifact (no run to address) or when the hub is unset.
func (rc *Reconciler) broadcast(orgID string, a domain.Artifact) {
	if rc.ws == nil || a.RunID == "" {
		return
	}
	rc.ws.Broadcast(websocket.Event{
		Type:  "agent_run_update",
		OrgID: orgID,
		RunID: a.RunID,
		Data:  map[string]any{"artifact_id": a.ID, "artifact_state": a.State},
	})
}

// nextState computes the artifact's reconciled state from the fetched GitHub
// data, returning ok=false when there's no confident answer (snapshot missing,
// review not yet surfaced, branch existence unknown) so the caller leaves the
// row untouched. A returned state equal to the current one is a no-op.
func nextState(a domain.Artifact, snapshots map[string]domain.PRSnapshot, branchExists map[github.BranchRef]bool) (string, bool) {
	switch a.Kind {
	case domain.ArtifactKindPullRequest:
		d, _ := domain.ParsePRArtifactDetails(a.DetailsJSON)
		snap, ok := snapshots[d.NodeID]
		if !ok {
			return "", false
		}
		return prState(snap), true

	case domain.ArtifactKindReview:
		d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
		snap, ok := snapshots[d.NodeID]
		if !ok {
			return "", false
		}
		return reviewState(snap, a.ExternalID)

	case domain.ArtifactKindBranch:
		ref, ok := branchRefOf(a)
		if !ok {
			return "", false
		}
		exists, known := branchExists[ref]
		if !known {
			return "", false // unknown (repo inaccessible) — never mark deleted on a non-answer
		}
		if exists {
			return domain.ArtifactStateBranchPushed, true // still there — no-op
		}
		return domain.ArtifactStateBranchDeleted, true
	}
	return "", false
}

// prState maps a PR snapshot onto the artifact lifecycle. GitHub's PR.state is
// OPEN/CLOSED/MERGED; merged is also flagged explicitly, checked first so a
// merged PR never reads as merely closed.
func prState(snap domain.PRSnapshot) string {
	switch {
	case snap.Merged || strings.EqualFold(snap.State, "MERGED"):
		return domain.ArtifactStatePRMerged
	case strings.EqualFold(snap.State, "CLOSED"):
		return domain.ArtifactStatePRClosed
	case snap.IsDraft:
		return domain.ArtifactStatePRDraft
	default:
		return domain.ArtifactStatePROpen
	}
}

// reviewState maps the bot's review (matched by its node id in the PR's latest
// reviews) onto the artifact lifecycle. A pending review is private until
// submitted, so the bot's own pending review either isn't surfaced here or
// reads as PENDING — both leave the artifact pending (ok=false / no-op). Only a
// positive match with a terminal review state transitions the row, so a missing
// review never flips a still-pending artifact to dismissed.
func reviewState(snap domain.PRSnapshot, reviewID string) (string, bool) {
	if reviewID == "" {
		return "", false
	}
	for _, rv := range snap.Reviews {
		if rv.ID != reviewID {
			continue
		}
		switch strings.ToUpper(rv.State) {
		case "DISMISSED":
			return domain.ArtifactStateReviewDismissed, true
		case "PENDING", "":
			return "", false // still pending/private — leave as-is
		default: // APPROVED, CHANGES_REQUESTED, COMMENTED
			return domain.ArtifactStateReviewSubmitted, true
		}
	}
	return "", false
}

// isTerminalState reports whether (kind, state) is a terminal lifecycle position
// — the transitions that warrant a final-outcome memory note. Branch deleted, PR
// merged/closed, review submitted/dismissed.
func isTerminalState(kind, state string) bool {
	switch kind {
	case domain.ArtifactKindPullRequest:
		return state == domain.ArtifactStatePRMerged || state == domain.ArtifactStatePRClosed
	case domain.ArtifactKindReview:
		return state == domain.ArtifactStateReviewSubmitted || state == domain.ArtifactStateReviewDismissed
	case domain.ArtifactKindBranch:
		return state == domain.ArtifactStateBranchDeleted
	}
	return false
}

// outcomeNote builds the markdown line appended to the producing run's memory on
// a terminal transition (TFAC-464 β), so the next agent on the entity sees how
// the artifact ended. Empty for a non-terminal transition (no note warranted).
func outcomeNote(a domain.Artifact, newState string) string {
	switch {
	case a.Kind == domain.ArtifactKindPullRequest && newState == domain.ArtifactStatePRMerged:
		return fmt.Sprintf("**Outcome:** `%s` was merged on GitHub.", a.Target)
	case a.Kind == domain.ArtifactKindPullRequest && newState == domain.ArtifactStatePRClosed:
		return fmt.Sprintf("**Outcome:** `%s` was closed without merging on GitHub.", a.Target)
	case a.Kind == domain.ArtifactKindReview && newState == domain.ArtifactStateReviewSubmitted:
		return fmt.Sprintf("**Outcome:** The review on `%s` was submitted on GitHub.", a.Target)
	case a.Kind == domain.ArtifactKindReview && newState == domain.ArtifactStateReviewDismissed:
		return fmt.Sprintf("**Outcome:** The review on `%s` was dismissed on GitHub.", a.Target)
	case a.Kind == domain.ArtifactKindBranch && newState == domain.ArtifactStateBranchDeleted:
		return fmt.Sprintf("**Outcome:** Branch `%s` in `%s` was deleted on GitHub.", branchName(a.ExternalID), a.Target)
	}
	return ""
}

// --- coordinate helpers ---

// prOwner extracts the repo owner from a PR/review artifact target
// (owner/repo#number). ok=false on a malformed target.
func prOwner(target string) (string, bool) {
	owner, _, _, ok := domain.ParsePRTarget(target)
	return owner, ok
}

// branchRefOf derives the GitHub coordinates of a branch artifact from its
// Target (owner/repo) and ExternalID (refs/heads/<branch>). ok=false when either
// is malformed — the row is skipped rather than probed against a guessed ref.
func branchRefOf(a domain.Artifact) (github.BranchRef, bool) {
	parts := strings.SplitN(a.Target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return github.BranchRef{}, false
	}
	branch, ok := strings.CutPrefix(a.ExternalID, "refs/heads/")
	if !ok || branch == "" {
		return github.BranchRef{}, false
	}
	return github.BranchRef{Owner: parts[0], Repo: parts[1], Branch: branch}, true
}

// branchName strips the refs/heads/ prefix for human-facing copy; returns the
// input unchanged if it isn't a branch ref.
func branchName(ref string) string {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b
	}
	return ref
}

func addNodeID(m map[string]map[string]bool, owner, id string) {
	if m[owner] == nil {
		m[owner] = map[string]bool{}
	}
	m[owner][id] = true
}

// sortedKeys returns the set's keys in a stable order so a batch's node-id list
// (and thus the GraphQL query text) is deterministic — keeps tests reproducible
// and any upstream caching keyed predictably.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ownerUnion returns every distinct owner across the PR and branch work maps,
// sorted for deterministic per-owner iteration.
func ownerUnion(prByOwner map[string]map[string]bool, branchByOwner map[string][]github.BranchRef) []string {
	seen := map[string]bool{}
	for o := range prByOwner {
		seen[o] = true
	}
	for o := range branchByOwner {
		seen[o] = true
	}
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}
