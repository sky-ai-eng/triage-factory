package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ArtifactStore owns the artifacts table — the single durable,
// run-attributed, polymorphic record of everything a run produces in an
// external system (branch, PR, review, Jira/Linear issue, comment). Every
// capture writer in TFAC-454's Sub-epic A UPSERTs through it; the
// run-detail view, team-level C2, and the audit ledger read from it.
//
// Mirrors the runs store for pool/RLS/scan conventions: app pool in
// Postgres (RLS-active under tf_app, team-scoped via team_id exactly like
// runs), the one connection in SQLite. orgID is required on every method
// and stays in the WHERE/INSERT clause as defense in depth alongside RLS;
// SQLite asserts it equals runmode.LocalDefaultOrgID. See TFAC-455.
type ArtifactStore interface {
	// Upsert inserts the artifact or, on a (org_id, dedup_key) conflict,
	// updates the existing row's mutable fields (state, url, external_id,
	// target, details_json, run_id, team_id, updated_at). This is the one
	// writer all of Sub-epic A shares: the same PR seen via exec and again
	// via reconciliation lands on one row. Returns the stored row with id
	// and timestamps populated. a.ID may be empty (both impls generate a
	// uuid); a non-empty a.ID is honored on insert and ignored on update.
	//
	// Upsert runs on the app pool (RLS-active in Postgres) — the manual-run
	// capture path wraps it in synthetic claims. UpsertSystem is its
	// admin-pool sibling for event-triggered runs, which have no user
	// identity to bind claims to (mirrors PendingPRStore.Create /
	// CreateSystem). Both write identical rows; they differ only in pool.
	Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error)

	// UpsertSystem is Upsert on the admin pool (BYPASSRLS in Postgres), for
	// capture writers running under an event-triggered run with no JWT
	// claims (auto-delegation). team_id on the row still scopes the artifact
	// to the run's owning team; the admin pool just sidesteps the RLS claim
	// requirement the app pool enforces. In SQLite (N=1, no RLS) it is
	// identical to Upsert. See TFAC-460.
	UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error)

	// ListByRun returns every artifact produced by one run, newest first.
	// Backs the run-detail surface (A·6).
	ListByRun(ctx context.Context, orgID, runID string) ([]domain.Artifact, error)

	// ListByTeam returns the team's artifacts, newest first (the
	// team_created index order). Backs team-level C2 (TFAC-449) through the
	// shared A·6 API. Detached rows (run purged → run_id NULL) are still
	// the team's and are included.
	ListByTeam(ctx context.Context, orgID, teamID string, opts ArtifactListOpts) ([]domain.Artifact, error)
}

// ArtifactListOpts carries the optional filters/paging for
// ArtifactStore.ListByTeam. Zero value lists every artifact for the team.
type ArtifactListOpts struct {
	// Limit caps the number of rows returned. Zero means no limit.
	Limit int
}
