package db

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrPerformedByRequired refuses an audit-ledger write whose performed_by is
// empty or not one of the domain.PerformedBy* values. Who performed an act is
// what separates a run's own actions from a person's approval of them, so a
// row that does not say is refused rather than defaulted.
var ErrPerformedByRequired = errors.New("external action performed_by must be agent, person or system")

// RequirePerformedBy refuses a value domain.IsPerformedBy rejects with
// ErrPerformedByRequired. Both dialects' external-action writes call it before
// touching the database.
func RequirePerformedBy(performedBy string) error {
	if !domain.IsPerformedBy(performedBy) {
		return ErrPerformedByRequired
	}
	return nil
}

// ExternalActionStore owns the external_actions table — the append-only audit
// log of record: one row per external write TF performs under an ORG-scoped
// credential (the org GitHub App / the org Jira service account). Capture is core
// substrate; the cross-team/org read surface is EE (gated by FeatureGovernance at
// the HTTP layer). See TFAC-483.
//
// Two write paths, mirroring the Deliverables Upsert/UpsertSystem pool split — this
// is the addition over AccessChangeLogStore (which has only an app-pool Record):
//
//   - Record runs on the APP pool (RLS-active). It composes inside the
//     claims-bearing WithTx that runs the action it audits — the manual bot
//     conversations (under synthetic claims) and the server approval/board
//     handlers
//     (under the approver/dragger's claims) — so the external_actions_all RLS
//     policy's WITH CHECK is exercised and the row commits or rolls back
//     atomically with the action's own DB state change.
//   - RecordSystem runs on the ADMIN pool (BYPASSRLS) for writers that hold a
//     real (org_id) identity but no JWT-claims context: the event-triggered
//     bot conversations and the Jira board mirror, neither of which has a
//     user to bind claims for. Mirrors the `...System` admin halves on Deliverables / Reviews.
//
// Both Record paths use ON CONFLICT(org_id, scope, dedup_key) DO NOTHING —
// append-only, never a mutation. An empty entry.DedupKey is filled with a uuid
// (a unique, non-colliding key) so the row is an unconditional append. A
// caller that sets one gets the collapse: the natural keys of the branch push,
// the egress denial and the spent signing budget (domain.BranchPushDedupKey,
// domain.EgressDenialDedupKey, domain.CommitSignBudgetDedupKey),
// and the per-act key the agent's recording funnel fixes before its first
// attempt, so a retried write lands on the row it may already have written.
// An empty entry.Scope is refused with
// ErrExternalObjectScopeRequired, and an entry.PerformedBy that is not one of
// the domain.PerformedBy* values with ErrPerformedByRequired, both before the
// database is touched. SQLite is N=1 and unscoped; both pools collapse to the
// one connection.
//
// A row references what the act was about by id — entry.EntityID, or
// entry.RepositoryID for a GitHub act with no entity — and the writer resolves
// both before it records the row; the store writes them as given, and their
// foreign keys hold them to rows of the org. Append-only carries one
// amendment: the record of the act is immutable, and the POINTER to where the
// object now lives (current_url) is maintained — a rename of the entity or the
// repository the row references, or a Slack channel's move, fills it inside
// its rewrite transaction, selecting the rows by those ids, and every list
// read serves COALESCE(current_url, url) as the row's URL. No method on this
// interface mutates. The UPDATEs live in those rewrites: a rename touches
// current_url and nothing else, and a fold of one entity or registry row into
// another re-points entity_id or repository_id onto the row that survives.
// See domain.ExternalAction.
type ExternalActionStore interface {
	// Record inserts one audit row for orgID on the APP pool. entry.ID may be
	// empty (the impls generate a uuid; Postgres via gen_random_uuid()).
	// occurred_at is stamped server-side (the column DEFAULT). An empty
	// entry.DedupKey is filled with a uuid so the append never collides; a
	// non-empty key enables the ON CONFLICT DO NOTHING collapse (see the type
	// doc for who sets one). Empty optional columns serialize to SQL NULL. A returned error
	// rolls back the surrounding transaction — the action and its audit row are
	// atomic (intentional).
	//
	// Exempt from the returned-row rule: it appends to an audit log. The row
	// is written to be read by a later listing, never by its own writer.
	Record(ctx context.Context, orgID string, entry domain.ExternalAction) error

	// RecordSystem is the admin-pool (BYPASSRLS) variant of Record for
	// system-service writers with no JWT-claims context — the event-triggered bot
	// exec choke point and the Jira board mirror. Identical insert semantics
	// (uuid fill, ON CONFLICT DO NOTHING); identical to Record in SQLite.
	//
	// Exempt from the returned-row rule: it appends to an audit log, same as
	// Record.
	RecordSystem(ctx context.Context, orgID string, entry domain.ExternalAction) error

	// ListByOrgSystem returns the org's actions across EVERY team, newest first
	// (occurred_at DESC, id DESC) — the org-wide source for the governance org
	// feed. Admin pool / org-wide in Postgres (the org-admin-gated cross-team
	// read has no per-team RLS context), bounded + filtered by opts. org_id stays
	// in the WHERE clause as defense in depth. Identical to a plain org read in
	// SQLite. The second return is the unpaged total under the same filters.
	ListByOrgSystem(ctx context.Context, orgID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error)

	// ListByTeam returns one team's actions, newest first — the team-scoped
	// governance feed. App pool in Postgres: the org-scoped RLS policy gates the
	// read, team_id is filtered in the WHERE clause, and the team-admin/org-admin
	// authorization is enforced at the HTTP handler. Bounded + filtered by opts.
	ListByTeam(ctx context.Context, orgID, teamID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error)

	// ListByConversation returns one conversation's actions, newest first —
	// what a single conversation did to the outside world, the sibling of
	// Deliverables.ListByConversation. App pool in Postgres under the same
	// org-scoped policy as ListByTeam; the handler reads the conversation
	// first, so a conversation the caller's team cannot see is a 404 before
	// this runs. Bounded + filtered by opts.
	//
	// It is a separate read rather than a filter on ListByTeam because the two
	// answer different questions and are reached from different surfaces: this
	// one needs no governance entitlement, since a member of the owning team is
	// already reading that conversation's transcript and deliverables.
	ListByConversation(ctx context.Context, orgID, conversationID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error)

	// ListPRCreatedWithoutDeliverableSystem returns orgID's pr_created rows on
	// scope that occurred at or after since, carry a node id and a
	// conversation, and have no deliverable with that node id in the same org and
	// scope — the pull requests the create verb opened whose deliverable write
	// failed. Oldest first (occurred_at, id). It is the reconciler's PR
	// backstop read, run every cycle, so it is one query with no count: a time
	// bound the (org_id, action, occurred_at) index narrows on, and a NOT
	// EXISTS against deliverables on (org_id, scope, node_id), which
	// idx_deliverables_node_id serves. A row with no node id has nothing to match
	// a deliverable on, and a row whose conversation was purged has no one to
	// own one, so neither is returned. Admin pool (BYPASSRLS): the backstop has
	// no claims and a deliverable may belong to any team. An empty scope is
	// refused with ErrExternalObjectScopeRequired.
	ListPRCreatedWithoutDeliverableSystem(ctx context.Context, orgID, scope string, since time.Time) ([]domain.ExternalAction, error)

	// PresentPostedCommentKeysSystem returns the subset of keys that are the
	// object_key of a comment TF posted in orgID on scope: a row with
	// object_kind 'comment' and action comment_posted or issue_comment_posted,
	// whoever performed it. keys are comment object keys
	// (domain.GitHubCommentKey, or a Jira or Linear comment's
	// domain.ObjectKey), which is what the tracker reads it with to leave TF's
	// own comments out of the events it emits. The (org_id, scope, object_key)
	// index serves it. An empty keys returns an empty map without a query, and
	// an empty scope is refused with ErrExternalObjectScopeRequired. Admin
	// pool (BYPASSRLS): the tracker holds no claims, and the row belongs to
	// whichever team's run posted the comment.
	PresentPostedCommentKeysSystem(ctx context.Context, orgID, scope string, keys []string) (map[string]bool, error)

	// CountByConversations returns, for each of conversationIDs that has any
	// row, its number of rows, whoever performed them and whatever their
	// action. The ledger is append-only, so the number moves with every act
	// the conversation records, a refused one included, whatever its
	// occurred_at says; a reader holding a conversation compares it to tell
	// that its actions changed. A conversation with no row has no entry, and
	// an empty conversationIDs returns an empty map without touching the
	// database. App pool, under the caller's claims.
	CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error)

	// ListAgentActionsForBlueprintRunSystem returns orgID's rows of one
	// action that a run's agent performed (performed_by 'agent') in any of
	// blueprintRunID's step conversations, newest first: by occurred_at, and
	// of two rows at one occurred_at the later write first (SQLite stamps
	// whole seconds, so there the tie is real; Postgres stamps microseconds
	// and breaks the rare tie on id). It is what the router's close phase
	// reads to find out whether the event closing a task was caused by the
	// task's own run: the push whose commit CI passed on, the merge, the
	// transition to done, where "newest" decides which transition counts.
	// performed_by keeps it to the agent's own acts: a person's approval of
	// the run's deliverable carries the run's conversation too. Admin pool
	// (BYPASSRLS): the router holds no claims.
	ListAgentActionsForBlueprintRunSystem(ctx context.Context, orgID, blueprintRunID, action string) ([]domain.ExternalAction, error)
}
