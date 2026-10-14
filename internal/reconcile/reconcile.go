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
	"time"

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
// non-terminal artifacts. Writes route through the admin pool (UpsertSystem) —
// the reconciler has no JWT-claims context in either tier.
//
// The resolver's clients reach the org's current GitHub host and no other, so
// only artifacts recorded on that host (their scope) are reconciled. Another
// host's pull request or branch is not where those clients look, and a branch
// missing there would read as deleted.
type Reconciler struct {
	resolver  clientResolver
	artifacts db.ArtifactStore
	actions   db.ExternalActionStore // read for the pr_created rows the backstop works from
	orgs      db.OrgsStore           // read for the org's current GitHub host
	ws        *websocket.Hub         // nil-safe: broadcasts are skipped when unset (tests)
	// prResolved runs after a pull request's draft → open transition commits.
	// nil skips it (tests, and the window before the spawner is wired).
	prResolved PullRequestResolvedHook
}

// PullRequestResolvedHook is what the reconciler calls when a draft pull
// request became ready somewhere other than the approval click — a human on
// GitHub, or an agent whose mission asked for it. The click runs the
// terminal-on-last task closure itself; this is how the same closure reaches a
// resolution nobody clicked, so a task does not stay open asking for an
// approval nobody has left to give.
type PullRequestResolvedHook func(ctx context.Context, orgID, conversationID string)

// NewReconciler builds the shared reconciler. ws may be nil (broadcasts become
// no-ops); the stores + resolver are required.
func NewReconciler(resolver clientResolver, artifacts db.ArtifactStore, actions db.ExternalActionStore, orgs db.OrgsStore, ws *websocket.Hub) *Reconciler {
	return &Reconciler{resolver: resolver, artifacts: artifacts, actions: actions, orgs: orgs, ws: ws}
}

// onCurrentHost returns the org's current GitHub host and the artifacts of
// arts recorded on it.
func (rc *Reconciler) onCurrentHost(ctx context.Context, orgID string, arts []domain.Artifact) (string, []domain.Artifact, error) {
	host, err := db.OrgGitHubHostSystem(ctx, rc.orgs, orgID)
	if err != nil {
		return "", nil, err
	}
	out := make([]domain.Artifact, 0, len(arts))
	for _, a := range arts {
		if a.Scope == host {
			out = append(out, a)
		}
	}
	return host, out, nil
}

// SetPullRequestResolvedHook installs the closure a draft → open transition
// runs. Set once at wiring, before any cycle runs.
func (rc *Reconciler) SetPullRequestResolvedHook(h PullRequestResolvedHook) {
	rc.prResolved = h
}

// HasPullRequestResolvedHook reports whether a hook is installed — the wiring's
// own check that a brain's reconciler was not left without one.
func (rc *Reconciler) HasPullRequestResolvedHook() bool {
	return rc.prResolved != nil
}

// ReconcileOrg runs the PR-artifact backstop, then lists the org's
// reconcilable non-terminal artifacts (admin pool, org-wide) and reconciles
// them. The backstop goes first so a pull request it records is brought to
// GitHub's current state in the same cycle, through the same transition — and
// the same resolved hook — as a pull request recorded when it was opened. The
// Tier-1 Runner's per-cycle body.
func (rc *Reconciler) ReconcileOrg(ctx context.Context, orgID string) error {
	// Best-effort — a backstop failure never aborts the reconcile cycle.
	if err := rc.BackfillPRArtifacts(ctx, orgID); err != nil {
		reconcileLog.Warn("PR artifact backstop failed", "org", orgID, "error", err)
	}
	arts, err := rc.artifacts.ListNonTerminalBySystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list non-terminal artifacts: %w", err)
	}
	_, err = rc.Reconcile(ctx, orgID, arts)
	return err
}

// prBackstopWindow is how far back the backstop reads the ledger. A missing
// artifact is normally recorded on the first cycle after its pull request was
// opened. The window is long enough that a row is still read after the
// backstop could not run for days (the control plane down, the org's GitHub
// unreachable), and bounded so that each cycle reads a week of one org's
// pr_created rows rather than every one it has.
const prBackstopWindow = 7 * 24 * time.Hour

// BackfillPRArtifacts is the PR-artifact backstop. TF opens a pull request only
// through the create verb (`tfac exec gh pr create`); the gh-channel gate
// refuses every other way of creating one. The verb writes the pull_request
// artifact and its pr_created ledger row in one write, and when the artifact
// write fails it writes the ledger row alone. That is the only case this pass
// handles: a pull request the verb opened whose ledger row landed and whose
// artifact did not. When the row and the artifact are both lost, this pass has
// nothing to find; recovering that record is the recording path's job, and
// once its row lands this pass handles it like any other.
//
// The ledger row decides both questions the pass answers. Whether TF opened
// the pull request: one with no pr_created row was opened by someone else (a
// person working from a branch a run pushed, for example) and gets no
// artifact, because an artifact for it would hold its task open on an approval
// for something TF never proposed. And who opened it: the row names the
// conversation and team, and they own the artifact, whichever conversation
// pushed the branch.
//
// The pass reads the pr_created rows on the org's current GitHub host from the
// last prBackstopWindow, keeps those whose pull request has no artifact, and
// fetches each of those from GitHub for what the row does not carry (node id,
// refs, title, body). When every pull request already has its artifact, it
// makes no GitHub call. The artifact is recorded in the state the verb opened
// the pull request in, and the reconcile that follows applies any transition
// since. The write is insert-if-absent, so a pull request recorded after the
// presence check is left untouched and repeating the pass changes nothing.
//
// A row whose link no longer names the repository in its target belongs to a
// repository renamed since. A rename rewrites the link and the artifact's key
// but never the row's target, so the target no longer finds the artifact, and
// the row is skipped rather than recorded a second time under the old name.
// Best-effort per pull request: a credential or GitHub failure skips it this
// pass.
func (rc *Reconciler) BackfillPRArtifacts(ctx context.Context, orgID string) error {
	host, err := db.OrgGitHubHostSystem(ctx, rc.orgs, orgID)
	if err != nil {
		return fmt.Errorf("resolve github host: %w", err)
	}
	created, _, err := rc.actions.ListByOrgSystem(ctx, orgID, domain.ExternalActionListOpts{
		Provider: domain.ArtifactProviderGitHub,
		Action:   domain.ActionPRCreated,
		Since:    time.Now().UTC().Add(-prBackstopWindow),
	})
	if err != nil {
		return fmt.Errorf("list pr_created actions: %w", err)
	}

	type openedPR struct {
		owner, repo string
		number      int
		act         domain.ExternalAction
	}
	byKey := map[string]openedPR{}
	for _, act := range created {
		if act.Scope != host || act.ConversationID == "" {
			continue // another host's, or its conversation was purged — nothing to attribute to
		}
		owner, repo, number, ok := domain.ParsePRTarget(act.Target)
		if !ok {
			continue
		}
		// TODO(TFAC-1087): match the row to its artifact by the PR's node id, so
		// a PR whose repository was renamed since is backfilled under the
		// current name rather than skipped, and the result stops depending on
		// the row's link being spelled on the configured host.
		if !domain.GitHubURLNamesRepo(act.URL, host, owner+"/"+repo) {
			continue // renamed since it was opened
		}
		byKey[domain.PullRequestDedupKey(owner+"/"+repo, number)] = openedPR{owner: owner, repo: repo, number: number, act: act}
	}
	if len(byKey) == 0 {
		return nil
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	present, err := rc.artifacts.PresentDedupKeysSystem(ctx, orgID, host, keys)
	if err != nil {
		return fmt.Errorf("read recorded pull requests: %w", err)
	}

	for _, key := range keys {
		if present[key] {
			continue
		}
		pr := byKey[key]
		repoPath := pr.owner + "/" + pr.repo
		client, err := rc.resolver.ClientFor(ctx, orgID, pr.owner)
		if err != nil {
			reconcileLog.Warn("backstop: resolve github client failed; skipping PR this pass",
				"org", orgID, "repo", repoPath, "pr", pr.number, "error", err)
			continue
		}
		live, err := client.GetPRBasic(ctx, pr.owner, pr.repo, pr.number)
		if err != nil {
			reconcileLog.Warn("backstop: fetch PR failed; skipping PR this pass",
				"org", orgID, "repo", repoPath, "pr", pr.number, "error", err)
			continue
		}
		// The proposed snapshot is what GitHub holds now: the body the verb
		// sent, unless someone has edited it since.
		art := domain.NewPullRequestArtifact(repoPath, pr.number, live.NodeID, live.HeadRef, live.BaseRef,
			live.HTMLURL, live.Title, live.Body, pr.act.ToState == domain.ArtifactStatePRDraft)
		art.ConversationID = pr.act.ConversationID
		art.OrgID = orgID
		art.TeamID = pr.act.TeamID
		art.Scope = host
		inserted, err := rc.artifacts.InsertArtifactIfAbsentSystem(ctx, orgID, art)
		if err != nil {
			reconcileLog.Warn("backstop: record PR artifact failed",
				"org", orgID, "repo", repoPath, "pr", pr.number, "error", err)
			continue
		}
		if inserted {
			reconcileLog.Info("backstop: recorded PR artifact from its pr_created row",
				"org", orgID, "repo", repoPath, "pr", pr.number, "conversation", pr.act.ConversationID)
		}
	}
	return nil
}

// Reconcile refreshes each artifact in arts on the org's current GitHub host
// against live GitHub and applies any state transition: PR
// draft/open/merged/closed, review submitted/dismissed, branch deleted. An
// artifact recorded on another host is skipped (see Reconciler). Each
// transition broadcasts over the WS hub (as
// artifact_updated on the owning conversation, so the conversation view's
// artifact-derived surface refreshes), and a draft pull request that stops
// being unresolved runs the resolved hook. Returns the artifacts that
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
	_, arts, err := rc.onCurrentHost(ctx, orgID, arts)
	if err != nil {
		return nil, fmt.Errorf("resolve github host: %w", err)
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

	// Pass 3 — apply each transition, then run the draft-resolved closure once
	// per conversation whose draft pull request stopped being unresolved this
	// cycle.
	//
	// The write-back runs on a detached ctx (the fetches above stayed
	// cancellable). A terminal artifact drops out of BOTH tiers' non-terminal
	// working sets the moment applyTransition commits it, so if a client
	// disconnect (Tier-2) or shutdown (Tier-1) cancelled the state write's
	// follow-up, no later cycle would re-process it — the closure would be
	// skipped, not self-corrected. Detaching keeps the state write and its
	// follow-up atomic w.r.t. the caller's lifecycle, mirroring the approval
	// handlers' post-action bookkeeping; the github http.Client's own 30s
	// timeout still bounds each call.
	writeCtx := context.WithoutCancel(ctx)
	var transitioned []domain.Artifact
	resolvedConversations := map[string]bool{}
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
		if a.ConversationID != "" && isDraftResolved(a, newState) {
			resolvedConversations[a.ConversationID] = true
		}
	}
	// After every write in the cycle has landed, so the closure's unresolved
	// check reads this cycle's transitions rather than racing them.
	if rc.prResolved != nil {
		for conversationID := range resolvedConversations {
			rc.prResolved(writeCtx, orgID, conversationID)
		}
	}
	return transitioned, nil
}

// isDraftResolved reports whether this transition is a draft pull request
// ceasing to await a human by any route GitHub can show: marked ready, merged
// as a draft, or closed unopened. Each ends the approval the draft was waiting
// on, and each is a route the approval click cannot have taken (the click
// flips the row itself, so the reconciler never sees that one as a
// transition). A review resolving is not here: the unresolved predicate reads
// reviews too, but nothing marks one ready out of band.
func isDraftResolved(a domain.Artifact, newState string) bool {
	return a.Kind == domain.ArtifactKindPullRequest && a.State == domain.ArtifactStatePRDraft &&
		newState != domain.ArtifactStatePRDraft
}

// applyTransition writes the new state (admin pool) and broadcasts the change.
// Best-effort WS: a dropped broadcast must not undo the transition.
func (rc *Reconciler) applyTransition(ctx context.Context, orgID string, a domain.Artifact, newState string) (domain.Artifact, error) {
	next := a
	next.State = newState
	if a.Kind == domain.ArtifactKindPullRequest {
		// Nobody resolved this through TF — the row records that so the
		// agent-facing note reads "on GitHub" rather than crediting a human
		// approval or dismissal that never happened.
		next.DetailsJSON = domain.StampPRResolution(a.DetailsJSON, domain.PRResolutionGitHub)
	}
	updated, err := rc.artifacts.UpsertSystem(ctx, orgID, next)
	if err != nil {
		return domain.Artifact{}, err
	}
	rc.broadcast(orgID, updated)
	reconcileLog.Info("artifact reconciled",
		"org", orgID, "artifact", a.ID, "kind", a.Kind, "from", a.State, "to", newState)
	return updated, nil
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
		return prState(snap, a.State), true

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

// prState maps a PR snapshot onto the artifact lifecycle, given the state the
// row holds now. GitHub's PR.state is OPEN/CLOSED/MERGED; merged is also
// flagged explicitly, checked first so a merged PR never reads as merely
// closed.
//
// Draft is one-way. A row that has left `draft` was resolved — a human, or an
// agent on a human's instruction, marked it ready — and a later conversion
// back to draft on GitHub is that human reworking the pull request, not a new
// approval to wait on. Re-deriving `draft` from the flag would show the task
// as owing a human an approval with nothing for the click to do and, on a task
// already closed, surface a banner on a card nobody can act on. The only
// producer of a `draft` row is the create verb, which stamps it at mint.
func prState(snap domain.PRSnapshot, current string) string {
	switch {
	case snap.Merged || strings.EqualFold(snap.State, "MERGED"):
		return domain.ArtifactStatePRMerged
	case strings.EqualFold(snap.State, "CLOSED"):
		return domain.ArtifactStatePRClosed
	case snap.IsDraft && current == domain.ArtifactStatePRDraft:
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
