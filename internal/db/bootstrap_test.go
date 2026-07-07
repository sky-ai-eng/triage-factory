package db_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBootstrapLocalOrg_FreshInstall pins the local provision path: on a
// genuinely tenant-less DB (the new fresh-install shape — nothing seeds
// at boot or in the migration), BootstrapLocalOrg creates the synthetic
// tenant rows AND the agent + team_agents + shipped defaults via the
// shared BootstrapNewOrg chain.
func TestBootstrapLocalOrg_FreshInstall(t *testing.T) {
	conn := openTenantlessSQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	// Pre-condition: a fresh local DB has zero tenant rows.
	if n := countRows(t, conn, "orgs"); n != 0 {
		t.Fatalf("fresh DB has %d orgs rows; want 0 (boot must provision nothing)", n)
	}

	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapLocalOrg: %v", err)
	}

	// Tenant rows now exist.
	for _, tc := range []struct {
		table string
		want  int
	}{
		{"orgs", 1}, {"teams", 1}, {"users", 1},
		{"org_memberships", 1}, {"memberships", 1},
		{"org_settings", 1}, {"team_settings", 1},
	} {
		if n := countRows(t, conn, tc.table); n != tc.want {
			t.Errorf("%s row count=%d after provision; want %d", tc.table, n, tc.want)
		}
	}

	// team_settings flips auto_delegate_enabled to 1 (local happy path).
	var autoDelegate bool
	if err := conn.QueryRow(
		`SELECT auto_delegate_enabled FROM team_settings WHERE team_id = ?`, runmode.LocalDefaultTeamID,
	).Scan(&autoDelegate); err != nil {
		t.Fatalf("read team_settings: %v", err)
	}
	if !autoDelegate {
		t.Error("team_settings.auto_delegate_enabled=false; want true (local-mode happy path)")
	}

	// Agent + enabled team membership.
	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	if agent == nil {
		t.Fatal("no agents row after provision")
	}
	if agent.DisplayName != "Triage Factory Bot" {
		t.Errorf("DisplayName=%q want Triage Factory Bot", agent.DisplayName)
	}
	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil || !ta.Enabled {
		t.Errorf("default team has no enabled team_agents row: %+v", ta)
	}

	// Shipped defaults materialized through the org template.
	if n := countSystemHandlers(t, conn, runmode.LocalDefaultTeamID); n == 0 {
		t.Error("no shipped event handlers after BootstrapLocalOrg")
	}
}

// TestBootstrapLocalOrg_Idempotent pins re-entrancy: provisioning N times
// (a double-click, or a re-run after a partial provision) leaves exactly
// one of each tenant + agent row and never duplicates the shipped set.
func TestBootstrapLocalOrg_Idempotent(t *testing.T) {
	conn := openTenantlessSQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
			t.Fatalf("BootstrapLocalOrg call %d: %v", i, err)
		}
	}

	for _, table := range []string{"orgs", "teams", "users", "agents", "team_agents", "org_settings", "team_settings"} {
		if n := countRows(t, conn, table); n != 1 {
			t.Errorf("%s row count=%d after 3 provisions; want 1", table, n)
		}
	}
	// The shipped handler set is stable across re-provisions (no duplication).
	after := countSystemHandlers(t, conn, runmode.LocalDefaultTeamID)
	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapLocalOrg re-run: %v", err)
	}
	if again := countSystemHandlers(t, conn, runmode.LocalDefaultTeamID); again != after {
		t.Errorf("system handler count drifted across re-provision: %d → %d", after, again)
	}
}

// Non-resurrection (provision → delete a shipped handler → re-provision →
// it stays deleted) is a property of the *provision endpoint*, not of
// BootstrapLocalOrg itself: the materializer is INSERT-OR-IGNORE keyed on
// (org, team, system_slug), so calling the bare function again WOULD
// re-create a deleted handler. The endpoint (handleSetupStart) no-ops
// once a tenant exists, which is what makes deletions durable — see
// TestSetupStart_NonResurrection in internal/server.

// TestBootstrapLocalOrg_ReEntrantAfterPartial pins that a re-run after a
// partial provision (only the tenant rows landed, the agent + template
// never ran) reaches the full end state.
func TestBootstrapLocalOrg_ReEntrantAfterPartial(t *testing.T) {
	conn := openTenantlessSQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	// Simulate a crash mid-provision: tenant rows created, but the
	// BootstrapNewOrg chain (agent/template/handlers) never ran.
	if err := stores.Orgs.CreateLocalTenant(ctx); err != nil {
		t.Fatalf("CreateLocalTenant (partial): %v", err)
	}
	if n := countRows(t, conn, "agents"); n != 0 {
		t.Fatalf("partial provision unexpectedly created %d agents rows; want 0", n)
	}

	// Re-run reaches the full end state.
	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("re-run after partial: %v", err)
	}
	if n := countRows(t, conn, "agents"); n != 1 {
		t.Errorf("agents row count=%d after re-run; want 1", n)
	}
	if n := countSystemHandlers(t, conn, runmode.LocalDefaultTeamID); n == 0 {
		t.Error("no shipped handlers after re-run; partial provision wasn't completed")
	}
}

// TestBootstrapLocalOrg_PreservesUserDisable pins that re-provisioning
// never flips a bot the user disabled back to enabled.
func TestBootstrapLocalOrg_PreservesUserDisable(t *testing.T) {
	conn := openTenantlessSQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	agent, _ := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrgID)
	if agent == nil {
		t.Fatal("no agent after provision")
	}
	// User disables the bot.
	if err := stores.TeamAgents.SetEnabled(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, agent.ID, false); err != nil {
		t.Fatalf("SetEnabled false: %v", err)
	}
	// Re-provision.
	if err := db.BootstrapLocalOrg(ctx, stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	ta, _ := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, agent.ID)
	if ta == nil {
		t.Fatal("team_agents row vanished")
	}
	if ta.Enabled {
		t.Fatal("re-provision re-enabled the bot; user's disable would leak away")
	}
}

// TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent pins the sequencing
// guard: a caller that wires team-create before org-create gets a
// clear error rather than a silent "team has no bot" outcome.
func TestBootstrapTeamAgent_ErrorsWhenOrgHasNoAgent(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	err := db.BootstrapTeamAgent(ctx, stores, runmode.LocalDefaultOrgID, "some-team")
	if err == nil {
		t.Fatal("BootstrapTeamAgent without prior org bootstrap returned nil; want explicit error")
	}
	if !strings.Contains(err.Error(), "no agent") {
		t.Errorf("error %q does not mention the missing-agent sequencing bug", err.Error())
	}
}

// TestBootstrapNewTeam_SeedsPerTeamDefaults pins the team-create bootstrap:
// a new 2nd+ team in an existing org gets a default-enabled team_agents row
// (so manual delegation works immediately) AND its own copies of the prompts
// + event handlers. Per-team seeding is correct — random-UUID id +
// system_slug, deduped on (org, team, slug) — so the second team materializes
// its own distinct rows. The *source* of those copies is the org
// template (seeded from the shipped lists at org-create by BootstrapNewOrg),
// not the shipped Go slices directly — so this seeds the org first, then adds
// a 2nd team.
func TestBootstrapNewTeam_SeedsPerTeamDefaults(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()

	// Org-create seeds the agent + the org template + the founder's (sentinel)
	// team. The 2nd team then copies the same template.
	if err := db.BootstrapNewOrg(ctx, stores,
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints(),
	); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// A second team in the same org (the "add team" outcome).
	const newTeamID = "00000000-0000-0000-0000-0000000000b2"
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'beta', 'Beta')`,
		newTeamID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("insert second team: %v", err)
	}

	if err := db.BootstrapNewTeam(ctx, stores, runmode.LocalDefaultOrgID, newTeamID); err != nil {
		t.Fatalf("BootstrapNewTeam: %v", err)
	}

	// team_agents row, default-enabled — manual delegation works.
	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetForOrg: %v", err)
	}
	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrgID, newTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil || !ta.Enabled {
		t.Errorf("new team has no default-enabled team_agents row: %+v", ta)
	}

	// The new team got its own per-team system handler copies.
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND team_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrgID, newTeamID,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers: %v", err)
	}
	if n == 0 {
		t.Errorf("new team got 0 system handlers; want its own per-team copies")
	}
	// And its own per-team system prompt copies.
	var np int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM prompts WHERE org_id = ? AND team_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrgID, newTeamID,
	).Scan(&np); err != nil {
		t.Fatalf("count prompts: %v", err)
	}
	if np == 0 {
		t.Errorf("new team got 0 system prompts; want its own per-team copies")
	}
}

// TestBootstrapNewOrg_SeedsFullStack pins the org-create
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
		runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints(),
	); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// Agent + enabled team membership.
	agent, err := stores.Agents.GetForOrg(ctx, runmode.LocalDefaultOrgID)
	if err != nil || agent == nil {
		t.Fatalf("GetForOrg: agent=%v err=%v", agent, err)
	}
	ta, err := stores.TeamAgents.GetForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeam: %v", err)
	}
	if ta == nil || !ta.Enabled {
		t.Errorf("default team has no enabled team_agents row: %+v", ta)
	}

	// Prompts seeded. The id is a random UUID per team copy now,
	// so resolve by system_slug rather than by id.
	got, err := stores.Prompts.GetBySystemSlug(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, "system-pr-review-security")
	if err != nil {
		t.Fatalf("Get prompt: %v", err)
	}
	if got == nil {
		t.Error("shipped prompt system-pr-review-security missing after BootstrapNewOrg")
	}

	// Handlers seeded on the default team.
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrgID,
	).Scan(&n); err != nil {
		t.Fatalf("count handlers: %v", err)
	}
	if n == 0 {
		t.Error("no shipped event handlers after BootstrapNewOrg")
	}
}

// TestOrgTemplate_ForwardOnly pins the load-bearing invariant: editing
// the org template affects FUTURE team creations only. An admin who adds a
// prompt and enables a trigger in the template AFTER the founder's team exists
// sees those edits in the next new team but NOT in the team that already
// existed. It also pins that the enabled state flows (an org enables a shipped
// trigger once in the template; new teams get it enabled — the value prop).
func TestOrgTemplate_ForwardOnly(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID

	// Org-create: agent + template + founder's (sentinel) team materialized
	// from the template-as-shipped.
	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// --- Edit the template (after the founder's team already exists) ---
	// (1) Add an admin-authored prompt.
	const customSlug = "tmpl-custom-1"
	if err := stores.OrgTemplate.CreatePrompt(ctx, org, domain.Prompt{
		ID: "custom-prompt-id", SystemSlug: customSlug, Name: "Org Custom Prompt", Body: "house rules", Source: "user",
	}); err != nil {
		t.Fatalf("CreatePrompt: %v", err)
	}
	// (2) Enable a shipped trigger in the template (they ship disabled).
	triggers, err := stores.OrgTemplate.ListHandlers(ctx, org, domain.EventHandlerKindTrigger)
	if err != nil || len(triggers) == 0 {
		t.Fatalf("ListHandlers(trigger): err=%v n=%d", err, len(triggers))
	}
	enabledSlug := triggers[0].SystemSlug
	if err := stores.OrgTemplate.SetHandlerEnabled(ctx, org, triggers[0].ID, true); err != nil {
		t.Fatalf("SetHandlerEnabled: %v", err)
	}

	// --- Create a 2nd team AFTER the edits ---
	const newTeamID = "00000000-0000-0000-0000-0000000000c3"
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'gamma', 'Gamma')`, newTeamID, org,
	); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if err := db.BootstrapNewTeam(ctx, stores, org, newTeamID); err != nil {
		t.Fatalf("BootstrapNewTeam: %v", err)
	}

	count := func(team, slug string) int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM prompts WHERE org_id = ? AND team_id = ? AND system_slug = ?`, org, team, slug,
		).Scan(&n); err != nil {
			t.Fatalf("count prompt %s/%s: %v", team, slug, err)
		}
		return n
	}
	triggerEnabled := func(team, slug string) bool {
		t.Helper()
		var enabled bool
		if err := conn.QueryRowContext(ctx,
			`SELECT enabled FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?`, org, team, slug,
		).Scan(&enabled); err != nil {
			t.Fatalf("read trigger %s/%s: %v", team, slug, err)
		}
		return enabled
	}

	// The NEW team carries the edits.
	if got := count(newTeamID, customSlug); got != 1 {
		t.Errorf("new team has %d copies of the template-added prompt; want 1 (edit should flow forward)", got)
	}
	if !triggerEnabled(newTeamID, enabledSlug) {
		t.Errorf("new team's copy of the template-enabled trigger is disabled; want enabled (enabled state should flow)")
	}

	// The EXISTING team is untouched (forward-only).
	if got := count(runmode.LocalDefaultTeamID, customSlug); got != 0 {
		t.Errorf("existing team gained %d copies of a prompt added to the template after it was created; want 0 (forward-only)", got)
	}
	if triggerEnabled(runmode.LocalDefaultTeamID, enabledSlug) {
		t.Errorf("existing team's trigger became enabled after a template edit; want still-disabled (forward-only)")
	}
}

// TestBootstrapLocalTenancy_ConstantsMatchRows is the anti-drift gate
// for the local sentinels. The runmode.LocalDefault*ID constants and the
// rows BootstrapLocalOrg writes MUST stay consistent — if either side
// drifts, FKs from resource tables resolve at insert time (the column
// DEFAULT literals in the baseline still backfill org_id/team_id) but the
// store impls' WHERE org_id = runmode.LocalDefaultOrgID lookups return
// zero rows. That's a silent runtime failure mode the test catches
// loudly.
//
// Postgres doesn't need an equivalent because its provisioning uses no
// hardcoded sentinels — every UUID is gen_random_uuid() at insert.
//
// Coverage: all four sentinel constants — org, team, user, agent — plus
// the cross-row membership/team_agent rows that reference combinations of
// them. BootstrapLocalOrg writes the whole tenant + agent set against a
// genuinely tenant-less DB, so this exercises the runtime provision path,
// not a fixture.
func TestBootstrapLocalTenancy_ConstantsMatchRows(t *testing.T) {
	conn := openTenantlessSQLite(t)
	stores := sqlitestore.New(conn)
	if err := db.BootstrapLocalOrg(t.Context(), stores, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapLocalOrg: %v", err)
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
// helpers from package sqlite_test). BootstrapSchemaForTest seeds the
// synthetic tenant rows, so this represents a *provisioned* local
// install — the right fixture for the BootstrapNewOrg/NewTeam tests,
// which assume the org/team rows already exist.
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

// openTenantlessSQLite returns the genuine fresh-install shape: schema
// migrated, but ZERO tenant rows (no orgs/teams/users/settings). This is
// what a brand-new local DB looks like now that nothing provisions at
// boot — the right fixture for exercising BootstrapLocalOrg's own
// tenant-creation path. Uses Migrate directly rather than
// BootstrapSchemaForTest (which seeds the tenant as a convenience).
func openTenantlessSQLite(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return conn
}

func countRows(t *testing.T, conn *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countSystemHandlers(t *testing.T, conn *sql.DB, teamID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM event_handlers WHERE org_id = ? AND team_id = ? AND source = 'system'`,
		runmode.LocalDefaultOrgID, teamID,
	).Scan(&n); err != nil {
		t.Fatalf("count system handlers: %v", err)
	}
	return n
}
