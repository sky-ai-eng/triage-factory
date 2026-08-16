package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// AccessChangeLogStore owns the access_change_log table — the small,
// low-volume audit log of governance actions with no external entity:
// org/team membership & role grants/changes/revokes, and credential
// bind/rotate. Capture only; the read/display surface is the future
// team-activity / org-governance view (TFAC-449 bucket C/D).
//
// Unlike SystemLLMRunStore (admin pool, system-written), this table is written
// on the APP pool from inside the claims-bearing WithTx that already runs each
// governance action — the Record call composes with that transaction so the
// log can't diverge from the action. The org-scoped RLS policy
// (org_id = tf.current_org_id() AND tf.user_has_org_access(org_id)) gates both
// the in-tx write and the future audit-view read. SQLite is N=1 and unscoped.
//
// See TFAC-471.
type AccessChangeLogStore interface {
	// Record inserts one audit row for orgID. entry.ID may be empty — both
	// impls then generate a uuid (SQLite app-side, Postgres via
	// gen_random_uuid()). created_at is stamped server-side (the column
	// DEFAULT). Empty entry.ActorUserID / TargetUserID / TeamID / DetailJSON
	// serialize to SQL NULL. Callers own the "never break the action on a
	// recording failure" contract only insofar as a returned error rolls back
	// the surrounding transaction (intentional — the action and its audit row
	// are atomic).
	Record(ctx context.Context, orgID string, entry domain.AccessChange) error

	// RecordSystem is the admin-pool (BYPASSRLS) variant of Record for
	// system-service writers that hold a real org identity but no JWT-claims
	// context — the SSO JIT auto-provisioning seam, whose user has no
	// membership/claims at provisioning time (the same reason grantOrgMembership
	// takes the admin *sql.DB). Same insert as Record, just no RLS/claims
	// context; mirrors external_actions' RecordSystem. The JIT production path
	// composes its audit row inline on the grant's admin-pool tx (the
	// access_change_log same-tx contract — see the server's GrantOrgMembership);
	// RecordSystem is the standalone admin primitive for a writer that doesn't
	// compose into a caller tx. Identical to Record in SQLite (N=1, no pool
	// split). See TFAC-486.
	RecordSystem(ctx context.Context, orgID string, entry domain.AccessChange) error

	// ListByOrg returns orgID's audit rows newest-first, bounded by opts.Limit
	// (≤ 0 → a default page size). For the future org-admin audit view; the
	// Postgres impl reads under the app pool so the org-scoped RLS policy
	// applies.
	ListByOrg(ctx context.Context, orgID string, opts domain.AccessChangeListOpts) ([]domain.AccessChange, int, error)
}
