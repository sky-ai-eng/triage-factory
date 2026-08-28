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
//   - Tier 2 — a conversation-scoped refresh the frontend polls while a
//     conversation view is open, reconciling just that conversation's
//     non-terminal artifacts (wired in internal/server).
package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/conversationevent"
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
// conversation-scoped refresh endpoint): both call Reconcile with a set of
// non-terminal artifacts. Writes route through the admin pool (UpsertSystem /
// UpdateConversationMemoryHumanContentSystem) — the reconciler has no JWT-claims context
// in either tier.
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
// org-wide) and reconciles them, then runs the gh-channel PR-artifact backstop.
// The Tier-1 Runner's per-cycle body.
func (rc *Reconciler) ReconcileOrg(ctx context.Context, orgID string) error {
	arts, err := rc.artifacts.ListNonTerminalBySystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list non-terminal artifacts: %w", err)
	}
	if _, err := rc.Reconcile(ctx, orgID, arts); err != nil {
		return err
	}
	// gh-channel backstop: record PRs a conversation created via the real gh
	// whose injector observation was lost (channel severed, or the create raced
	// a crash). Best-effort — a failure here never aborts the reconcile cycle.
	if err := rc.BackfillPRArtifactsForBranches(ctx, orgID, arts); err != nil {
		reconcileLog.Warn("gh-channel PR backstop failed", "org", orgID, "error", err)
	}
	return nil
}

// BackfillPRArtifactsForBranches is the gh-channel PR-artifact backstop
// (TFAC-669). Exec-verb self-reporting doesn't exist on the real-gh channel, so
// a PR created there is normally recorded by the injector's observation relay;
// this covers the two cases that path can't — the observation channel severed,
// or the create raced an executor crash. For each conversation that pushed a
// branch (its git:branch artifact carries the full ref), it discovers the open
// PR on that branch and records the pull_request artifact via the insert-if-
// absent write. Idempotent by construction: a PR the observation path (or a
// prior pass) already recorded is left untouched, so re-running changes nothing.
//
// arts is the org's already-listed non-terminal set (branch artifacts included),
// passed in by ReconcileOrg to avoid a second list; a nil arts makes this
// self-list (the boot pass). Best-effort per repo — a per-owner credential or
// GitHub failure skips that repo this pass.
func (rc *Reconciler) BackfillPRArtifactsForBranches(ctx context.Context, orgID string, arts []domain.Artifact) error {
	if arts == nil {
		var err error
		arts, err = rc.artifacts.ListNonTerminalBySystem(ctx, orgID)
		if err != nil {
			return fmt.Errorf("list non-terminal artifacts: %w", err)
		}
	}

	// (owner/repo) → (branch → the conversation that pushed it). Only pushed
	// branch artifacts anchored to a live conversation are candidates.
	type convRef struct{ conversationID, teamID string }
	byRepo := map[string]map[string]convRef{}
	for _, a := range arts {
		if a.Kind != domain.ArtifactKindBranch || a.State != domain.ArtifactStateBranchPushed {
			continue
		}
		if a.ConversationID == "" || a.Target == "" {
			continue // detached from its conversation, or malformed — can't attribute a PR
		}
		branch, ok := strings.CutPrefix(a.ExternalID, "refs/heads/")
		if !ok {
			continue
		}
		if byRepo[a.Target] == nil {
			byRepo[a.Target] = map[string]convRef{}
		}
		byRepo[a.Target][branch] = convRef{conversationID: a.ConversationID, teamID: a.TeamID}
	}
	if len(byRepo) == 0 {
		return nil
	}

	for repoPath, byBranch := range byRepo {
		owner, repo, ok := strings.Cut(repoPath, "/")
		if !ok || owner == "" || repo == "" {
			continue
		}
		client, err := rc.resolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			reconcileLog.Warn("backstop: resolve github client failed; skipping repo this pass",
				"org", orgID, "repo", repoPath, "error", err)
			continue
		}
		prs, _, _, err := client.ListOpenPRs(ctx, owner, repo, "")
		if err != nil {
			reconcileLog.Warn("backstop: list open PRs failed; skipping repo this pass",
				"org", orgID, "repo", repoPath, "error", err)
			continue
		}
		for _, pr := range prs {
			ref, matched := byBranch[pr.Snapshot.HeadRef]
			if !matched {
				continue // no conversation of this org pushed this PR's head branch
			}
			art := domain.NewPullRequestArtifact(repoPath, pr.Snapshot.Number, pr.NodeID,
				pr.Snapshot.HeadRef, pr.Snapshot.BaseRef, pr.Snapshot.URL, pr.Snapshot.Title, "", pr.Snapshot.IsDraft)
			art.ConversationID = ref.conversationID
			art.OrgID = orgID
			art.TeamID = ref.teamID
			inserted, err := rc.artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, art)
			if err != nil {
				reconcileLog.Warn("backstop: record PR artifact failed",
					"org", orgID, "repo", repoPath, "pr", pr.Snapshot.Number, "error", err)
				continue
			}
			if inserted {
				reconcileLog.Info("backstop: recorded PR artifact from branch match",
					"org", orgID, "repo", repoPath, "pr", pr.Snapshot.Number, "conversation", ref.conversationID)
			}
		}
	}
	return nil
}

// Reconcile refreshes each artifact in arts against live GitHub and applies any
// state transition: PR draft/open/merged/closed, review submitted/dismissed,
// branch deleted. On a TERMINAL transition it appends a final-outcome note to
// the producing conversation's memory (β) and broadcasts the change over the WS
// hub (as artifact_updated on the owning conversation, so the conversation
// view's artifact-derived surface refreshes). Returns the artifacts that
// transitioned, carrying their new state.
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
			snaps, err := client.RefreshPRs(ctx, ids, false)
			if err != nil {
				reconcileLog.Warn("refresh PRs failed", "org", orgID, "owner", owner, "count", len(ids), "error", err)
			} else {
				for k, v := range snaps {
					snapshots[k] = v
				}
			}
		}
		if refs := branchRefsByOwner[owner]; len(refs) > 0 {
			ex, err := client.BranchesExist(ctx, refs)
			if err != nil {
				reconcileLog.Warn("branch existence check failed", "org", orgID, "owner", owner, "count", len(refs), "error", err)
			} else {
				for k, v := range ex {
					branchExists[k] = v
				}
			}
		}
	}

	// Pass 3 — apply each transition, then recompute conversation memory once
	// per conversation that had a TERMINAL transition this cycle. The memory
	// recompute is deferred out of the per-artifact loop and composed over the
	// conversation's whole artifact set (recordConversationOutcome) so a
	// conversation that produced several artifacts — commonly a branch AND a
	// PR — accumulates one outcome rather than each terminal write clobbering
	// the last.
	//
	// The write-back runs on a detached ctx (the fetches above stayed
	// cancellable). A terminal artifact drops out of BOTH tiers' non-terminal
	// working sets the moment applyTransition commits it, so if a client
	// disconnect (Tier-2) or shutdown (Tier-1) cancelled the state write's
	// follow-up memory note, no later cycle would re-process it — the note would
	// be lost, not self-corrected. Detaching keeps the state + memory writes
	// atomic w.r.t. the caller's lifecycle, mirroring the approval handlers'
	// post-action bookkeeping; the github http.Client's own 30s timeout still
	// bounds each call.
	writeCtx := context.WithoutCancel(ctx)
	var transitioned []domain.Artifact
	terminalConversations := map[string]bool{}
	for _, a := range arts {
		newState, ok := nextState(a, snapshots, branchExists)
		if !ok || newState == a.State {
			continue
		}
		updated, err := rc.applyTransition(writeCtx, orgID, a, newState)
		if err != nil {
			reconcileLog.Warn("apply artifact transition failed",
				"org", orgID, "artifact", a.ID, "kind", a.Kind, "from", a.State, "to", newState, "error", err)
			continue
		}
		transitioned = append(transitioned, updated)
		if a.ConversationID != "" && isTerminalState(a.Kind, newState) {
			terminalConversations[a.ConversationID] = true
		}
	}
	for conversationID := range terminalConversations {
		rc.recordConversationOutcome(writeCtx, orgID, conversationID)
	}
	return transitioned, nil
}

// applyTransition writes the new state (admin pool) and broadcasts the change.
// Memory capture is deferred to recordConversationOutcome (a per-conversation
// recompute) so a conversation with several artifacts gets one composed outcome,
// not one clobbering write per artifact. Best-effort WS: a dropped broadcast
// must not undo the transition.
func (rc *Reconciler) applyTransition(ctx context.Context, orgID string, a domain.Artifact, newState string) (domain.Artifact, error) {
	next := a
	next.State = newState
	updated, err := rc.artifacts.UpsertSystem(ctx, orgID, next)
	if err != nil {
		return domain.Artifact{}, err
	}
	rc.broadcast(orgID, updated)
	reconcileLog.Info("artifact reconciled",
		"org", orgID, "artifact", a.ID, "kind", a.Kind, "from", a.State, "to", newState)
	return updated, nil
}

// recordConversationOutcome recomputes a conversation's final-outcome memory
// note over its WHOLE artifact set and OVERWRITES human_content with it. Called
// once per conversation that had a terminal transition this cycle, after the
// artifact writes commit (so it reads the fresh terminal states). Composing over
// the full set — not just this cycle's transitions — is what lets a
// branch-then-PR conversation accumulate without an append: every resolution
// recomputes the complete picture, which is also why a repeated or concurrent
// cycle is idempotent (same set → same note). The
// overwrite supersedes any approval-time verdict by design; the terminal state
// is the authoritative account of how reality diverged from the agent's draft.
// Best-effort — the external state already moved, so a failed note must not
// undo the transition.
func (rc *Reconciler) recordConversationOutcome(ctx context.Context, orgID, conversationID string) {
	note := rc.composeConversationOutcome(ctx, orgID, conversationID)
	if note == "" {
		return
	}
	if _, err := rc.memory.UpdateConversationMemoryHumanContentSystem(ctx, orgID, conversationID, note); err != nil {
		reconcileLog.Warn("record conversation outcome memory failed", "org", orgID, "conversation", conversationID, "error", err)
	}
}

// broadcast pushes the transition to the frontend as a dedicated artifact_updated
// event on the owning conversation. It deliberately does NOT reuse conversation_update:
// that event's consumers (the Board) optimistically write the conversation's
// Status from data.status, so a payload carrying no real status would blank the
// card until a refetch. The conversation's own status is unchanged here — only its
// artifact-derived surface (pending kind / approval card) is — so the FE handlers
// refetch the conversation on this event without touching status. Skipped for a
// detached artifact (no conversation) or unset hub.
func (rc *Reconciler) broadcast(orgID string, a domain.Artifact) {
	if rc.ws == nil || a.ConversationID == "" {
		return
	}
	rc.ws.Broadcast(websocket.Event{
		Type:           "artifact_updated",
		OrgID:          orgID,
		ConversationID: a.ConversationID,
		Data:           map[string]any{"artifact_id": a.ID, "state": a.State},
	})
	// And the resource-wide ping beside it, for the counters that follow a SET
	// rather than one conversation. The event above is addressed to whoever is
	// watching this conversation and carries what changed on it; the shell rail
	// is watching neither, and a transition here can be the whole difference
	// between "waiting on a person" and "done" — a draft PR merged on GitHub
	// resolves without any surface of ours writing a thing.
	conversationevent.Publish(rc.ws, orgID)
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

// outcomeNoteHeader prefaces the composed final-outcome memory note — the
// framing the next agent reads the per-artifact outcome blocks under. The note
// text itself says "run" deliberately: it is agent-facing prose, and "run" is
// the legible word for the engagement whose work resolved.
const outcomeNoteHeader = "**Post-run outcome** — how your work resolved on GitHub versus what you drafted:\n\n"

// composeConversationOutcome builds the conversation's final-outcome memory note
// from every TERMINAL artifact it produced. It reads the conversation's whole
// artifact set (admin pool — the reconciler has no claims), so the note is the
// complete picture regardless of which cycle each artifact resolved in. Returns
// "" when nothing is terminal yet (still in flight — no outcome to report),
// which recordConversationOutcome treats as "don't touch the row."
func (rc *Reconciler) composeConversationOutcome(ctx context.Context, orgID, conversationID string) string {
	arts, err := rc.artifacts.ListByConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		reconcileLog.Warn("list conversation artifacts for outcome note failed", "org", orgID, "conversation", conversationID, "error", err)
		return ""
	}
	var blocks []string
	for _, a := range arts {
		if block := rc.describeArtifactOutcome(ctx, orgID, a); block != "" {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return outcomeNoteHeader + strings.Join(blocks, "\n\n")
}

// describeArtifactOutcome renders one terminal artifact's outcome block, or ""
// when the artifact isn't terminal (still in flight — nothing final to report).
// A merged PR is diffed against the agent's authored draft (Proposed) so the
// note carries how the shipped title/description differs; every other terminal
// kind renders its disposition.
func (rc *Reconciler) describeArtifactOutcome(ctx context.Context, orgID string, a domain.Artifact) string {
	switch {
	case a.Kind == domain.ArtifactKindPullRequest && a.State == domain.ArtifactStatePRMerged:
		return rc.describeMergedPR(ctx, orgID, a)
	case a.Kind == domain.ArtifactKindPullRequest && a.State == domain.ArtifactStatePRClosed:
		return fmt.Sprintf("`%s` was closed without merging on GitHub.", a.Target)
	case a.Kind == domain.ArtifactKindReview && a.State == domain.ArtifactStateReviewSubmitted:
		return rc.describeResolvedReview(ctx, orgID, a, "submitted")
	case a.Kind == domain.ArtifactKindReview && a.State == domain.ArtifactStateReviewDismissed:
		return rc.describeResolvedReview(ctx, orgID, a, "dismissed")
	case a.Kind == domain.ArtifactKindBranch && a.State == domain.ArtifactStateBranchDeleted:
		return fmt.Sprintf("Branch `%s` in `%s` was deleted on GitHub.", branchName(a.ExternalID), a.Target)
	}
	return ""
}

// describeMergedPR reports a merged PR and diffs the SHIPPED title/body (read
// live via GetPRBasic) against the agent's authored draft (Proposed), so the
// note tells the next agent how the merged result differs from what it wrote.
// A fetch/parse failure degrades to the bare disposition rather than dropping
// the line.
//
// This captures the title/description divergence — the cheap delta that
// subsumes the approval-time verdict (same fields, more final). The deeper code
// divergence — commits that landed on top of the agent's — is a follow-up that
// needs a base..head diff against the authored head SHA.
func (rc *Reconciler) describeMergedPR(ctx context.Context, orgID string, a domain.Artifact) string {
	base := fmt.Sprintf("`%s` was merged on GitHub.", a.Target)
	owner, repo, number, ok := domain.ParsePRTarget(a.Target)
	if !ok {
		return base
	}
	client, err := rc.resolver.ClientFor(ctx, orgID, owner)
	if err != nil {
		return base
	}
	pr, err := client.GetPRBasic(ctx, owner, repo, number)
	if err != nil || pr == nil {
		return base
	}
	d, _ := domain.ParsePRArtifactDetails(a.DetailsJSON)
	delta := formatTitleBodyDelta(d.Proposed.Title, d.Proposed.Body, pr.Title, pr.Body)
	if delta == "" {
		return base + " Its title and description shipped as you drafted them."
	}
	return base + "\n" + delta
}

// formatTitleBodyDelta renders how a PR's shipped title/body differs from the
// agent's draft, or "" when both are unchanged. Only changed fields appear; the
// shipped body is quoted in full (it's the current truth the next agent acts on).
func formatTitleBodyDelta(draftTitle, draftBody, finalTitle, finalBody string) string {
	var b strings.Builder
	if draftTitle != finalTitle {
		fmt.Fprintf(&b, "- **Title** shipped as %q (you drafted %q).\n", finalTitle, draftTitle)
	}
	if draftBody != finalBody {
		b.WriteString("- **Description** was edited before it shipped. Final:\n\n")
		writeBlockquote(&b, finalBody)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeBlockquote prefixes each line of text with "> " for a markdown
// blockquote. CRLF-normalizes (GitHub web/Windows clients deliver "\r\n") and
// trims trailing newlines first, so an empty or trailing-newline body doesn't
// emit a stray "> " line; an empty body writes nothing.
func writeBlockquote(b *strings.Builder, text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// describeResolvedReview reports a review that resolved on GitHub (submitted or
// dismissed) and diffs what landed against the agent's authored draft (the
// artifact's Proposed snapshot), fetching the final review by its node id. A
// fetch/parse failure degrades to the bare disposition rather than dropping the
// line. The framing is the reconciler's ("resolved on GitHub"), distinct from
// the approval path's human-action framing — a different actor resolved it.
func (rc *Reconciler) describeResolvedReview(ctx context.Context, orgID string, a domain.Artifact, disposition string) string {
	base := fmt.Sprintf("Your review on `%s` was %s on GitHub.", a.Target, disposition)
	owner, _, _, ok := domain.ParsePRTarget(a.Target)
	if !ok || a.ExternalID == "" {
		return base
	}
	client, err := rc.resolver.ClientFor(ctx, orgID, owner)
	if err != nil {
		return base
	}
	final, err := client.GetReview(ctx, a.ExternalID)
	if err != nil || final == nil {
		return base
	}
	d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
	if body := formatReviewDelta(d.Proposed, *final); body != "" {
		return base + "\n" + body
	}
	return base
}

// formatReviewDelta renders how a resolved review differs from the agent's
// drafted review. With no agent draft to diff against (the review was submitted
// before TF finalized it, so Proposed is empty) it records the final review
// content instead, so the next agent still sees what was reviewed. Returns ""
// only when there's a draft and nothing changed — the bare disposition suffices.
func formatReviewDelta(proposed domain.ReviewArtifactProposed, final github.SubmittedReview) string {
	hasDraft := len(proposed.Comments) > 0 || strings.TrimSpace(proposed.Body) != "" || proposed.Event != ""
	if !hasDraft {
		return formatFinalReview(final)
	}
	return formatReviewDiff(proposed, final)
}

// formatFinalReview records a resolved review's content verbatim — used when
// there's no agent draft to diff against.
func formatFinalReview(final github.SubmittedReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verdict: %s.", reviewVerdict(final.State))
	if body := strings.TrimSpace(final.Body); body != "" {
		b.WriteString("\n\n")
		writeBlockquote(&b, body)
	}
	for _, c := range final.Comments {
		_, clean := domain.ParseSeverityBadge(c.Body)
		fmt.Fprintf(&b, "\n- %s — %s", commentLoc(c.Path, derefLine(c.Line)), inlineComment(clean))
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatReviewDiff renders the difference between the agent's drafted review and
// what landed: a changed verdict, an edited body, and per-comment edits/drops/
// adds (joined by GraphQL node id, severity badges stripped so the diff is
// prose). Returns "" when nothing changed.
func formatReviewDiff(proposed domain.ReviewArtifactProposed, final github.SubmittedReview) string {
	var b strings.Builder

	if proposed.Event != "" && !verdictMatches(proposed.Event, final.State) {
		fmt.Fprintf(&b, "Verdict shipped as %s (you drafted %s).\n", reviewVerdict(final.State), strings.ToLower(proposed.Event))
	}
	if strings.TrimSpace(proposed.Body) != strings.TrimSpace(final.Body) {
		b.WriteString("Body was edited before it shipped. Final:\n\n")
		writeBlockquote(&b, final.Body)
		b.WriteByte('\n')
	}
	for _, line := range diffReviewComments(proposed.Comments, final.Comments) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// diffReviewComments classifies the agent's drafted comments against the final
// set by GraphQL node id — the same join the approval-time editor uses — into
// dropped / edited / added bullet lines. Unchanged comments are omitted.
func diffReviewComments(proposed []domain.ReviewArtifactComment, final []github.PendingReviewComment) []string {
	type cmt struct {
		path, body string
		line       int
	}
	pm, pOrder := map[string]cmt{}, []string{}
	for _, p := range proposed {
		if p.ID == "" {
			continue
		}
		_, clean := domain.ParseSeverityBadge(p.Body)
		if _, seen := pm[p.ID]; !seen {
			pOrder = append(pOrder, p.ID)
		}
		pm[p.ID] = cmt{p.Path, clean, derefLine(p.Line)}
	}
	fm := map[string]cmt{}
	var fOrder []string
	for _, f := range final {
		if f.ID == "" {
			continue
		}
		_, clean := domain.ParseSeverityBadge(f.Body)
		if _, seen := fm[f.ID]; !seen {
			fOrder = append(fOrder, f.ID)
		}
		fm[f.ID] = cmt{f.Path, clean, derefLine(f.Line)}
	}

	var out []string
	for _, id := range pOrder {
		p := pm[id]
		f, ok := fm[id]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("- %s — dropped before submit (you wrote: %s)", commentLoc(p.path, p.line), inlineComment(p.body)))
		case p.body != f.body:
			out = append(out, fmt.Sprintf("- %s — edited before submit. Final: %s (you wrote: %s)", commentLoc(p.path, p.line), inlineComment(f.body), inlineComment(p.body)))
		}
	}
	for _, id := range fOrder {
		if _, drafted := pm[id]; drafted {
			continue
		}
		f := fm[id]
		out = append(out, fmt.Sprintf("- %s — added before submit: %s", commentLoc(f.path, f.line), inlineComment(f.body)))
	}
	return out
}

// commentLoc renders an inline comment's location. A line of 0 — GitHub returns
// a null line for a comment no longer anchored on the current diff — reads as
// "(outdated)" rather than a meaningless ":0".
func commentLoc(path string, line int) string {
	if line <= 0 {
		return fmt.Sprintf("`%s` (outdated)", path)
	}
	return fmt.Sprintf("`%s:%d`", path, line)
}

// reviewVerdict maps a GitHub review state to friendly prose.
func reviewVerdict(state string) string {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes requested"
	case "COMMENTED":
		return "comment"
	case "DISMISSED":
		return "dismissed"
	default:
		return strings.ToLower(state)
	}
}

// verdictMatches reports whether the agent's drafted review event (APPROVE /
// REQUEST_CHANGES / COMMENT) equals the final GitHub review state (which uses a
// past-tense vocabulary), normalizing across the two.
func verdictMatches(draftEvent, finalState string) bool {
	norm := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}
	want, ok := norm[strings.ToUpper(draftEvent)]
	if !ok {
		want = strings.ToUpper(draftEvent)
	}
	return want == strings.ToUpper(finalState)
}

// inlineComment collapses a comment body to a single trimmed line for prose use.
func inlineComment(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// derefLine returns the pointed-to line, or 0 for a comment with no anchor on
// the current diff (GitHub returns null line for an outdated comment).
func derefLine(p *int) int {
	if p == nil {
		return 0
	}
	return *p
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
