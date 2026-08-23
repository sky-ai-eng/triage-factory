package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ArtifactStore owns the artifacts table — the single durable,
// conversation-attributed, polymorphic record of everything a conversation
// produces in an external system (branch, PR, review, Jira/Linear issue,
// comment). Every capture writer in TFAC-454's Sub-epic A UPSERTs through it;
// the conversation detail view, team-level C2, and the audit ledger read from
// it.
//
// Mirrors the conversations store for pool/RLS/scan conventions: app pool in
// Postgres (RLS-active under tf_app, team-scoped via team_id exactly like
// conversations), the one connection in SQLite. orgID is required on every method
// and stays in the WHERE/INSERT clause as defense in depth alongside RLS;
// SQLite asserts it equals runmode.LocalDefaultOrgID. See TFAC-455.
type ArtifactStore interface {
	// Upsert inserts the artifact or, on a (org_id, dedup_key) conflict,
	// updates the existing row's mutable fields (state, target,
	// details_json, conversation_id, team_id, updated_at). This is the one writer all
	// of Sub-epic A shares: the same PR seen via exec and again via
	// reconciliation lands on one row. Returns the stored row with id and
	// timestamps populated. a.ID may be empty (both impls generate a uuid); a
	// non-empty a.ID is honored on insert and ignored on update.
	//
	// external_id and url are preserve-on-empty: an upsert that leaves them
	// empty keeps whatever the row already holds rather than blanking it.
	// They are the backing object's stable coordinates (PR number / issue
	// key, html link) — once known they only fill in, never legitimately
	// clear — so a later writer that can't supply them (reconciliation, or a
	// mutation that can't compute the URL) won't erase an earlier writer's
	// value. To intentionally change them, pass the new non-empty value.
	//
	// Runs on the app pool in Postgres (RLS-active), so the caller must be
	// inside a claims-set tx (request handler or SyntheticClaimsWithTx). A
	// caller with an authoritative team identity but no JWT-claims context —
	// an event-triggered conversation's exec choke point, which has no user
	// (event-triggered conversations by schema CHECK carry no creator) — must
	// use UpsertSystem instead.
	Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error)

	// UpsertSystem is the admin-pool (BYPASSRLS) variant of Upsert for
	// system-service writers that hold a real (org_id, team_id) identity but
	// no JWT-claims context — chiefly the exec choke point on an
	// event-triggered conversation (TFAC-459), whose insert is unreachable through
	// tf_app because the artifacts_insert RLS policy demands a team-writing
	// user and the conversation has none. Mirrors the `...System` admin halves on
	// Conversations / ConversationWorktrees / Reviews. org_id stays bound as defense in
	// depth; team_id on the row is authoritative (it comes from the conversation).
	// Identical to Upsert in SQLite (single-tenant, no RLS).
	UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error)

	// InsertArtifactIfAbsentSystem inserts a only when no artifact with its
	// (org_id, dedup_key) already exists — ON CONFLICT DO NOTHING — and reports
	// whether a row was actually inserted (false when one was already present,
	// changing nothing). Unlike UpsertSystem it NEVER overwrites an existing
	// row's state/details, so a discovery pass can record an object it found
	// without clobbering a more advanced state an earlier writer set (a merged
	// PR must not regress to open). This is the write the gh-channel reconciler
	// backstop uses: it matches an open PR to a conversation's pushed branch and
	// records the pull_request artifact only if the observation path (or a prior
	// pass) didn't already. Admin pool — the backstop has no JWT-claims context.
	// Identical single-connection path in SQLite.
	InsertArtifactIfAbsentSystem(ctx context.Context, orgID string, a domain.Artifact) (bool, error)

	// TransitionReviewState atomically compare-and-swaps a REVIEW artifact's
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
	// submitted→submitted carrying what GitHub minted, so the artifact row (and
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

	// UpdateReviewDetailsIfPending rewrites a review artifact's details_json
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

	// Get returns a single artifact by id, or nil when none matches. Runs on
	// the app pool in Postgres (RLS-active), so the caller must be inside a
	// claims-set tx — every consumer is an HTTP handler under request claims
	// (the artifact-id-addressed PR endpoints: GET / PATCH / diff / approve).
	// org_id stays bound as defense in depth alongside RLS.
	Get(ctx context.Context, orgID, id string) (*domain.Artifact, error)

	// ListByConversation returns every artifact produced by one conversation, newest first.
	// Backs the conversation detail surface (A·6).
	ListByConversation(ctx context.Context, orgID, conversationID string) ([]domain.Artifact, error)

	// ListPendingReviewsByTargetSystem returns every PENDING review artifact
	// (Kind=review, State=pending) whose Target is the given PR resource key
	// (owner/repo#number), across every team in the org, newest first. It backs
	// the PR coherence feed's freshness gate: on a head advance, the in-flight
	// review drafts anchored to that PR say which conversations already recorded
	// the new head and so are not behind it. "Pending" — not yet submitted or
	// dismissed — is the
	// only state with a live draft worth reconciling; a submitted/dismissed
	// review is done.
	//
	// Admin-pool / org-wide in Postgres — the same BYPASSRLS, org-scoped shape
	// ListNonTerminalBySystem uses: the notifier runs in an eventbus subscriber
	// goroutine with no JWT-claims context and must see every team's drafts for
	// the PR (the event is org-wide, not team-scoped). org_id stays bound in the
	// WHERE clause as defense in depth. Identical to a plain org read in SQLite
	// (single-tenant, no RLS).
	ListPendingReviewsByTargetSystem(ctx context.Context, orgID, target string) ([]domain.Artifact, error)

	// ListByConversationSystem is the admin-pool (BYPASSRLS) variant of ListByConversation for
	// system-service readers that hold a real (org_id) identity but no
	// JWT-claims context — chiefly the delegate spawner's post-completion
	// artifact check, which reads a conversation's artifacts from a goroutine detached
	// from any request. Identical to ListByConversation
	// in SQLite (single-tenant, no RLS).
	ListByConversationSystem(ctx context.Context, orgID, conversationID string) ([]domain.Artifact, error)

	// CountByConversation returns the number of artifacts each given conversation produced,
	// keyed by conversation id. Conversations with zero artifacts (or absent
	// from the table) have no entry — the caller treats a missing key as 0. It
	// batches the conversation-list path's per-card count into one query so
	// listing a task's conversations stays O(1) in artifact reads rather than
	// N+1 (the conversation response's artifact_count,
	// internal/server/agent.go). An empty conversationIDs returns an empty map
	// without touching the DB.
	//
	// Mirrors ListByConversation's pool/RLS conventions: app pool in Postgres
	// (RLS-active, so the count is team-scoped exactly like the rows it
	// counts — a non-member counts zero), the one connection in SQLite.
	// orgID stays bound as defense in depth. Detached artifacts (conversation
	// purged → conversation_id NULL) never match and are correctly excluded.
	CountByConversation(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error)

	// ListByConversations returns the artifacts for every given conversation as
	// one flat slice, each ordered newest-first (created_at DESC, id DESC) so a
	// conversation's rows match ListByConversation's order once grouped. It
	// lets a caller projecting many conversations fetch their artifacts in a
	// single query instead of an N+1 of per-conversation ListByConversation
	// calls — the conversation-list response uses it to resolve pending_kind
	// for the parked conversations in a task's conversation list
	// (internal/server/agent.go). Each artifact carries its ConversationID, so
	// the caller groups by it; an empty conversationIDs returns nil without
	// touching the DB.
	//
	// Mirrors ListByConversation's pool/RLS conventions: app pool in Postgres
	// (RLS-active, team-scoped), the one connection in SQLite. orgID stays
	// bound as defense in depth. Detached artifacts (conversation_id NULL) never match.
	ListByConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error)

	// ListByTeam returns the team's artifacts, newest first (the
	// team_created index order). Backs team-level C2 (TFAC-449) through the
	// shared A·6 API. Detached rows (conversation purged → conversation_id
	// NULL) are still the team's and are included.
	ListByTeam(ctx context.Context, orgID, teamID string, opts ArtifactListOpts) ([]domain.Artifact, int, error)

	// ListNonTerminalBySystem returns every artifact for the org whose
	// (kind, state) is still reconcilable — PR draft|open, review pending,
	// branch pushed (domain.IsReconcilableNonTerminal). It is the artifact
	// reconciler's per-cycle working set (TFAC-464), org-wide and bounded:
	// terminal artifacts (merged/closed/submitted/dismissed/deleted) drop
	// out, so the set shrinks as work resolves.
	//
	// Admin-pool / org-wide in Postgres — the same BYPASSRLS, org-scoped
	// shape the scorer's UnscoredTasks and EntityStore's
	// ListActiveTerminalCandidatesSystem use: the reconciler is a background
	// system job with no JWT-claims context, and it must see every team's
	// non-terminal artifacts to keep them fresh. org_id stays bound as
	// defense in depth.
	// Identical to a plain org read in SQLite (single-tenant, no RLS).
	ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Artifact, error)

	// ListByOrgSystem returns the org's artifacts across EVERY team, newest
	// first (created_at DESC, id DESC) — the org-wide source for the
	// bot-activity audit feed (TFAC-483). Unlike ListNonTerminalBySystem (the
	// reconciler's working set, which hides terminal rows) it returns ALL
	// states, terminal included: the feed is a history of the actions the
	// org's bot took, not a worklist, so merged PRs / deleted branches /
	// dismissed reviews must be present. opts filters (provider / kind / state
	// / time window) and pages (limit / offset).
	//
	// Admin-pool / org-wide in Postgres — the same BYPASSRLS, org-scoped shape
	// ListNonTerminalBySystem uses (mirrors ListByConversationSystem's pool selection):
	// the org feed is an org-admin-gated cross-team read with no per-team RLS
	// context, so it must see every team's rows. org_id stays bound in the
	// WHERE clause as defense in depth. Identical to a plain org read in SQLite
	// (single-tenant, no RLS).
	ListByOrgSystem(ctx context.Context, orgID string, opts ArtifactListOpts) ([]domain.Artifact, int, error)
}

// ArtifactListOpts carries the optional filters/paging for
// ArtifactStore.ListByTeam and ListByOrgSystem. The zero value lists every
// artifact (newest first) with no filter or paging.
type ArtifactListOpts struct {
	// Limit caps the number of rows returned. Zero means no limit; the
	// activity feed passes a page size (~50).
	Limit int
	// Offset skips the first N rows, for limit/offset paging ("load more").
	// Only meaningful alongside a positive Limit.
	Offset int
	// CountOnly returns only the filtered total: the count query runs, the row
	// query doesn't, and the page comes back empty. Limit and Offset are
	// ignored. Mirrors ListOpts.CountOnly (the explicit page_size: 0 request).
	CountOnly bool
	// Provider / Kind / State are optional exact-match filters on the matching
	// artifact column. Empty means no filter on that column.
	Provider string
	Kind     string
	State    string
	// Since / Until bound created_at to the half-open window [Since, Until).
	// Each side applies only when non-zero, so the zero value is unbounded.
	Since time.Time
	Until time.Time
}
