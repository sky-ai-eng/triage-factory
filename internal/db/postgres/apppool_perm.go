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
// team_agents, secrets). It does NOT prevent the bug — the SKY-387 static
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
