// Package reconcile keeps the deliverables table in sync with external GitHub
// state — webhook-independent, both modes. It is self-sufficient: it owns its
// fetch (RefreshPRs for PR + review state) rather than leaning on the tracker,
// whose snapshots aren't a reliable source (throttled PR refreshes).
//
// The work splits into two tiers over one shared Reconciler:
//
//   - Tier 1 — a per-org background Manager/Runner (this package), kicked off
//     the system:poll: GitHub sentinel, mirroring scorer/profiler/classifier.
//     Each cycle reconciles the org's whole non-terminal deliverable set.
//   - Tier 2 — a conversation-scoped refresh the frontend polls while a
//     conversation view is open, reconciling just that conversation's
//     non-terminal deliverables (wired in internal/server).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/conversationevent"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// clientResolver is the slice of github.Resolver the reconciler needs — a
// per-(org, owner) GitHub client. github.Resolver satisfies it; tests pass a
// fake returning a stub-backed *github.Client.
type clientResolver interface {
	ClientFor(ctx context.Context, orgID, target string) (*github.Client, error)
}

// Reconciler mirrors deliverables against live GitHub state. One instance is
// shared between Tier 1 (the background Manager/Runner) and Tier 2 (the
// conversation-scoped refresh endpoint): both call Reconcile with a set of
// non-terminal deliverables. Writes route through the admin pool (UpsertSystem) —
// the reconciler has no JWT-claims context in either tier.
//
// It is also where a pull request create's deliverable, recorded pending, is
// settled: the reconciler holds GitHub's copy of every pull request it
// reconciles, which is what its Authorship checks the run's record against
// (settle).
//
// The resolver's clients reach the org's current GitHub host and no other, so
// only deliverables recorded on that host (their scope) are reconciled. Another
// host's pull request is not where those clients look.
type Reconciler struct {
	resolver      clientResolver
	deliverables  db.DeliverableStore
	actions       db.ExternalActionStore    // read for the pr_created rows the backstop works from
	entities      db.EntityStore            // read for the pull request entity a deliverable is about, and stamped
	repos         db.RepositoryStore        // read for the registry row a deliverable's pull request is in
	orgs          db.OrgsStore              // read for the org's current GitHub host
	conversations db.ConversationStore      // read for the creator a run's pull request is stamped with
	claims        db.ConversationQueueStore // read for the engagement live when a pull request was opened
	authorship    Authorship
	ws            *websocket.Hub // nil-safe: broadcasts are skipped when unset (tests)
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

// NewReconciler builds the shared reconciler over stores, deciding who opened
// a pull request by authorship: TrustRecords in local mode, SealedAuthor in
// multi. ws may be nil (broadcasts become no-ops); the stores + resolver are
// required.
func NewReconciler(resolver clientResolver, stores db.Stores, authorship Authorship, ws *websocket.Hub) *Reconciler {
	return &Reconciler{
		resolver:      resolver,
		deliverables:  stores.Deliverables,
		actions:       stores.ExternalActions,
		entities:      stores.Entities,
		repos:         stores.Repos,
		orgs:          stores.Orgs,
		conversations: stores.Conversations,
		claims:        stores.ConversationQueue,
		authorship:    authorship,
		ws:            ws,
	}
}

// onCurrentHost returns the org's current GitHub host and the deliverables of
// arts recorded on it.
func (rc *Reconciler) onCurrentHost(ctx context.Context, orgID string, arts []domain.Deliverable) (string, []domain.Deliverable, error) {
	host, err := db.OrgGitHubHostSystem(ctx, rc.orgs, orgID)
	if err != nil {
		return "", nil, err
	}
	out := make([]domain.Deliverable, 0, len(arts))
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

// ReconcileOrg runs the PR-deliverable backstop, then lists the org's
// reconcilable non-terminal deliverables (admin pool, org-wide) and reconciles
// them. The backstop goes first so a pull request it records is brought to
// GitHub's current state in the same cycle, through the same transition — and
// the same resolved hook — as a pull request recorded when it was opened. The
// Tier-1 Runner's per-cycle body.
func (rc *Reconciler) ReconcileOrg(ctx context.Context, orgID string) error {
	// Best-effort — a backstop failure never aborts the reconcile cycle.
	if err := rc.BackfillPRDeliverables(ctx, orgID); err != nil {
		reconcileLog.WarnContext(ctx, "PR deliverable backstop failed", "org", orgID, "error", err)
	}
	arts, err := rc.deliverables.ListNonTerminalBySystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("list non-terminal deliverables: %w", err)
	}
	_, err = rc.Reconcile(ctx, orgID, arts)
	return err
}

// prBackstopWindow is how far back the backstop reads the ledger. A missing
// deliverable is normally recorded on the first cycle after its pull request was
// opened. The window is long enough that a row is still read after the
// backstop could not run for days (the control plane down, the org's GitHub
// unreachable). It bounds both the rows the read returns and the rows it
// scans: the read walks only the org's pr_created rows inside it
// (ExternalActionStore.ListPRCreatedWithoutDeliverableSystem).
const prBackstopWindow = 7 * 24 * time.Hour

// BackfillPRDeliverables is the PR-deliverable backstop. TF opens a pull request only
// through the create verb (`tfac exec gh pr create`); the gh-channel gate
// refuses every other way of creating one. The verb writes the pull_request
// deliverable and its pr_created ledger row in one write, and when the deliverable
// write fails it writes the ledger row alone. That is the only case this pass
// handles: a pull request the verb opened whose ledger row landed and whose
// deliverable did not. When the row and the deliverable are both lost, this pass has
// nothing to find; recovering that record is the recording path's job, and
// once its row lands this pass handles it like any other.
//
// The ledger row decides both questions the pass answers. Whether TF opened
// the pull request: one with no pr_created row was opened by someone else (a
// person working from a branch a run pushed, for example) and gets no
// deliverable, because a deliverable for it would hold its task open on an approval
// for something TF never proposed. And who opened it: the row names the
// conversation and team, and they own the deliverable, whichever conversation
// pushed the branch.
//
// The pass reads the pr_created rows on the org's current GitHub host from the
// last prBackstopWindow whose pull request has no deliverable, matching the two by
// the pull request's node id. A node id survives a repository rename, so the
// match does not depend on the name the row recorded or on how its link spells
// the host. A row written without a node id is never read, nor is one whose
// conversation was purged, which leaves no one to own the deliverable. It
// fetches those pull requests by node id (RefreshPRs, batched per owner the
// pull request is under now: pullRequestOwner), which gives what the row does
// not carry: the repository's current name, the number, link, refs, title and
// body. The deliverable is keyed
// on the node id (domain.PullRequestDedupKey), the key the create verb writes
// under, so the two writers land on one row whatever name each spelled the
// repository with; its target is the current name. When every pull request
// already has its deliverable, the pass makes no GitHub call.
//
// The deliverable is recorded pending, like the create's own, in the state the
// verb opened the pull request in: the row is relayed, like the deliverable it
// stands in for, so whether its run opened the pull request is settled by the
// reconcile that follows (settle), which also applies any transition since. A
// row settled foreign is still a row, so its ledger row is not read again. The write is insert-if-absent, so a pull request recorded after the read
// is left untouched and repeating the pass changes nothing. Best-effort per
// owner and per pull request: a credential or GitHub failure skips it this
// pass.
func (rc *Reconciler) BackfillPRDeliverables(ctx context.Context, orgID string) error {
	host, err := db.OrgGitHubHostSystem(ctx, rc.orgs, orgID)
	if err != nil {
		return fmt.Errorf("resolve github host: %w", err)
	}
	missing, err := rc.actions.ListPRCreatedWithoutDeliverableSystem(ctx, orgID, host, time.Now().UTC().Add(-prBackstopWindow))
	if err != nil {
		return fmt.Errorf("list pr_created rows without a deliverable: %w", err)
	}

	byNodeID := map[string]domain.ExternalAction{}
	nodeIDsByOwner := map[string]map[string]bool{}
	for _, act := range missing {
		owner, ok := rc.pullRequestOwner(ctx, orgID, act)
		if !ok {
			continue
		}
		// The read is oldest first, so a node id two rows share stays with the
		// conversation that opened the pull request first.
		if _, seen := byNodeID[act.NodeID]; seen {
			continue
		}
		byNodeID[act.NodeID] = act
		addNodeID(nodeIDsByOwner, owner, act.NodeID)
	}

	for _, owner := range sortedOwners(nodeIDsByOwner) {
		client, err := rc.resolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			reconcileLog.WarnContext(ctx, "backstop: resolve github client failed; skipping owner this pass",
				"org", orgID, "owner", owner, "error", err)
			continue
		}
		ids := sortedKeys(nodeIDsByOwner[owner])
		snaps, err := client.RefreshPRs(ctx, ids, false)
		if err != nil {
			reconcileLog.WarnContext(ctx, "backstop: fetch PRs failed; skipping owner this pass",
				"org", orgID, "owner", owner, "count", len(ids), "error", err)
			continue
		}
		for _, nodeID := range ids {
			act := byNodeID[nodeID]
			snap, ok := snaps[nodeID]
			if !ok {
				reconcileLog.WarnContext(ctx, "backstop: PR not found by its node id; skipping PR this pass",
					"org", orgID, "target", act.Target, "node_id", nodeID)
				continue
			}
			rc.recordBackfilledPR(ctx, orgID, host, act, snap)
		}
	}
	return nil
}

// pullRequestOwner is the owner the pr_created row act's pull request is under
// now, which picks the credential it is fetched with: the owner of the
// registry row of the repository the row's entity (entity_id) is in, which a
// rename or transfer of the repository moves, and, for a row about no entity
// or whose entity or repository cannot be read, the owner its target recorded
// at the time of the act.
func (rc *Reconciler) pullRequestOwner(ctx context.Context, orgID string, act domain.ExternalAction) (string, bool) {
	if act.EntityID != "" && rc.entities != nil && rc.repos != nil {
		row, err := rc.entityRepository(ctx, orgID, act.EntityID)
		switch {
		case err != nil:
			reconcileLog.WarnContext(ctx, "backstop: read the PR's repository failed; fetching under the owner the row recorded",
				"org", orgID, "target", act.Target, "entity", act.EntityID, "error", err)
		case row != nil:
			return row.Owner, true
		}
	}
	return prOwner(act.Target)
}

// entityRepository is the registry row of the repository entity entityID is
// in, or nil when the entity is gone or references no repository.
func (rc *Reconciler) entityRepository(ctx context.Context, orgID, entityID string) (*domain.Repository, error) {
	e, err := rc.entities.GetSystem(ctx, orgID, entityID)
	if err != nil || e == nil || e.RepositoryID == "" {
		return nil, err
	}
	return rc.repos.GetSystem(ctx, orgID, e.RepositoryID)
}

// recordBackfilledPR records the pull_request deliverable for the pr_created row
// act from snap, GitHub's current copy of the pull request, keyed on the node
// id act carries and linked to the entity carrying it: the create verb's own
// recording resolved, or created, that entity before its deliverable write
// failed. The target is the entity's key, as every writer records it, and
// GitHub's current name when no entity carries the node id. The proposed
// snapshot is what GitHub holds now: the title and body the verb sent, unless
// someone has edited them since.
func (rc *Reconciler) recordBackfilledPR(ctx context.Context, orgID, host string, act domain.ExternalAction, snap domain.PRSnapshot) {
	target := domain.PullRequestTarget(snap.Repo, snap.Number)
	if _, _, _, ok := domain.ParsePRTarget(target); !ok {
		reconcileLog.WarnContext(ctx, "backstop: PR snapshot names no repository and number; skipping PR this pass",
			"org", orgID, "target", act.Target, "node_id", act.NodeID)
		return
	}
	art := domain.NewPullRequestDeliverable(snap.Repo, snap.Number, act.NodeID, snap.HeadRef, snap.BaseRef,
		snap.URL, snap.Title, snap.Body, act.ToState == domain.DeliverableStatePRDraft)
	art.ConversationID = act.ConversationID
	art.OrgID = orgID
	art.TeamID = act.TeamID
	art.Scope = host
	art.Verification = domain.DeliverableVerificationPending
	if rc.entities != nil {
		entity, err := rc.entities.GetByExternalIDSystem(ctx, orgID, domain.ProviderGitHub, host, act.NodeID)
		if err != nil {
			reconcileLog.WarnContext(ctx, "backstop: read the PR's entity failed; recording the deliverable unlinked",
				"org", orgID, "target", target, "error", err)
		} else if entity != nil {
			art.EntityID, art.Target = entity.ID, entity.SourceID
		}
	}
	inserted, err := rc.deliverables.InsertDeliverableIfAbsentSystem(ctx, orgID, art)
	if err != nil {
		reconcileLog.WarnContext(ctx, "backstop: record PR deliverable failed",
			"org", orgID, "target", target, "error", err)
		return
	}
	if inserted {
		reconcileLog.InfoContext(ctx, "backstop: recorded PR deliverable from its pr_created row",
			"org", orgID, "target", target, "recorded_as", act.Target, "conversation", act.ConversationID)
	}
}

// Reconcile refreshes each deliverable in arts on the org's current GitHub host
// against live GitHub and applies any state transition: PR
// draft/open/merged/closed, review submitted/dismissed. A deliverable recorded
// on another host is skipped (see Reconciler). Each
// transition broadcasts over the WS hub (as
// deliverable_updated on the owning conversation, so the conversation view's
// deliverable-derived surface refreshes), and a draft pull request that stops
// being unresolved runs the resolved hook. A pending pull request is settled
// first (settle); one settled foreign is closed by it and transitions no
// further. Returns the deliverables that transitioned, carrying their new
// state.
//
// Best-effort per deliverable: a single GitHub or write failure is logged and
// skipped, never aborting the rest. GitHub work is grouped by repo OWNER —
// RefreshPRs keys off a per-owner credential — and batched, so a cycle is a
// handful of GraphQL calls regardless of deliverable count.
func (rc *Reconciler) Reconcile(ctx context.Context, orgID string, arts []domain.Deliverable) ([]domain.Deliverable, error) {
	if len(arts) == 0 {
		return nil, nil
	}
	_, arts, err := rc.onCurrentHost(ctx, orgID, arts)
	if err != nil {
		return nil, fmt.Errorf("resolve github host: %w", err)
	}

	// Pass 1 — group the GitHub fetches by owner. PR and review deliverables both
	// refresh from a PR node id (stored in details). The owner is the registry
	// row's current one, read by id through the pull request's entity. A
	// retired row's name is another repository's now, so its deliverables are
	// left as they stand. A deliverable missing its handle (no node id, no
	// repository) can't be refreshed and is left for a later cycle once a
	// writer fills it in.
	reads := newRegistryReads(rc)
	prNodeIDsByOwner := map[string]map[string]bool{}
	for _, a := range arts {
		nodeID := prNodeID(a)
		if nodeID == "" {
			continue
		}
		pr, err := db.ResolveDeliverablePullRequest(ctx, orgID, a, reads.entity, reads.repository)
		if err != nil {
			if !errors.Is(err, db.ErrDeliverableNamesNoPullRequest) {
				reconcileLog.WarnContext(ctx, "resolve deliverable's pull request failed; skipping it this cycle",
					"org", orgID, "deliverable", a.ID, "error", err)
			}
			continue
		}
		if pr.Retired {
			continue
		}
		addNodeID(prNodeIDsByOwner, pr.Owner, nodeID)
	}

	// Pass 2 — one fetch per owner, tolerant of a per-owner credential/network
	// failure (skip that owner's deliverables this cycle). PR node ids are
	// globally unique, so snapshots merge safely across owners.
	snapshots := map[string]domain.PRSnapshot{}
	for _, owner := range sortedOwners(prNodeIDsByOwner) {
		client, err := rc.resolver.ClientFor(ctx, orgID, owner)
		if err != nil {
			reconcileLog.WarnContext(ctx, "resolve github client failed; skipping owner this cycle",
				"org", orgID, "owner", owner, "error", err)
			continue
		}
		if ids := sortedKeys(prNodeIDsByOwner[owner]); len(ids) > 0 {
			// includeCheckRuns=false: reconciliation needs PR lifecycle + reviews,
			// not CI — the lighter discovery fragment still carries both.
			snaps, err := client.RefreshPRs(ctx, ids, false)
			if err != nil {
				reconcileLog.WarnContext(ctx, "refresh PRs failed", "org", orgID, "owner", owner, "count", len(ids), "error", err)
			} else {
				for k, v := range snaps {
					snapshots[k] = v
				}
			}
		}
	}

	// Pass 3 — settle each pending pull request, apply each transition, then
	// run the draft-resolved closure once per conversation whose draft pull
	// request stopped being unresolved this cycle, including by being settled
	// foreign.
	//
	// The write-back runs on a detached ctx (the fetches above stayed
	// cancellable). A terminal deliverable drops out of BOTH tiers' non-terminal
	// working sets the moment applyTransition commits it, so if a client
	// disconnect (Tier-2) or shutdown (Tier-1) cancelled the state write's
	// follow-up, no later cycle would re-process it — the closure would be
	// skipped, not self-corrected. Detaching keeps the state write and its
	// follow-up atomic w.r.t. the caller's lifecycle, mirroring the approval
	// handlers' post-action bookkeeping; the github http.Client's own 30s
	// timeout still bounds each call.
	writeCtx := context.WithoutCancel(ctx)
	var transitioned []domain.Deliverable
	resolvedConversations := map[string]bool{}
	attribution := newAttributionReads(rc)
	for _, a := range arts {
		if snap, ok := snapshots[prNodeID(a)]; ok && a.Verification == domain.DeliverableVerificationPending {
			prev := a
			var v verdict
			var moved bool
			if a, v, moved = rc.settle(writeCtx, orgID, a, &snap, attribution); moved && v == verdictForeign {
				rc.broadcast(orgID, a)
				transitioned = append(transitioned, a)
				if a.ConversationID != "" && isDraftResolved(prev, a.State) {
					resolvedConversations[a.ConversationID] = true
				}
				continue
			}
		}
		newState, ok := nextState(a, snapshots)
		if !ok || newState == a.State {
			continue
		}
		updated, err := rc.applyTransition(writeCtx, orgID, a, newState)
		if err != nil {
			reconcileLog.WarnContext(ctx, "apply deliverable transition failed",
				"org", orgID, "deliverable", a.ID, "kind", a.Kind, "from", a.State, "to", newState, "error", err)
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
func isDraftResolved(a domain.Deliverable, newState string) bool {
	return a.Kind == domain.DeliverableKindPullRequest && a.State == domain.DeliverableStatePRDraft &&
		newState != domain.DeliverableStatePRDraft
}

// applyTransition writes the new state (admin pool) and broadcasts the change.
// Best-effort WS: a dropped broadcast must not undo the transition.
func (rc *Reconciler) applyTransition(ctx context.Context, orgID string, a domain.Deliverable, newState string) (domain.Deliverable, error) {
	next := a
	next.State = newState
	if a.Kind == domain.DeliverableKindPullRequest {
		// Nobody resolved this through TF — the row records that so the
		// agent-facing note reads "on GitHub" rather than crediting a human
		// approval or dismissal that never happened.
		next.DetailsJSON = domain.StampPRResolution(a.DetailsJSON, domain.PRResolutionGitHub)
	}
	updated, err := rc.deliverables.UpsertSystem(ctx, orgID, next)
	if err != nil {
		return domain.Deliverable{}, err
	}
	rc.broadcast(orgID, updated)
	reconcileLog.InfoContext(ctx, "deliverable reconciled",
		"org", orgID, "deliverable", a.ID, "kind", a.Kind, "from", a.State, "to", newState)
	return updated, nil
}

// broadcast pushes the transition to the frontend as a dedicated deliverable_updated
// event on the owning conversation. It deliberately does NOT reuse conversation_update:
// that event's consumers (the Board) optimistically write the conversation's
// Status from data.status, so a payload carrying no real status would blank the
// card until a refetch. The conversation's own status is unchanged here — only its
// deliverable-derived surface (pending kind / approval card) is — so the FE handlers
// refetch the conversation on this event without touching status. Skipped for a
// detached deliverable (no conversation) or unset hub.
func (rc *Reconciler) broadcast(orgID string, a domain.Deliverable) {
	if rc.ws == nil || a.ConversationID == "" {
		return
	}
	rc.ws.Broadcast(websocket.Event{
		Type:           "deliverable_updated",
		OrgID:          orgID,
		ConversationID: a.ConversationID,
		Data:           map[string]any{"deliverable_id": a.ID, "state": a.State},
	})
	// And the resource-wide ping beside it, for the counters that follow a SET
	// rather than one conversation. The event above is addressed to whoever is
	// watching this conversation and carries what changed on it; the shell rail
	// is watching neither, and a transition here can be the whole difference
	// between "waiting on a person" and "done" — a draft PR merged on GitHub
	// resolves without any surface of ours writing a thing.
	conversationevent.Publish(rc.ws, orgID)
}

// nextState computes the deliverable's reconciled state from the fetched GitHub
// data, returning ok=false when there's no confident answer (snapshot missing,
// review not yet surfaced) so the caller leaves the row untouched. A returned
// state equal to the current one is a no-op.
func nextState(a domain.Deliverable, snapshots map[string]domain.PRSnapshot) (string, bool) {
	switch a.Kind {
	case domain.DeliverableKindPullRequest:
		snap, ok := snapshots[prNodeID(a)]
		if !ok {
			return "", false
		}
		return prState(snap, a.State), true

	case domain.DeliverableKindReview:
		d, _ := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
		snap, ok := snapshots[d.NodeID]
		if !ok {
			return "", false
		}
		return reviewState(snap, a.ExternalID)
	}
	return "", false
}

// prState maps a PR snapshot onto the deliverable lifecycle, given the state the
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
		return domain.DeliverableStatePRMerged
	case strings.EqualFold(snap.State, "CLOSED"):
		return domain.DeliverableStatePRClosed
	case snap.IsDraft && current == domain.DeliverableStatePRDraft:
		return domain.DeliverableStatePRDraft
	default:
		return domain.DeliverableStatePROpen
	}
}

// reviewState maps the bot's review (matched by its node id in the PR's latest
// reviews) onto the deliverable lifecycle. A pending review is private until
// submitted, so the bot's own pending review either isn't surfaced here or
// reads as PENDING — both leave the deliverable pending (ok=false / no-op). Only a
// positive match with a terminal review state transitions the row, so a missing
// review never flips a still-pending deliverable to dismissed.
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
			return domain.DeliverableStateReviewDismissed, true
		case "PENDING", "":
			return "", false // still pending/private — leave as-is
		default: // APPROVED, CHANGES_REQUESTED, COMMENTED
			return domain.DeliverableStateReviewSubmitted, true
		}
	}
	return "", false
}

// --- coordinate helpers ---

// prOwner extracts the repo owner from a PR/review deliverable target
// (owner/repo#number). ok=false on a malformed target.
func prOwner(target string) (string, bool) {
	owner, _, _, ok := domain.ParsePRTarget(target)
	return owner, ok
}

// prNodeID is the node id of the pull request a pull request or review
// deliverable is about; "" when the row carries none. A pull request's is the
// one its key names (domain.DeliverablePullRequestNodeID), the pull request
// its entity is linked to, and its details only where the key names none:
// details are the recording run's to fill, so a pull request read through
// them could be another than the one the row is about.
func prNodeID(a domain.Deliverable) string {
	if a.Kind == domain.DeliverableKindReview {
		d, _ := domain.ParseReviewDeliverableDetails(a.DetailsJSON)
		return d.NodeID
	}
	if nodeID, ok := domain.DeliverablePullRequestNodeID(a); ok {
		return nodeID
	}
	d, _ := domain.ParsePRDeliverableDetails(a.DetailsJSON)
	return d.NodeID
}

// registryReads are the entity and registry-row point reads one reconcile
// pass makes, each row read once however many deliverables reference it.
type registryReads struct {
	rc       *Reconciler
	entities map[string]*domain.Entity
	repos    map[string]*domain.Repository
}

func newRegistryReads(rc *Reconciler) *registryReads {
	return &registryReads{rc: rc, entities: map[string]*domain.Entity{}, repos: map[string]*domain.Repository{}}
}

func (r *registryReads) entity(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if e, ok := r.entities[id]; ok {
		return e, nil
	}
	e, err := r.rc.entities.GetSystem(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	r.entities[id] = e
	return e, nil
}

func (r *registryReads) repository(ctx context.Context, orgID, id string) (*domain.Repository, error) {
	if row, ok := r.repos[id]; ok {
		return row, nil
	}
	row, err := r.rc.repos.GetSystem(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	r.repos[id] = row
	return row, nil
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

// sortedOwners returns the owners the PR work map holds, sorted for
// deterministic per-owner iteration.
func sortedOwners(prByOwner map[string]map[string]bool) []string {
	out := make([]string, 0, len(prByOwner))
	for o := range prByOwner {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}
