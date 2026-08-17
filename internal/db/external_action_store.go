package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ExternalActionStore owns the external_actions table — the append-only audit
// log of record: one row per external write TF performs under an ORG-scoped
// credential (the org GitHub App / the org Jira service account). Capture is core
// substrate; the cross-team/org read surface is EE (gated by FeatureGovernance at
// the HTTP layer). See TFAC-483.
//
// Two write paths, mirroring the Artifacts Upsert/UpsertSystem pool split — this
// is the addition over AccessChangeLogStore (which has only an app-pool Record):
//
//   - Record runs on the APP pool (RLS-active). It composes inside the
//     claims-bearing WithTx that runs the action it audits — the manual bot
//     runs (under synthetic claims) and the server approval/board handlers
//     (under the approver/dragger's claims) — so the external_actions_all RLS
//     policy's WITH CHECK is exercised and the row commits or rolls back
//     atomically with the action's own DB state change.
//   - RecordSystem runs on the ADMIN pool (BYPASSRLS) for writers that hold a
//     real (org_id) identity but no JWT-claims context: the event-triggered bot
//     runs and the Jira board mirror, neither of which has a user to bind claims
//     for. Mirrors the `...System` admin halves on Artifacts / Reviews.
//
// Both Record paths use ON CONFLICT(org_id, dedup_key) DO NOTHING — append-only,
// never a mutation. An empty entry.DedupKey is filled with a uuid (a unique,
// non-colliding key) so the row is an unconditional append; only the branch push
// passes a deterministic key (domain.BranchPushDedupKey) so the git hook+proxy
// twin collapses. SQLite is N=1 and unscoped; both pools collapse to the one
// connection.
//
// Append-only carries one amendment: the record of the act is immutable, and
// the POINTER to where the object now lives (current_url) is maintained — a
// repository rename fills it inside its rewrite transaction, and every list
// read serves COALESCE(current_url, url) as the row's URL. No method on this
// interface mutates; the one UPDATE lives in the rename rewrite, and it may
// touch current_url and nothing else. See domain.ExternalAction.
type ExternalActionStore interface {
	// Record inserts one audit row for orgID on the APP pool. entry.ID may be
	// empty (the impls generate a uuid; Postgres via gen_random_uuid()).
	// occurred_at is stamped server-side (the column DEFAULT). An empty
	// entry.DedupKey is filled with a uuid so the append never collides; a
	// non-empty key (the branch push) enables the ON CONFLICT DO NOTHING twin
	// collapse. Empty optional columns serialize to SQL NULL. A returned error
	// rolls back the surrounding transaction — the action and its audit row are
	// atomic (intentional).
	Record(ctx context.Context, orgID string, entry domain.ExternalAction) error

	// RecordSystem is the admin-pool (BYPASSRLS) variant of Record for
	// system-service writers with no JWT-claims context — the event-triggered bot
	// exec choke point and the Jira board mirror. Identical insert semantics
	// (uuid fill, ON CONFLICT DO NOTHING); identical to Record in SQLite.
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

	// ListByConversation returns one conversation's actions, newest first — what a single
	// run did to the outside world, the sibling of Artifacts.ListByConversation. App pool
	// in Postgres under the same org-scoped policy as ListByTeam; the handler
	// reads the conversation first, so a run the caller's team cannot see is a
	// 404 before this runs. Bounded + filtered by opts.
	//
	// It is a separate read rather than a filter on ListByTeam because the two
	// answer different questions and are reached from different surfaces: this
	// one needs no governance entitlement, since a member of the owning team is
	// already reading that run's transcript and artifacts.
	ListByConversation(ctx context.Context, orgID, conversationID string, opts domain.ExternalActionListOpts) ([]domain.ExternalAction, int, error)
}
