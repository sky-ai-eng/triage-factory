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
// 42501) from an admin-pool call into a legible one, naming the table
// Postgres reports (surfaced here via pgErr.TableName) instead of leaving
// a bare pq error for whoever reads the logs.
//
// Historically the admin pool never raised 42501 at all — it connected as
// supabase_admin, the real superuser. That changed for executors: their
// admin pool is now tf_system, an enumerated-grant BYPASSRLS role (see
// the tf_system section of internal/db/migrations-postgres/
// 202605130001_pg_baseline.sql). A 42501 here means one specific thing —
// a table this call touches is missing from that grant list — and it is
// ALWAYS a Triage Factory bug, never a mistake the person running the
// executor made. The likely cause is a later migration adding an
// executor-path table without adding its tf_system grant in the same
// migration (the forward-maintenance rule the baseline's tf_system
// comment calls out).
//
// The wrapped message is written for whoever is actually staring at this
// log line — for a shipped release, that's a self-hosting operator, not
// a Triage Factory engineer. It deliberately does NOT tell them to edit
// a migration or the pg baseline: self-hosters run the binary we
// distribute, not a checkout of this repo, and this baseline is
// fresh-install-only (see its own "NEVER edit this baseline" header) —
// once it has shipped in a release, the real fix can only land as a NEW
// migration in a future release, authored by us. So the actionable step
// for the reader is "upgrade, or tell us" — never "go patch this
// yourself." (For whoever on our side picks up that report: the fix is a
// migration adding the missing grant, landed and shipped normally —
// never a hand-edit of an operator's already-running database, and,
// post-ship, never an edit to this baseline file either — see the
// forward-maintenance note next to the tf_system grant block.)
//
// This is pure diagnosis, applied at the executor's highest-traffic
// admin-pool call sites (the dispatcher's claim CAS, the fleet
// heartbeat) so a grant gap surfaces immediately instead of buried in a
// retry loop. The original *pgconn.PgError is wrapped (%w), so callers
// that inspect SQLSTATE still see 42501.
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
		return fmt.Errorf("%s: permission denied on table %s — this is a Triage Factory bug "+
			"(a missing database grant), not a local misconfiguration; upgrade to the latest "+
			"release or file an issue at https://github.com/sky-ai-eng/triage-factory/issues "+
			"with this table name: %w", callsite, table, err)
	}
	return err
}
