package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// wrapAppPoolPermErr turns a Postgres permission-denied error (SQLSTATE
// 42501) from an app-pool read into an actionable, self-explaining one.
//
// The app pool is a bare `authenticator` connection until SET ROLE tf_app
// + JWT claims are set inside a request tx (see WithTx). A system or
// bootstrap caller that reaches an app-pool read method (GetForOrg,
// GetForTeam, ListForOrg, …) outside that ceremony has no table/function
// grant, so Postgres raises "permission denied for table <t>" / "for
// function <f>" — a cryptic grant error that gives no hint about the real
// fix. That fix is always the same: call the method's `*System`
// admin-pool twin from system/bootstrap code.
//
// This is pure diagnosis, applied at the app-pool read sites of the
// stores whose `*System` convention exists precisely because bootstrap /
// background code must avoid the app pool (agents, github_apps,
// team_agents, secrets). It does NOT prevent the bug — the static
// guard (TestBootstrapUsesAdminPoolStoreCalls) does that for the
// bootstrap chain — it just makes the one remaining runtime failure mode
// legible. The original *pgconn.PgError is wrapped (%w), so callers that
// inspect SQLSTATE still see 42501.
//
// A nil error, or any non-42501 error, passes through untouched. In a
// correctly-wired request handler an app-pool read either succeeds or RLS
// filters it to zero rows; it does not raise 42501 (a missing table grant
// is the only 42501 source on a read, and that only happens on the
// grant-less bare authenticator — exactly the misuse this diagnoses).
func wrapAppPoolPermErr(err error, callsite string) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%s: permission denied — app-pool call with no tf_app role; "+
			"system/bootstrap callers must use the *System variant: %w", callsite, err)
	}
	return err
}

// wrapAdminPoolPermErr turns a Postgres permission-denied error (SQLSTATE
// 42501) from an admin-pool call into an actionable one, naming the table
// (Postgres's own DETAIL already carries it, surfaced here via
// pgErr.TableName) and pointing at the fix.
//
// Historically the admin pool never raised 42501 at all — it connected as
// supabase_admin, the real superuser. PS-H4 changes that for executors:
// their admin pool is tf_system, an enumerated-grant BYPASSRLS role (see
// the tf_system section of internal/db/migrations-postgres/
// 202605130001_pg_baseline.sql). A 42501 here means exactly one thing — a
// table this call touches is missing from that grant list, most likely
// because a later migration added an executor-path table without adding
// its tf_system grant in the same migration (the forward-maintenance
// rule the baseline's tf_system comment calls out). This wrapper is pure
// diagnosis, applied only at the executor's highest-traffic admin-pool
// call sites (the dispatcher's claim CAS, the fleet heartbeat) so a
// production grant gap surfaces immediately and legibly instead of as a
// bare pq error buried in a retry loop. The original *pgconn.PgError is
// wrapped (%w), so callers that inspect SQLSTATE still see 42501.
func wrapAdminPoolPermErr(err error, callsite string) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		table := pgErr.TableName
		if table == "" {
			table = "(unknown — see the wrapped error's DETAIL)"
		}
		return fmt.Errorf("%s: permission denied for tf_system on table %s — "+
			"add its grant to the tf_system section of the pg baseline migration: %w",
			callsite, table, err)
	}
	return err
}
