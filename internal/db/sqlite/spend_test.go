package sqlite_test

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSpendStore_SQLite runs the shared SpendStore conformance suite against the
// SQLite impl. Each subtest opens a fresh in-memory DB (which includes the
// llm_spend view from migration 202606260004 via the cached schema bundle) so
// the assertions also prove the view applies on the shipped baseline and
// returns rows. SQLite is N=1 / no RLS, so the store reads directly.
func TestSpendStore_SQLite(t *testing.T) {
	dbtest.RunSpendStoreConformance(t, func(t *testing.T) dbtest.SpendStoreFixture {
		t.Helper()
		conn := openSQLiteSpendConn(t)
		stores := sqlitestore.New(conn)

		// One org agent (UNIQUE per org) for the actor_agent_id passthrough, and
		// two projects (team-scoped + null-team) so the curator conversations'
		// project_id FK (set on curator rows, NULL for every other type) has both
		// a team and a null-team source for the curator team_id snapshot. The
		// local tenant sentinels (org/team/user)
		// already exist via BootstrapSchemaForTest → SeedLocalTenantRows.
		agentID := uuid.New().String()
		if _, err := conn.Exec(
			`INSERT INTO agents (id, org_id) VALUES (?, ?)`, agentID, runmode.LocalDefaultOrgID,
		); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
		// A team-scoped project (curator team-attribution case) and a null-team,
		// org-visibility project (the null-team case). The CHECK only forces a
		// team_id for visibility='team', so 'org' + NULL team_id is valid. The
		// Curator seeder snapshots team_id from whichever project the fixture's
		// TeamID selects (TFAC-476).
		teamProjectID := uuid.New().String()
		if _, err := conn.Exec(
			`INSERT INTO projects (id, name, team_id, org_id, creator_user_id, visibility) VALUES (?, 'spend-test', ?, ?, ?, 'team')`,
			teamProjectID, runmode.LocalDefaultTeamID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		); err != nil {
			t.Fatalf("seed team project: %v", err)
		}
		nullTeamProjectID := uuid.New().String()
		if _, err := conn.Exec(
			`INSERT INTO projects (id, name, team_id, org_id, creator_user_id, visibility) VALUES (?, 'spend-test-org', NULL, ?, ?, 'org')`,
			nullTeamProjectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		); err != nil {
			t.Fatalf("seed null-team project: %v", err)
		}

		// A blueprint + trigger event_handler so an autonomous run can carry a
		// non-NULL trigger_id (conversations.trigger_id FK → event_handlers;
		// the trigger's same-team FK → blueprints). event_type is a stable
		// seeded catalog id.
		// The view passes conversations.trigger_id straight through (TFAC-478).
		blueprintID := uuid.New().String()
		if _, err := conn.Exec(
			`INSERT INTO blueprints (id, name, org_id, team_id, creator_user_id) VALUES (?, 'spend-bp', ?, ?, ?)`,
			blueprintID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID,
		); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		triggerID := uuid.New().String()
		if _, err := conn.Exec(
			`INSERT INTO event_handlers
				(id, org_id, team_id, creator_user_id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability)
			 VALUES (?, ?, ?, ?, 'trigger', 'github:pr:ci_check_failed', ?, 3, 0.5)`,
			triggerID, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, blueprintID,
		); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}

		return dbtest.SpendStoreFixture{
			Store:     stores.Spend,
			OrgID:     runmode.LocalDefaultOrgID,
			TeamID:    runmode.LocalDefaultTeamID,
			UserID:    runmode.LocalDefaultUserID,
			AgentID:   agentID,
			TriggerID: triggerID,
			Seeder:    newSQLiteSpendSeeder(conn, teamProjectID, nullTeamProjectID),
		}
	})
}

// openSQLiteSpendConn mirrors openSQLiteForTest but adds _time_format=sqlite —
// the same DSN production's db.OpenAt uses — so a bound time.Time and a stored
// started_at/created_at serialize to a datetime()-parseable form. The view's
// ListSpend/SpendByCategory time filters wrap both sides in datetime(); without
// the production time format the wrapper would see modernc's default
// time.String() serialization (unparseable) and silently drop every row.
func openSQLiteSpendConn(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

// newSQLiteSpendSeeder owns the raw INSERT shape for each source. origin is
// 'manual' so the conversations_origin_requires_parents CHECK (which only
// constrains origin='blueprint') is satisfied without a
// blueprint/task/prompt graph. Empty CreatorUserID / ActorAgentID and a nil
// Cost serialize to SQL NULL. The two agent arms plant one assistant ledger
// row each and return ITS id — the view's source_id.
func newSQLiteSpendSeeder(conn *sql.DB, teamProjectID, nullTeamProjectID string) dbtest.SpendSeeder {
	seedLedgerRow := func(t *testing.T, convID, model string, cost any, tok dbtest.SpendTokens, at time.Time) string {
		t.Helper()
		res, err := conn.Exec(`
			INSERT INTO messages
				(org_id, conversation_id, role, subtype, content, model,
				 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
				 cost_usd, created_at)
			VALUES (?, ?, 'assistant', '', 'work', ?, ?, ?, ?, ?, ?, ?)
		`, runmode.LocalDefaultOrgID, convID, nullStr(model),
			tok.Input, tok.Output, tok.CacheRead, tok.CacheCreation, cost, at)
		if err != nil {
			t.Fatalf("seed ledger row: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("ledger row id: %v", err)
		}
		return strconv.FormatInt(id, 10)
	}
	return dbtest.SpendSeeder{
		Conversation: func(t *testing.T, f dbtest.ConversationSpendFixture) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversations
					(id, org_id, team_id, creator_user_id, trigger_type, origin, actor_agent_id, trigger_id, model, status, started_at)
				VALUES (?, ?, ?, ?, ?, 'manual', ?, ?, ?, ?, ?)
			`,
				id, runmode.LocalDefaultOrgID, f.TeamID, nullStr(f.CreatorUserID), f.TriggerType,
				nullStr(f.ActorAgentID), nullStr(f.TriggerID), f.Model, f.Status, f.StartedAt,
			); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			return seedLedgerRow(t, id, f.Model, costArg(f.Cost), f.Tokens, f.StartedAt)
		},
		Curator: func(t *testing.T, f dbtest.CuratorSpendFixture) string {
			t.Helper()
			// One curator conversation carrying the (team, creator)
			// attribution snapshot, one cost-stamped ledger row. Non-empty
			// TeamID → the team-scoped project (snapshot carries its team);
			// empty → the null-team project (snapshot is NULL), captured via
			// the same (SELECT team_id FROM projects WHERE id = ?) subquery
			// production uses, so this proves project team → conversation
			// team → view.
			projID := nullTeamProjectID
			if f.TeamID != "" {
				projID = teamProjectID
			}
			creator := f.CreatorUserID
			if creator == "" {
				creator = runmode.LocalDefaultUserID
			}
			convID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO conversations
					(id, org_id, type, creator_user_id, team_id, visibility,
					 trigger_type, origin, runtime, status, project_id, started_at)
				VALUES (?, ?, 'curator', ?, (SELECT team_id FROM projects WHERE id = ?),
				        'private', 'manual', 'curator', 'sdk', NULL, ?, ?)
			`, convID, runmode.LocalDefaultOrgID, creator, projID, projID, f.CreatedAt); err != nil {
				t.Fatalf("seed curator conversation: %v", err)
			}
			// The curator arm's model deliberately stays NULL — the wire
			// contract never exposed a curator model.
			return seedLedgerRow(t, convID, "", f.Cost, f.Tokens, f.CreatedAt)
		},
		System: func(t *testing.T, f dbtest.SystemSpendFixture) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO system_llm_runs
					(id, org_id, job, model, total_cost_usd,
					 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, started_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				id, runmode.LocalDefaultOrgID, f.Job, f.Model, f.Cost,
				f.Tokens.Input, f.Tokens.Output, f.Tokens.CacheRead, f.Tokens.CacheCreation, f.StartedAt,
			); err != nil {
				t.Fatalf("seed system_llm_run: %v", err)
			}
			return id
		},
	}
}

// nullStr returns nil for an empty string so the column lands as SQL NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// costArg returns nil for a nil cost (the in-flight shape → NULL total_cost_usd).
func costArg(c *float64) any {
	if c == nil {
		return nil
	}
	return *c
}

// TestSpendView_SQLite_MigrationApplies pins that the view migration applies on
// the shipped baseline chain via a real goose Migrate (not the cached test
// bundle) and that the view is queryable. Empty DB → empty view, no error.
func TestSpendView_SQLite_MigrationApplies(t *testing.T) {
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var isView int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'view' AND name = 'llm_spend'`,
	).Scan(&isView); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if isView != 1 {
		t.Fatalf("llm_spend view missing after Migrate (found %d)", isView)
	}

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM llm_spend`).Scan(&n); err != nil {
		t.Fatalf("select from llm_spend: %v", err)
	}
	if n != 0 {
		t.Errorf("fresh llm_spend has %d rows, want 0", n)
	}
}
