package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// tenantSeedExecer is the minimal write surface SeedLocalTenantRows
// needs. Both *sql.DB and *sql.Tx satisfy it, as does the SQLite
// store's internal queryer — so the same body runs from the runtime
// provision path (OrgsStore.CreateLocalTenant) and the test-fixture
// builder (BootstrapSchemaForTest) without either depending on the
// other's handle type.
type tenantSeedExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SeedLocalTenantRows idempotently inserts the synthetic local-mode
// tenant rows — the orgs / users / org_memberships / teams / memberships
// / org_settings / team_settings rows for the runmode.LocalDefault*
// sentinels. It is the runtime analog of the static INSERT block that
// used to live in the v1.11.0 SQLite baseline: provisioning is now an
// explicit user action ("Start your Triage Factory"), not a boot-time
// or migration-time side effect, so the rows are written here when the
// action fires rather than seeded on every fresh install.
//
// INSERT OR IGNORE makes it re-entrant: a re-run after a partial
// provision (or after the user already provisioned) leaves existing
// rows untouched and reaches the same end state. The team_settings row
// flips auto_delegate_enabled to 1 — the schema default is 0
// (multi-mode's "new teams require explicit opt-in"), but local mode is
// the auto-delegate happy path. Every other column on both settings
// rows takes its NOT NULL DEFAULT, so listing only the PK is a formal
// restatement of the schema defaults.
//
// SQLite-only: the SQL is INSERT OR IGNORE and the sentinel IDs are
// hardcoded UUIDs, both of which are the local-mode shape. Multi-mode
// creates real tenant rows per signup in auth_provision.go.
func SeedLocalTenantRows(ctx context.Context, ex tenantSeedExecer) error {
	stmts := []struct {
		desc string
		sql  string
		args []any
	}{
		{
			"orgs",
			`INSERT OR IGNORE INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`,
			[]any{runmode.LocalDefaultOrgID},
		},
		{
			"users",
			`INSERT OR IGNORE INTO users (id, display_name) VALUES (?, 'You')`,
			[]any{runmode.LocalDefaultUserID},
		},
		{
			"org_memberships",
			`INSERT OR IGNORE INTO org_memberships (user_id, org_id, role) VALUES (?, ?, 'owner')`,
			[]any{runmode.LocalDefaultUserID, runmode.LocalDefaultOrgID},
		},
		{
			"teams",
			`INSERT OR IGNORE INTO teams (id, org_id, slug, name) VALUES (?, ?, 'default', 'Default')`,
			[]any{runmode.LocalDefaultTeamID, runmode.LocalDefaultOrgID},
		},
		{
			"memberships",
			`INSERT OR IGNORE INTO memberships (user_id, team_id, role) VALUES (?, ?, 'admin')`,
			[]any{runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID},
		},
		{
			"org_settings",
			`INSERT OR IGNORE INTO org_settings (org_id) VALUES (?)`,
			[]any{runmode.LocalDefaultOrgID},
		},
		{
			"team_settings",
			`INSERT OR IGNORE INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`,
			[]any{runmode.LocalDefaultTeamID},
		},
	}
	for _, s := range stmts {
		if _, err := ex.ExecContext(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed local tenant %s: %w", s.desc, err)
		}
	}
	return nil
}
