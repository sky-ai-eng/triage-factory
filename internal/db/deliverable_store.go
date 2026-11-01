package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrExternalObjectScopeRequired refuses a deliverable or audit-ledger write
// with no scope. A dedup key and a target are only unique within a provider
// namespace, so a row without one would not say which object it records.
var ErrExternalObjectScopeRequired = errors.New("external object scope is required")

// RequireExternalObjectScope refuses an empty scope with
// ErrExternalObjectScopeRequired. Both dialects' deliverable and external-action
// writes call it before touching the database.
func RequireExternalObjectScope(scope string) error {
	if scope == "" {
		return ErrExternalObjectScopeRequired
	}
	return nil
}

// ErrDeliverableKind refuses a deliverable write of a kind other than a pull
// request or a review. A deliverable is work an agent prepared for a person to
// act on, and those are the two kinds a person acts on; every other object a
// run changes is an artifact, read from the external-action ledger.
var ErrDeliverableKind = errors.New("a deliverable is a pull request or a review")

// RequireDeliverableKind refuses a kind other than a pull request or a review
// with ErrDeliverableKind. Both dialects' deliverable writes call it before touching
// the database; the Postgres table's CHECK says the same.
func RequireDeliverableKind(kind string) error {
	switch kind {
	case domain.DeliverableKindPullRequest, domain.DeliverableKindReview:
		return nil
	}
	return fmt.Errorf("%w, not %q", ErrDeliverableKind, kind)
}

// DeliverableStore owns the deliverables table: one row per pull request or
// review an agent prepared for a person, owned by the conversation that made
// it. Approval, attention, task close and teardown read it, and so do the
// conversation detail view and the artifacts read, which highlights the
// artifact a deliverable is.
//
// Mirrors the conversations store for pool/RLS/scan conventions: app pool in
// Postgres (RLS-active under tf_app, team-scoped via team_id exactly like
// conversations), the one connection in SQLite. orgID is required on every method
// and stays in the WHERE/INSERT clause as defense in depth alongside RLS;
// SQLite asserts it equals runmode.LocalDefaultOrgID. See TFAC-455.
type DeliverableStore interface {
	// Upsert inserts the deliverable or, on a (org_id, scope, dedup_key)
	// conflict, updates the existing row's mutable fields (state, target,
	// details_json, updated_at). The same PR seen via exec and again via
	// reconciliation lands on one row. Returns the stored row with id and
	// timestamps populated. a.ID may be empty (both impls generate a uuid); a
	// non-empty a.ID is honored on insert and ignored on update. An empty
	// a.Scope is refused with ErrExternalObjectScopeRequired: the same dedup
	// key in two scopes is two objects. A kind other than a pull request or a
	// review is refused with ErrDeliverableKind.
	//
	// external_id, url and node_id are preserve-on-empty: an upsert that
	// leaves them empty keeps whatever the row already holds rather than
	// blanking it. They are the backing object's stable coordinates (PR
	// number or review id, html link, GraphQL node id) — once known they only
	// fill in, never legitimately clear — so a later writer that can't supply
	// them (reconciliation) won't erase an earlier writer's value. To
	// intentionally change them, pass the new non-empty value. details_json is
	// preserve-on-empty too.
	//
	// Runs on the app pool in Postgres (RLS-active), so the caller must be
	// inside a claims-set tx (request handler or SyntheticClaimsWithTx). A
	// caller with an authoritative team identity but no JWT-claims context —
	// an event-triggered conversation's exec choke point, which has no user
	// (event-triggered conversations by schema CHECK carry no creator) — must
	// use UpsertSystem instead.
	Upsert(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error)

	// UpsertSystem is the admin-pool (BYPASSRLS) variant of Upsert for
	// system-service writers that hold a real (org_id, team_id) identity but
	// no JWT-claims context — chiefly the exec choke point on an
	// event-triggered conversation (TFAC-459), whose insert is unreachable through
	// tf_app because the deliverables_insert RLS policy demands a team-writing
	// user and the conversation has none. Mirrors the `...System` admin halves on
	// Conversations / ConversationWorktrees / Reviews. org_id stays bound as defense in
	// depth; team_id on the row is authoritative (it comes from the conversation).
	// Identical to Upsert in SQLite (single-tenant, no RLS).
	UpsertSystem(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error)

	// InsertDeliverableIfAbsentSystem inserts a only when no deliverable with its
	// (org_id, scope, dedup_key) already exists — ON CONFLICT DO NOTHING — and reports
	// whether a row was actually inserted (false when one was already present,
	// changing nothing). Unlike UpsertSystem it NEVER overwrites an existing
	// row's state/details, so a discovery pass can record an object it found
	// without clobbering a more advanced state an earlier writer set (a merged
	// PR must not regress to open). This is the write the reconciler's PR
	// backstop uses: it records the pull_request deliverable for a PR the ledger
	// says the create verb opened, only if the verb (or a prior pass) didn't
	// already. Admin pool — the backstop has no JWT-claims context.
	// Identical single-connection path in SQLite. A kind other than a pull
	// request or a review is refused with ErrDeliverableKind, as Upsert does.
	InsertDeliverableIfAbsentSystem(ctx context.Context, orgID string, a domain.Deliverable) (bool, error)

	// TransitionReviewState atomically compare-and-swaps a REVIEW deliverable's
	// state: the row moves from → to only when it still holds `from` (and is a
	// review), and the caller learns whether it won (true) or lost the race /
	// addressed a missing row (false). This is the approve/dismiss claim
	// primitive: Upsert's state column is last-writer-wins, so a
	// read-check-then-upsert flip lets two concurrent approves both pass the
	// check and double-submit one review to GitHub — the CAS makes exactly one
	// caller the winner, and it runs BEFORE the external write so a review can
	// be posted at most once.
	//
	// externalID / url / detailsJSON optionally stamp the resolved review's
	// coordinates in the same atomic write, preserve-on-empty like Upsert (an
	// empty value leaves the column untouched) — the approve success path flips
	// submitted→submitted carrying what GitHub minted, so the deliverable row (and
	// every list read over it) shows the posted review's URL, never the stale
	// draft.
	//
	// Runs on the app pool in Postgres (RLS-active): every caller is a request
	// handler under claims. org_id stays bound as defense in depth.
	TransitionReviewState(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error)

	// TransitionReviewStateSystem is the admin-pool (BYPASSRLS) variant of
	// TransitionReviewState, for the exec choke point's event-triggered review
	// writers — the same split UpdateReviewDetailsIfPendingSystem covers. The
	// auto-posting review posture needs the identical claim the approve handler
	// takes: the drafting conversation and a human approver can both reach a finalized
	// draft, so whichever calls SubmitReview must win this CAS first or the
	// review posts twice. Identical to TransitionReviewState in SQLite.
	TransitionReviewStateSystem(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error)

	// UpdateReviewDetailsIfPending rewrites a review deliverable's details_json
	// only while the draft is still state=pending, returning false when the row
	// is missing, not a review, or already resolved. Draft mutations (staged
	// body/event/comments, refresh) must use this instead of Upsert: an
	// unconditional upsert writes the writer's stale in-memory state back over
	// a concurrently submitted/dismissed review, resurrecting it as approvable
	// — which re-opens the double-submit hole TransitionReviewState closes.
	//
	// Runs on the app pool in Postgres (RLS-active), under request or synthetic
	// claims. org_id stays bound as defense in depth.
	UpdateReviewDetailsIfPending(ctx context.Context, orgID, id, detailsJSON string) (bool, error)

	// UpdateReviewDetailsIfPendingSystem is the admin-pool (BYPASSRLS) variant
	// of UpdateReviewDetailsIfPending for the exec choke point's event-triggered
	// review-draft writers, which hold a real (org_id, team_id) identity but no
	// JWT-claims context — the same split UpsertSystem covers. Identical to
	// UpdateReviewDetailsIfPending in SQLite (single-tenant, no RLS).
	UpdateReviewDetailsIfPendingSystem(ctx context.Context, orgID, id, detailsJSON string) (bool, error)

	// Get returns a single deliverable by id, or nil when none matches. Runs on
	// the app pool in Postgres (RLS-active), so the caller must be inside a
	// claims-set tx — every consumer is an HTTP handler under request claims
	// (the deliverable-id-addressed PR endpoints: GET / PATCH / diff / approve).
	// org_id stays bound as defense in depth alongside RLS.
	Get(ctx context.Context, orgID, id string) (*domain.Deliverable, error)

	// ListByConversation returns one page of the deliverables a conversation
	// produced, newest first (created_at DESC, id DESC), and how many it
	// produced in all. opts windows the page; db.Unwindowed reads every row,
	// for a caller deciding something about the whole set. Backs the
	// conversation detail surface (A·6).
	ListByConversation(ctx context.Context, orgID, conversationID string, opts ListOpts) ([]domain.Deliverable, int, error)

	// ListPendingReviewsByEntitySystem returns every PENDING review deliverable
	// (Kind=review, State=pending) about the pull request entity entityID
	// (deliverables.entity_id), across every team in the org, newest first, so a
	// draft is found whatever name its target spells. An empty entityID
	// matches nothing. It backs the PR coherence feed's freshness gate: on a
	// head advance, the in-flight review drafts anchored to that PR say which
	// conversations already recorded the new head and so are not behind it.
	// "Pending" — not yet submitted or dismissed — is the only state with a
	// live draft worth reconciling; a submitted/dismissed review is done.
	//
	// Admin-pool / org-wide in Postgres — the same BYPASSRLS, org-scoped shape
	// ListNonTerminalBySystem uses: the notifier runs in an eventbus subscriber
	// goroutine with no JWT-claims context and must see every team's drafts for
	// the PR (the event is org-wide, not team-scoped). org_id stays bound in the
	// WHERE clause as defense in depth. Identical to a plain org read in SQLite
	// (single-tenant, no RLS).
	ListPendingReviewsByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Deliverable, error)

	// PresentExternalIDsSystem returns the subset of externalIDs that a
	// deliverable of the given provider and kind in scope carries as its
	// external_id, as a set. The tracker reads it to tell a review TF
	// submitted, whose review deliverable carries the review's numeric id once
	// it is posted, from one a person submitted. idx_deliverables_external_id
	// serves it. An empty scope is refused with
	// ErrExternalObjectScopeRequired.
	//
	// Admin pool (BYPASSRLS) in Postgres: the tracker holds no claims, and
	// the deliverable belongs to whichever team's run submitted the review.
	// org_id stays bound as defense in depth. Identical to a plain org read in
	// SQLite.
	PresentExternalIDsSystem(ctx context.Context, orgID, scope, provider, kind string, externalIDs []string) (map[string]bool, error)

	// ListByConversationSystem is the admin-pool (BYPASSRLS) variant of ListByConversation for
	// system-service readers that hold a real (org_id) identity but no
	// JWT-claims context — chiefly the delegate spawner's post-completion
	// deliverable check, which reads a conversation's deliverables from a goroutine detached
	// from any request. Identical to ListByConversation
	// in SQLite (single-tenant, no RLS).
	ListByConversationSystem(ctx context.Context, orgID, conversationID string) ([]domain.Deliverable, error)

	// CountByConversation returns the number of deliverables each given conversation produced,
	// keyed by conversation id. Conversations with zero deliverables (or absent
	// from the table) have no entry — the caller treats a missing key as 0. It
	// batches the conversation-list path's per-card count into one query so
	// listing a task's conversations stays O(1) in deliverable reads rather than
	// N+1 (the conversation response's approval-signal guard,
	// internal/server/agent.go). An empty conversationIDs returns an empty map
	// without touching the DB.
	//
	// Mirrors ListByConversation's pool/RLS conventions: app pool in Postgres
	// (RLS-active, so the count is team-scoped exactly like the rows it
	// counts — a non-member counts zero), the one connection in SQLite.
	// orgID stays bound as defense in depth. Detached deliverables (conversation
	// purged → conversation_id NULL) never match and are correctly excluded.
	CountByConversation(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error)

	// ListByConversations returns the deliverables for every given conversation as
	// one flat slice, each ordered newest-first (created_at DESC, id DESC) so a
	// conversation's rows match ListByConversation's order once grouped. It
	// lets a caller projecting many conversations fetch their deliverables in a
	// single query instead of an N+1 of per-conversation ListByConversation
	// calls — the conversation-list response uses it to resolve pending_kind
	// for the parked conversations in a task's conversation list
	// (internal/server/agent.go). Each deliverable carries its ConversationID, so
	// the caller groups by it; an empty conversationIDs returns nil without
	// touching the DB.
	//
	// Mirrors ListByConversation's pool/RLS conventions: app pool in Postgres
	// (RLS-active, team-scoped), the one connection in SQLite. orgID stays
	// bound as defense in depth. Detached deliverables (conversation_id NULL) never match.
	ListByConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Deliverable, error)

	// ListArtifactMatchesByTeamSystem returns teamID's deliverables that can
	// be the same object as an artifact: its pull_request deliverables about
	// one of entityIDs and its review deliverables whose external_id is one of
	// reviewIDs (domain.ArtifactDeliverableKeys derives both sets from the
	// artifacts), newest first (created_at DESC, id DESC), so the match
	// domain.ArtifactDeliverable finds first is the most recently created.
	// Both sets empty returns nil without touching the database.
	//
	// Admin pool (BYPASSRLS) in Postgres. Its reader is the Usage → Objects
	// team lens, behind that route's team-admin-or-org-admin gate: an org admin
	// on no team reads the team's ledger there, and under the deliverables
	// RLS policy would see its artifacts with none of their deliverables, which
	// the org lens shows them. org_id and team_id stay bound. Identical to a
	// plain org read in SQLite.
	//
	// TODO(TFAC-1285): once deliverables_select admits org admins, this read
	// moves to the app pool as ListArtifactMatchesByTeam and the route's gate
	// stops being its only enforcement.
	ListArtifactMatchesByTeamSystem(ctx context.Context, orgID, teamID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error)

	// ListArtifactMatchesByOrgSystem is ListArtifactMatchesByTeamSystem over
	// every team of the org, for the org lens. Admin pool.
	ListArtifactMatchesByOrgSystem(ctx context.Context, orgID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error)

	// ListNonTerminalBySystem returns every deliverable for the org whose
	// (kind, state) is still reconcilable — PR draft|open
	// (domain.IsReconcilableNonTerminal). It is the deliverable reconciler's
	// per-cycle working set, org-wide and bounded: terminal deliverables
	// (merged/closed/submitted/dismissed) drop out, so the set shrinks as work
	// resolves. A pending review is not in it: it is staged TF-side and has no
	// GitHub object to reconcile until a person's approval posts it, at which
	// point it is already terminal.
	//
	// Admin-pool / org-wide in Postgres — the same BYPASSRLS, org-scoped
	// shape the scorer's UnscoredTasks and EntityStore's
	// ListActiveTerminalCandidatesSystem use: the reconciler is a background
	// system job with no JWT-claims context, and it must see every team's
	// non-terminal deliverables to keep them fresh. org_id stays bound as
	// defense in depth.
	// Identical to a plain org read in SQLite (single-tenant, no RLS).
	ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Deliverable, error)
}
