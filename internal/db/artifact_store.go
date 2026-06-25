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
	// updates the existing row's mutable fields (state, target,
	// details_json, run_id, team_id, updated_at). This is the one writer all
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
	// an event-triggered run's exec choke point, which has no user (runs by
	// schema CHECK carry no creator) — must use UpsertSystem instead.
	Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error)

	// UpsertSystem is the admin-pool (BYPASSRLS) variant of Upsert for
	// system-service writers that hold a real (org_id, team_id) identity but
	// no JWT-claims context — chiefly the exec choke point on an
	// event-triggered run (TFAC-459), whose insert is unreachable through
	// tf_app because the artifacts_insert RLS policy demands a team-writing
	// user and the run has none. Mirrors the `...System` admin halves on
	// AgentRuns / RunWorktrees / Reviews. org_id stays bound as defense in
	// depth; team_id on the row is authoritative (it comes from the run).
	// Identical to Upsert in SQLite (single-tenant, no RLS).
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
