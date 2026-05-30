package db_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBootstrapLocalAgent_FreshInstall pins the v1 install path:
// after migrations, BootstrapLocalAgent inserts exactly one agents
// row + one team_agents row in the synthetic local org/team.
func TestBootstrapLocalAgent_FreshInstall(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("BootstrapLocalAgent: %v", err)
	}

	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if agent == nil {
		t.Fatal("no agents row after bootstrap")
	}
	if agent.DisplayName != "Triage Factory Bot" {
		t.Errorf("DisplayName=%q want Triage Factory Bot", agent.DisplayName)
	}

	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil {
		t.Fatal("no team_agents row after bootstrap")
	}
	if !ta.Enabled {
		t.Errorf("team_agents.enabled=false; want true (DEFAULT TRUE)")
	}
}

// TestBootstrapLocalAgent_Idempotent pins the v1.10.1 → current
// upgrade path: BootstrapLocalAgent runs every boot, second + Nth
// calls are no-ops (row count stays at 1).
func TestBootstrapLocalAgent_Idempotent(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
			t.Fatalf("BootstrapLocalAgent call %d: %v", i, err)
		}
	}

	var agentCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agentCount); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agentCount != 1 {
		t.Errorf("agents row count=%d after 3 bootstrap calls; want 1", agentCount)
	}
	var teamAgentCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM team_agents`).Scan(&teamAgentCount); err != nil {
		t.Fatalf("count team_agents: %v", err)
	}
	if teamAgentCount != 1 {
		t.Errorf("team_agents row count=%d after 3 bootstrap calls; want 1", teamAgentCount)
	}
}

// TestBootstrapLocalAgent_PreservesUserDisable pins the load-bearing
// invariant for migrating users: if a user has disabled the bot for
// their team, re-running bootstrap on the next boot must NOT flip
// Enabled back to TRUE.
func TestBootstrapLocalAgent_PreservesUserDisable(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	agent, _ := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if agent == nil {
		t.Fatal("no agent after bootstrap")
	}
	// User disables the bot.
	if err := stores.TeamAgents.SetEnabled(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID, false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	// Simulate a restart.
	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	ta, _ := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID)
	if ta == nil {
		t.Fatal("team_agents row vanished")
	}
	if ta.Enabled {
		t.Fatal("bootstrap re-enabled the bot; user's disable would leak away across boots")
	}
}

// TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent pins the sequencing
// guard: a caller that wires team-create before org-create gets a
// clear error rather than a silent "team has no bot" outcome.
func TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	err := db.BootstrapTeamAgent(ctx, stores, runmode.LocalDefaultOrg, "some-team")
	if err == nil {
		t.Fatal("BootstrapTeamAgent without prior org bootstrap returned nil; want explicit error")
	}
	if !strings.Contains(err.Error(), "no agent") {
		t.Errorf("error %q does not mention the missing-agent sequencing bug", err.Error())
	}
}

// TestBootstrapNewTeam_SeedsAgentAndHandlers pins SKY-378's team-create
// bootstrap: a new team in an existing org gets a default-enabled
// team_agents row AND the shipped event handlers scoped to it, so a
// freshly-created team's auto-delegation works out of the box rather
// than starting dead. Uses the local org/team graph from the baseline +
// BootstrapLocalAgent for the org agent, then seeds the org's prompts
// (the shipped trigger rows FK into them) before adding the new team.
func TestBootstrapNewTeam_SeedsAgentAndHandlers(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	// Org agent + the org's shipped prompts must exist first (the
	// shipped triggers FK into prompts).
	if err := db.BootstrapLocalAgent(ctx, stores); err != nil {
		t.Fatalf("BootstrapLocalAgent: %v", err)
	}
	for _, p := range ai.ShippedPrompts() {
		if err := stores.Prompts.SeedOrUpdate(ctx, runmode.LocalDefaultOrg, p); err != nil {
			t.Fatalf("seed prompt %s: %v", p.ID, err)
		}
	}

	// A second team in the same org (the "add team" outcome).
	const newTeamID = "00000000-0000-0000-0000-0000000000b2"
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'beta', 'Beta')`,
		newTeamID, runmode.LocalDefaultOrg,
	); err != nil {
		t.Fatalf("insert second team: %v", err)
	}

	if err := db.BootstrapNewTeam(ctx, stores, runmode.LocalDefaultOrg, newTeamID); err != nil {
		t.Fatalf("BootstrapNewTeam: %v", err)
	}

	// team_agents row, default-enabled.
	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, newTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil || !ta.Enabled {
		t.Errorf("new team has no default-enabled team_agents row: %+v", ta)
	}

	// Shipped handlers landed on the new team.
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND team_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrg, newTeamID,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers: %v", err)
	}
	if n == 0 {
		t.Error("new team got no shipped event handlers; auto-delegation would be dead for it")
	}
}

// TestBootstrapNewOrg_SeedsFullStack pins SKY-378's org-create
// bootstrap: a brand-new org + default team gets the agent, the shipped
// prompts, the shipped handlers, and the team's enabled bot membership —
// the founder-signup path that previously created only the tenant rows.
// Runs against the local sentinel org/team as a stand-in for a freshly
// provisioned multi-mode org (same store contracts).
func TestBootstrapNewOrg_SeedsFullStack(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapNewOrg(ctx, stores,
		runmode.LocalDefaultOrg, db.LocalDefaultTeamID, ai.ShippedPrompts(),
	); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// Agent + enabled team membership.
	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrg)
	if err != nil || agent == nil {
		t.Fatalf("GetForOrg: agent=%v err=%v", agent, err)
	}
	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrg, db.LocalDefaultTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil || !ta.Enabled {
		t.Errorf("default team has no enabled team_agents row: %+v", ta)
	}

	// Prompts seeded.
	got, err := stores.Prompts.Get(ctx, runmode.LocalDefaultOrg, "system-pr-review")
	if err != nil {
		t.Fatalf("Get prompt: %v", err)
	}
	if got == nil {
		t.Error("shipped prompt system-pr-review missing after BootstrapNewOrg")
	}

	// Handlers seeded on the default team.
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrg,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers: %v", err)
	}
	if n == 0 {
		t.Error("no shipped event handlers after BootstrapNewOrg")
	}
}

// TestBootstrapLocalTenancy_ConstantsMatchRows is the anti-drift gate
// for SKY-269. The runmode.LocalDefault*ID constants and the SQL
// literals in 202605120003_local_tenancy.sql's INSERT statements MUST
// stay byte-identical — if either side drifts, FKs from resource
// tables resolve correctly at insert time (because the migration
// backfills the column DEFAULT to the SQL literal) but the store
// impls' WHERE org_id = runmode.LocalDefaultOrgID lookups return zero
// rows. That's a silent runtime failure mode the test catches loudly.
//
// Postgres doesn't need an equivalent because its migration uses no
// hardcoded sentinels — every UUID is gen_random_uuid() at insert.
//
// Coverage: all four sentinel constants — org, team, user, agent —
// plus the two cross-row membership rows that reference combinations
// of them. The agent row is populated by BootstrapLocalAgent at boot
// (not by the migration itself), so the test runs that first to put
// the agent row in place before the constant-matches-row check.
func TestBootstrapLocalTenancy_ConstantsMatchRows(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	if err := db.BootstrapLocalAgent(t.Context(), stores); err != nil {
		t.Fatalf("BootstrapLocalAgent: %v", err)
	}

	for _, c := range []struct {
		name   string
		table  string
		column string
		want   string
	}{
		{"org", "orgs", "id", runmode.LocalDefaultOrgID},
		{"team", "teams", "id", runmode.LocalDefaultTeamID},
		{"user", "users", "id", runmode.LocalDefaultUserID},
		{"agent", "agents", "id", runmode.LocalDefaultAgentID},
	} {
		var n int
		if err := conn.QueryRow(
			`SELECT COUNT(*) FROM `+c.table+` WHERE `+c.column+` = ?`,
			c.want,
		).Scan(&n); err != nil {
			t.Fatalf("count %s.%s=%q: %v", c.table, c.column, c.want, err)
		}
		if n != 1 {
			t.Errorf("%s sentinel row count for %s.%s=%q = %d, want 1 — runmode constant has drifted from migration literal",
				c.name, c.table, c.column, c.want, n)
		}
	}

	// org_memberships + memberships + team_agents cross-reference
	// combinations of the four sentinels. A failure here surfaces a
	// drift on whichever pair doesn't resolve, even when the single-
	// table checks above all pass (e.g. if memberships drifted on
	// team_id but the teams row hadn't).
	for _, c := range []struct {
		name  string
		query string
		args  []any
	}{
		{
			"org_memberships(user, org)",
			`SELECT COUNT(*) FROM org_memberships WHERE user_id = ? AND org_id = ?`,
			[]any{runmode.LocalDefaultUserID, runmode.LocalDefaultOrgID},
		},
		{
			"memberships(user, team)",
			`SELECT COUNT(*) FROM memberships WHERE user_id = ? AND team_id = ?`,
			[]any{runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID},
		},
		{
			"team_agents(team, agent)",
			`SELECT COUNT(*) FROM team_agents WHERE team_id = ? AND agent_id = ?`,
			[]any{runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID},
		},
	} {
		var n int
		if err := conn.QueryRow(c.query, c.args...).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.name, err)
		}
		if n != 1 {
			t.Errorf("%s row count = %d, want 1 — one of the sentinel constants drifted", c.name, n)
		}
	}
}

// openInMemorySQLite gives the bootstrap tests their own SQLite
// fixture (the *_test.go files in package db can't import internal
// helpers from package sqlite_test).
func openInMemorySQLite(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
