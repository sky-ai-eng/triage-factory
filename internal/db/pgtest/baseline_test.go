package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestBaseline_AppliesCleanly pins the high-level invariants of the
// migration: every expected schema object is present after goose.Up.
func TestBaseline_AppliesCleanly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	expectedTables := []string{
		"orgs", "teams", "users", "user_github_identities", "user_jira_identities", "memberships", "org_memberships", "sessions",
		"org_settings", "team_settings", "user_settings", "jira_project_status_rules",
		"team_github_groups", "team_github_repos",
		"prompts", "events_catalog", "entities", "entity_links", "events",
		"event_handlers", "tasks", "task_events", "conversations", "claims", "artifacts",
		"messages", "claim_credentials", "conversation_memory", "conversation_memory_entities", "pending_firings", "conversation_worktrees",
		"swipe_events", "poller_state", "repositories",
		// org_secrets replaces the Supabase Vault secret path (TFAC-402):
		// app-encrypted ciphertext in a normal RLS table.
		"org_secrets",
		// system_llm_runs: per-call cost + token accounting for the headless
		// system jobs (scorer/repo-profiler). TFAC-451.
		"system_llm_runs",
		// access_change_log: low-volume audit log of governance actions
		// (membership/role grants/changes/revokes, credential bind/rotate). TFAC-471.
		"access_change_log",
	}
	for _, table := range expectedTables {
		var n int
		err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, table,
		).Scan(&n)
		if err != nil {
			t.Fatalf("probe table %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing after goose.Up", table)
		}
	}

	for _, fn := range []string{
		"current_user_id", "current_org_id",
		"user_has_org_access", "user_is_org_admin", "user_is_team_admin",
		"user_owns_org", "user_is_org_admin_via_team",
		"user_in_team", "user_can_write_team",
	} {
		var n int
		if err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid
			 WHERE n.nspname = 'tf' AND p.proname = $1`, fn,
		).Scan(&n); err != nil {
			t.Fatalf("probe function tf.%s: %v", fn, err)
		}
		if n == 0 {
			t.Errorf("tf.%s missing", fn)
		}
	}

	// TF never creates the public.vault_* secret wrappers — secrets are
	// app-encrypted into org_secrets instead. Assert they're absent so a
	// regression that re-introduces the Vault-backed secret path is
	// caught here.
	for _, fn := range []string{
		"vault_put_org_secret", "vault_get_org_secret", "vault_get_org_secret_system", "vault_delete_org_secret",
		"vault_put_user_secret", "vault_get_user_secret", "vault_get_user_secret_system", "vault_delete_user_secret",
		"vault_put_user_secret_system",
	} {
		var n int
		if err := h.AdminDB.QueryRow(
			`SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid
			 WHERE n.nspname = 'public' AND p.proname = $1`, fn,
		).Scan(&n); err != nil {
			t.Fatalf("probe dropped function public.%s: %v", fn, err)
		}
		if n != 0 {
			t.Errorf("public.%s present — TF must never create the vault_* wrappers", fn)
		}
	}

	// The image pre-creates supabase_vault before this migration runs;
	// the migration drops it immediately since TF doesn't use it. Assert
	// it stays gone so a regression (e.g. a stray CREATE EXTENSION) is
	// caught here.
	var vaultExtCount int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM pg_extension WHERE extname = 'supabase_vault'`,
	).Scan(&vaultExtCount); err != nil {
		t.Fatalf("probe supabase_vault extension: %v", err)
	}
	if vaultExtCount != 0 {
		t.Errorf("supabase_vault extension present — should be dropped by the baseline migration")
	}
}

// TestRoles_TfAppShape pins the exact pg_roles attributes for tf_app
// and the authenticator → tf_app grant. Drift here breaks the entire
// RLS posture, so the assertion is per-bit explicit.
func TestRoles_TfAppShape(t *testing.T) {
	h := Shared(t)

	var canLogin, inherit, bypassRLS bool
	err := h.AdminDB.QueryRow(`
		SELECT rolcanlogin, rolinherit, rolbypassrls
		  FROM pg_roles WHERE rolname = 'tf_app'
	`).Scan(&canLogin, &inherit, &bypassRLS)
	if err != nil {
		t.Fatalf("query tf_app: %v", err)
	}
	if canLogin {
		t.Errorf("tf_app.rolcanlogin = true, want false (NOLOGIN)")
	}
	if inherit {
		t.Errorf("tf_app.rolinherit = true, want false (NOINHERIT)")
	}
	if bypassRLS {
		t.Errorf("tf_app.rolbypassrls = true, want false")
	}

	// authenticator must be granted tf_app so SET ROLE works.
	var granted bool
	err = h.AdminDB.QueryRow(`
		SELECT EXISTS (
		  SELECT 1 FROM pg_auth_members am
		    JOIN pg_roles member ON member.oid = am.member
		    JOIN pg_roles role   ON role.oid   = am.roleid
		   WHERE member.rolname = 'authenticator' AND role.rolname = 'tf_app'
		)
	`).Scan(&granted)
	if err != nil {
		t.Fatalf("query grant: %v", err)
	}
	if !granted {
		t.Errorf("authenticator was not granted tf_app — SET LOCAL ROLE tf_app would fail")
	}
}

// TestSeedData asserts events_catalog rowcount equals
// len(domain.AllEventTypes()). Asserting against the slice length (not
// a hardcoded number) makes drift between the Go event registry and
// the SQL seed list surface here, not at runtime.
func TestSeedData(t *testing.T) {
	h := Shared(t)

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM events_catalog`).Scan(&n); err != nil {
		t.Fatalf("count events_catalog: %v", err)
	}
	want := len(domain.AllEventTypes())
	if n != want {
		t.Errorf("events_catalog rowcount = %d, want %d (len of domain.AllEventTypes)", n, want)
	}

	// Spot-check: a known event ID is present.
	var label string
	if err := h.AdminDB.QueryRow(
		`SELECT label FROM events_catalog WHERE id = 'github:pr:opened'`,
	).Scan(&label); err != nil {
		t.Fatalf("query github:pr:opened: %v", err)
	}
	if label != "PR Opened" {
		t.Errorf("label = %q, want 'PR Opened'", label)
	}
}

// TestSeedEventTypes_OverwritesDrift is the Postgres-side counterpart to
// the SQLite test of the same name (internal/db/event_types_test.go) —
// the ON CONFLICT DO UPDATE SQL text differs by dialect, so it needs its
// own coverage. A row hand-mutated via raw SQL is overwritten back to
// what domain.AllEventTypes() declares when db.SeedEventTypes runs
// again, proving UPSERT semantics rather than insert-only.
func TestSeedEventTypes_OverwritesDrift(t *testing.T) {
	h := Shared(t)

	const id = "github:pr:opened"
	if _, err := h.AdminDB.Exec(
		`UPDATE events_catalog SET label = 'DRIFTED LABEL' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("mutate label: %v", err)
	}
	t.Cleanup(func() {
		if err := db.SeedEventTypes(h.AdminDB, "postgres"); err != nil {
			t.Fatalf("cleanup reseed: %v", err)
		}
	})

	if err := db.SeedEventTypes(h.AdminDB, "postgres"); err != nil {
		t.Fatalf("SeedEventTypes: %v", err)
	}

	var label string
	if err := h.AdminDB.QueryRow(`SELECT label FROM events_catalog WHERE id = $1`, id).Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "PR Opened" {
		t.Errorf("label = %q after SeedEventTypes, want 'PR Opened' (drift not overwritten)", label)
	}
}

// TestSeedEventTypes_UnchangedRowsSkipWrite is the Postgres-side
// counterpart to the SQLite test of the same name
// (internal/db/event_types_test.go): the WHERE guard on DO UPDATE must
// make reseeding with unchanged values a true no-op (no new tuple
// version), which matters most on Postgres where every attempted UPDATE
// costs a WAL entry and a dead tuple for autovacuum regardless of whether
// any column value actually changed. An AFTER UPDATE trigger makes "was a
// write attempted" observable, since row content alone can't distinguish
// a no-op from an update that wrote the same values back.
func TestSeedEventTypes_UnchangedRowsSkipWrite(t *testing.T) {
	h := Shared(t)

	if _, err := h.AdminDB.Exec(`
		CREATE TABLE test_ec_update_counter (n INT NOT NULL DEFAULT 0);
		INSERT INTO test_ec_update_counter (n) VALUES (0);
		CREATE OR REPLACE FUNCTION test_count_ec_updates() RETURNS trigger AS $$
		BEGIN
			UPDATE test_ec_update_counter SET n = n + 1;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_ec_update_trigger AFTER UPDATE ON events_catalog
			FOR EACH ROW EXECUTE FUNCTION test_count_ec_updates();
	`); err != nil {
		t.Fatalf("install update counter: %v", err)
	}
	t.Cleanup(func() {
		if _, err := h.AdminDB.Exec(`
			DROP TRIGGER IF EXISTS test_ec_update_trigger ON events_catalog;
			DROP FUNCTION IF EXISTS test_count_ec_updates();
			DROP TABLE IF EXISTS test_ec_update_counter;
		`); err != nil {
			t.Fatalf("cleanup update counter: %v", err)
		}
		if err := db.SeedEventTypes(h.AdminDB, "postgres"); err != nil {
			t.Fatalf("cleanup reseed: %v", err)
		}
	})

	// Reseeding with unchanged values must not fire the trigger at all.
	if err := db.SeedEventTypes(h.AdminDB, "postgres"); err != nil {
		t.Fatalf("SeedEventTypes (unchanged): %v", err)
	}
	assertPGUpdateCounter(t, h, 0)

	// Mutating exactly one row and reseeding must fire the trigger exactly
	// once — proves the guard isn't accidentally suppressing real changes.
	const id = "github:pr:opened"
	if _, err := h.AdminDB.Exec(
		`UPDATE events_catalog SET label = 'DRIFTED LABEL' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("mutate label: %v", err)
	}
	// The mutation itself fires the trigger once; reset before the real
	// assertion so the counter isolates SeedEventTypes's own writes.
	if _, err := h.AdminDB.Exec(`UPDATE test_ec_update_counter SET n = 0`); err != nil {
		t.Fatalf("reset counter: %v", err)
	}

	if err := db.SeedEventTypes(h.AdminDB, "postgres"); err != nil {
		t.Fatalf("SeedEventTypes (one drifted row): %v", err)
	}
	assertPGUpdateCounter(t, h, 1)
}

func assertPGUpdateCounter(t *testing.T, h *Harness, want int) {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(`SELECT n FROM test_ec_update_counter`).Scan(&n); err != nil {
		t.Fatalf("read update counter: %v", err)
	}
	if n != want {
		t.Errorf("events_catalog UPDATE trigger fired %d times, want %d", n, want)
	}
}

// TestBaseline_JiraStatusRulesCompleteOrEmpty_CHECKsFire pins the two
// jpsr_*_complete_or_empty CHECK constraints. The HTTP handler's
// validateProjectRules is the user-facing gate; these CHECKs are
// defense-in-depth so any path that bypasses validation (admin UI in
// multi mode, direct SQL, restore from backup) still can't persist a
// half-mapped write-target rule.
//
// What they refuse is members without a canonical or a canonical without
// members — a rule that cannot be executed, since the canonical is the status
// TF transitions a ticket INTO. What they permit, and the happy-path cases
// below prove, is a rule with NEITHER: a project may be watched without being
// armed, and pickup carries no constraint at all now that it has nothing to be
// cross-checked against.
func TestBaseline_JiraStatusRulesCompleteOrEmpty_CHECKsFire(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Need a team_id to satisfy the FK. Use the same seed helpers as
	// the rest of the suite so org bootstrap stays consistent.
	owner := SeedUser(t, h, "check-owner")
	orgID := SeedOrg(t, h, "check-org", owner)
	teamID := SeedTeam(t, h, orgID, "check-team")
	_ = orgID // referenced only for FK chain

	const insertRule = `INSERT INTO jira_project_status_rules
		(team_id, project_key, pickup_members,
		 in_progress_members, in_progress_canonical,
		 done_members, done_canonical)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb)`

	const (
		toDo       = `[{"id":"10000","name":"To Do"}]`
		inProgress = `[{"id":"10001","name":"In Progress"}]`
		inProgOne  = `{"id":"10001","name":"In Progress"}`
		done       = `[{"id":"10002","name":"Done"}]`
		doneOne    = `{"id":"10002","name":"Done"}`
		empty      = `[]`
	)

	refused := []struct {
		name                                                       string
		pickup, inProgMembers, inProgCanon, doneMembers, doneCanon any
		wantConstraint                                             string
	}{
		{
			name:   "in_progress members with no canonical",
			pickup: toDo, inProgMembers: inProgress, inProgCanon: nil,
			doneMembers: done, doneCanon: doneOne,
			wantConstraint: "jpsr_in_progress_complete_or_empty",
		},
		{
			name:   "in_progress canonical with no members",
			pickup: toDo, inProgMembers: empty, inProgCanon: inProgOne,
			doneMembers: done, doneCanon: doneOne,
			wantConstraint: "jpsr_in_progress_complete_or_empty",
		},
		{
			name:   "done members with no canonical",
			pickup: toDo, inProgMembers: inProgress, inProgCanon: inProgOne,
			doneMembers: done, doneCanon: nil,
			wantConstraint: "jpsr_done_complete_or_empty",
		},
		{
			name:   "done canonical with no members",
			pickup: toDo, inProgMembers: inProgress, inProgCanon: inProgOne,
			doneMembers: empty, doneCanon: doneOne,
			wantConstraint: "jpsr_done_complete_or_empty",
		},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.AdminDB.Exec(insertRule, teamID, "SKY",
				tc.pickup, tc.inProgMembers, tc.inProgCanon, tc.doneMembers, tc.doneCanon)
			if err == nil {
				t.Fatalf("expected CHECK violation for %q, got nil", tc.name)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("expected pg error, got %T: %v", err, err)
			}
			if pgErr.Code != "23514" { // check_violation
				t.Errorf("expected SQLSTATE 23514, got %s: %v", pgErr.Code, err)
			}
			if !strings.Contains(pgErr.Message, tc.wantConstraint) && !strings.Contains(pgErr.ConstraintName, tc.wantConstraint) {
				t.Errorf("expected CHECK %q in error, got: %v (constraint=%s)", tc.wantConstraint, pgErr.Message, pgErr.ConstraintName)
			}
		})
	}

	accepted := []struct {
		name                                                       string
		key                                                        string
		pickup, inProgMembers, inProgCanon, doneMembers, doneCanon any
	}{
		{"watched with no rules at all", "UNARMED", empty, empty, nil, empty, nil},
		{"pickup mapped, write targets not", "PARTIAL", toDo, empty, nil, empty, nil},
		{"fully armed", "ARMED", toDo, inProgress, inProgOne, done, doneOne},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.AdminDB.Exec(insertRule, teamID, tc.key,
				tc.pickup, tc.inProgMembers, tc.inProgCanon, tc.doneMembers, tc.doneCanon); err != nil {
				t.Fatalf("expected %q to be storable, got: %v", tc.name, err)
			}
		})
	}
}

// TestBaseline_OrgSettingsMaxLLMTier_AppValidated pins the baseline
// decision that org_settings.max_llm_model_tier is an opaque, app-validated,
// provider-agnostic identifier with NO DB CHECK. NULL ("no cap"), the three
// app-known tiers, AND a value the app doesn't know today (a future
// OpenAI/Bedrock family) must all insert at the DB layer — the settings
// handler is the validation gate, not the column. Dropping the CHECK is what
// lets new model families land with zero DDL in a later migration.
func TestBaseline_OrgSettingsMaxLLMTier_AppValidated(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	owner := SeedUser(t, h, "tier-owner")
	orgID := SeedOrg(t, h, "tier-org", owner)

	for _, tier := range []sql.NullString{
		{Valid: false},
		{String: "haiku", Valid: true},
		{String: "sonnet", Valid: true},
		{String: "opus", Valid: true},
		// Not an app-known tier today — must still pass the DB (no CHECK).
		{String: "gpt-5-future", Valid: true},
	} {
		name := "null"
		if tier.Valid {
			name = tier.String
		}
		t.Run("accepted/"+name, func(t *testing.T) {
			if _, err := h.AdminDB.Exec(
				`DELETE FROM org_settings WHERE org_id = $1`, orgID,
			); err != nil {
				t.Fatalf("reset org_settings: %v", err)
			}
			var arg any
			if tier.Valid {
				arg = tier.String
			}
			if _, err := h.AdminDB.Exec(
				`INSERT INTO org_settings (org_id, max_llm_model_tier) VALUES ($1, $2)`,
				orgID, arg,
			); err != nil {
				t.Errorf("INSERT max_llm_model_tier=%v: %v (column must be app-validated, no DB CHECK)", arg, err)
			}
		})
	}
}

// TestBaseline_SourceCHECKsDropped pins the baseline decision that the
// `source IN (...)` enum CHECK is dropped on prompts / blueprints /
// event_handlers, making `source` uniformly app-validated. The structural
// pairing CHECK survives in its harmonized `source <> 'system'` form: a
// non-system source still requires a creator. The acceptance anchor —
// inserting a hypothetical source='imported' handler WITH a creator passes —
// is the event_handlers case.
func TestBaseline_SourceCHECKsDropped(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")

	// event_handlers: source='imported' (not in the old enum) + a creator
	// passes the harmonized source<>'system' pairing CHECK.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, source, name, default_priority, sort_order)
		VALUES ($1, $2, $3, 'rule', 'github:pr:opened', 'imported', 'imported-rule', 0.5, 0)
	`, orgA, alice, teamA); err != nil {
		t.Errorf("INSERT event_handlers source='imported' with creator: %v (source CHECK must be dropped, pairing must tolerate non-system)", err)
	}

	// The harmonized pairing still bites: source='imported' with NULL creator
	// must fail the system_has_no_creator CHECK.
	_, err := h.AdminDB.Exec(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, source, name, default_priority, sort_order)
		VALUES ($1, NULL, $2, 'rule', 'github:pr:opened', 'imported', 'imported-rule-2', 0.5, 0)
	`, orgA, teamA)
	if err == nil {
		t.Fatalf("INSERT source='imported' with NULL creator succeeded; want system_has_no_creator CHECK failure")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("err = %v, want SQLSTATE 23514 (system_has_no_creator)", err)
	}

	// prompts + blueprints: source='imported' + creator inserts cleanly.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
		VALUES ($1, $2, $3, 'imported-prompt', '', 'imported')
	`, orgA, alice, teamA); err != nil {
		t.Errorf("INSERT prompts source='imported': %v (source CHECK must be dropped)", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3, 'imported-blueprint', 'imported')
	`, orgA, alice, teamA); err != nil {
		t.Errorf("INSERT blueprints source='imported': %v (source CHECK must be dropped)", err)
	}
}

// TestRLS_AdminConnectionBypassesRLS pins the harness contract: tests
// run through AdminDB see all rows regardless of RLS policies. If
// this ever fails, it means tf_app or some other role's BYPASSRLS bit
// changed, which would invalidate every RLS test in the suite (false
// passes from a connection that wasn't actually bypassing RLS).
func TestRLS_AdminConnectionBypassesRLS(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, _ := SeedOrgWithUser(t, h, "alice")
	orgB, _, _ := SeedOrgWithUser(t, h, "bob")

	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM orgs WHERE id IN ($1, $2)`, orgA, orgB).Scan(&n); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if n != 2 {
		t.Errorf("AdminDB saw %d orgs, want 2 — RLS not actually bypassed", n)
	}
}

// TestRLS_CrossOrgIsolation is the core RLS assertion. Two orgs, two
// users, each user can only see their own org's data — even when they
// hand-craft INSERTs/SELECTs with the OTHER org's UUIDs.
func TestRLS_CrossOrgIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, userA, _ := SeedOrgWithUser(t, h, "alice")
	orgB, userB, _ := SeedOrgWithUser(t, h, "bob")

	// Seed an entity + task under each org via AdminDB (RLS-bypassed).
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	entityB := seedEntity(t, h, orgB, "github", "octo/repo#1")
	taskA := seedTask(t, h, orgA, userA, entityA, "github:pr:opened")
	taskB := seedTask(t, h, orgB, userB, entityB, "github:pr:opened")

	// Alice (in orgA) SELECTs from tasks — must see only her task.
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != taskA {
			t.Errorf("alice saw tasks %v, want [%s]", ids, taskA)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice select: %v", err)
	}

	// Alice tries to INSERT a task with orgB's org_id directly — RLS
	// WITH CHECK rejects it (her current_org_id = orgA, but the row
	// being inserted has org_id = orgB).
	primaryEventB := getEventForEntity(t, h, entityB)
	err = h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO tasks (org_id, creator_user_id, entity_id, event_type, primary_event_id)
			VALUES ($1, $2, $3, 'github:pr:opened', $4)
		`, orgB, userA, entityB, primaryEventB)
		return err
	})
	if err == nil {
		t.Errorf("alice INSERT into orgB tasks did not fail — RLS WITH CHECK broken")
	} else {
		assertPgCode(t, err, "42501", "alice INSERT into orgB tasks")
	}

	// Bob in orgB sees only taskB.
	err = h.WithUser(t, userB, orgB, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != taskB {
			t.Errorf("bob saw tasks %v, want [%s]", ids, taskB)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob select: %v", err)
	}
}

// TestRLS_UsersIsolation — public.users is org-scoped via the
// "shares at least one org with caller" policy. Alice in orgA cannot
// see bob in orgB; both can see themselves; co-workers in the same
// org can see each other (so display_name/avatar resolve for task
// authors etc.).
func TestRLS_UsersIsolation(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := SeedOrgWithUser(t, h, "alice")
	_, bob, _ := SeedOrgWithUser(t, h, "bob") // separate org
	// Co-worker: charlie joins alice's team.
	charlie := SeedUser(t, h, "charlie")
	teamA := getOrgTeam(t, h, orgA)
	AddOrgMember(t, h, charlie, orgA, teamA, "member", "member")

	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var ids []string
		rows, err := tx.Query(`SELECT id FROM users ORDER BY display_name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Alice should see herself + charlie (same org), but NOT bob.
		seen := make(map[string]bool)
		for _, id := range ids {
			seen[id] = true
		}
		if !seen[alice] {
			t.Errorf("alice can't see her own user row")
		}
		if !seen[charlie] {
			t.Errorf("alice can't see charlie's user row (same org)")
		}
		if seen[bob] {
			t.Errorf("alice CAN see bob's user row across orgs — RLS broken")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}

	// Alice can update her own display_name.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, "alice-renamed", alice)
		return err
	})
	if err != nil {
		t.Fatalf("alice self-update: %v", err)
	}

	// Alice CANNOT update bob's row.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET display_name = $1 WHERE id = $2`, "bob-pwned", bob)
		if err != nil {
			return err
		}
		rows, _ := res.RowsAffected()
		if rows != 0 {
			t.Errorf("alice's UPDATE on bob affected %d rows, want 0 (RLS WITH CHECK should reject)", rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice cross-org update: %v", err)
	}
}

// TestRLS_UsersVisibleWithoutTeamMembership catches a regression
// where users_select joined through memberships+teams instead of
// org_memberships. A founder who has just created their org has an
// org_memberships row but may not yet have any memberships row;
// the old policy made them invisible to org-mates added afterward.
func TestRLS_UsersVisibleWithoutTeamMembership(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Founder: org owner with org_memberships row, NO memberships row.
	founder := SeedUser(t, h, "founder")
	orgID := SeedOrg(t, h, "founder-org", founder)
	teamID := SeedTeam(t, h, orgID, "default")
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, founder, orgID)
	// (deliberately no memberships row for founder)

	// Joiner: org member via org_memberships AND on the team.
	joiner := SeedUser(t, h, "joiner")
	AddOrgMember(t, h, joiner, orgID, teamID, "member", "member")

	// Joiner must be able to see the founder via the users table
	// even though founder isn't on any team.
	err := h.WithUser(t, joiner, orgID, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT display_name FROM users WHERE id = $1`, founder).Scan(&name); err != nil {
			return err
		}
		if name != "founder" {
			t.Errorf("got display_name = %q, want 'founder'", name)
		}
		return nil
	})
	if err != nil {
		t.Errorf("joiner can't see team-less founder: %v — users_select must use org_memberships, not memberships+teams", err)
	}
}

// TestRLS_TeamSettingsIsTeamMemberOnly catches the team_settings
// regression where SELECT was open to all org members. A member of
// team A should not see team B's settings even within the same org.
func TestRLS_TeamSettingsIsTeamMemberOnly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	// teamB is a second team in the same org; bob is on teamB only.
	teamB := SeedTeam(t, h, orgA, "team-b")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamB, "member", "admin")

	// Alice writes team_settings for HER team (teamA).
	if _, err := h.AdminDB.Exec(`INSERT INTO team_settings (team_id) VALUES ($1)`, teamA); err != nil {
		t.Fatalf("seed team_settings: %v", err)
	}

	// Bob (member of teamB, same org as teamA) must NOT see teamA's settings.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM team_settings WHERE team_id = $1`, teamA).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("bob (teamB) saw %d team_settings rows for teamA, want 0 — gate must require team membership, not just org membership", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}

	// Sanity: alice (on teamA) CAN see her own team's settings.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM team_settings WHERE team_id = $1`, teamA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("alice saw %d rows for her own team's settings, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice sanity: %v", err)
	}
}

// TestRLS_TeamVisibilityIsTeamScoped — a row with visibility='team'
// must only be visible to members of that specific team_id, not to
// every org member. Covers all three tables that use this pattern:
// prompts, event_handlers (rule + trigger kinds).
//
// Subtle bug this guards against: in the EXISTS subquery,
// `m.team_id = team_id` is ambiguous — SQL name resolution binds the
// unqualified `team_id` to memberships.team_id (innermost scope),
// making the predicate `m.team_id = m.team_id` which is always true
// for any membership row the EXISTS scans. The correct form
// qualifies the outer table explicitly: `m.team_id = <outer>.team_id`.
// All three policies had this footgun; this test exercises each.
func TestRLS_TeamVisibilityIsTeamScoped(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	carol := SeedUser(t, h, "carol")
	// Bob joins teamA; carol joins a NEW team in the same org. Both
	// are org-level 'member' (only alice as founder is org-level admin).
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	teamB := SeedTeam(t, h, orgA, "team-b")
	AddOrgMember(t, h, carol, orgA, teamB, "member", "member")

	// Seed one team-scoped row in each of the four tables.
	// A trigger references a blueprint (FK to the same-team blueprints), so
	// seed a teamA-owned blueprint for the trigger to point at.
	var teamBlueprintID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3, 'team-blueprint', 'user') RETURNING id
	`, orgA, alice, teamA).Scan(&teamBlueprintID); err != nil {
		t.Fatalf("seed team blueprint: %v", err)
	}
	var teamPromptID, teamRuleID, teamTriggerID string

	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body)
		VALUES ($1, $2, $3, 'team-prompt', '') RETURNING id
	`, orgA, alice, teamA).Scan(&teamPromptID); err != nil {
		t.Fatalf("seed team prompt: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, name, default_priority, sort_order)
		VALUES ($1, $2, $3, 'rule', 'github:pr:opened', 'team-rule', 0.5, 0) RETURNING id
	`, orgA, alice, teamA).Scan(&teamRuleID); err != nil {
		t.Fatalf("seed team rule: %v", err)
	}
	if err := h.AdminDB.QueryRow(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, blueprint_id, event_type, breaker_threshold, min_autonomy_suitability)
		VALUES ($1, $2, $3, 'trigger', $4, 'github:pr:opened', 4, 0.0) RETURNING id
	`, orgA, alice, teamA, teamBlueprintID).Scan(&teamTriggerID); err != nil {
		t.Fatalf("seed team trigger: %v", err)
	}

	// Bob (in teamA) should see all four.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		for _, c := range []struct {
			label, query, id string
		}{
			{"prompts", `SELECT 1 FROM prompts WHERE id = $1`, teamPromptID},
			{"event_handlers/rule", `SELECT 1 FROM event_handlers WHERE id = $1`, teamRuleID},
			{"event_handlers/trigger", `SELECT 1 FROM event_handlers WHERE id = $1`, teamTriggerID},
		} {
			var n int
			if err := tx.QueryRow(c.query, c.id).Scan(&n); err != nil {
				t.Errorf("bob can't see teamA-scoped %s row: %v", c.label, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}

	// Carol (in teamB, same org, DIFFERENT team) must NOT see any of
	// the four. The unqualified-team_id bug would let her see all of
	// them.
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		for _, c := range []struct {
			label, query, id string
		}{
			{"prompts", `SELECT COUNT(*) FROM prompts WHERE id = $1`, teamPromptID},
			{"event_handlers/rule", `SELECT COUNT(*) FROM event_handlers WHERE id = $1`, teamRuleID},
			{"event_handlers/trigger", `SELECT COUNT(*) FROM event_handlers WHERE id = $1`, teamTriggerID},
		} {
			var n int
			if err := tx.QueryRow(c.query, c.id).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("carol (different team) saw teamA-scoped %s row — outer-table-qualified team_id check broken", c.label)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol query: %v", err)
	}
}

// TestRLS_RevokedMembership — if a user's session still carries
// org_id claims after the underlying membership is gone, RLS must
// still gate them. Without `tf.user_has_org_access(org_id)` in the
// USING clause, the user could read rows they created in that org
// even after being kicked out. Two independent gates: claims match
// AND live membership.
//
// Uses a regular member (bob), not the org owner, so the test
// doesn't hit the "must retain at least one owner" invariant.
func TestRLS_RevokedMembership(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")

	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	taskB := seedTask(t, h, orgA, bob, entityA, "github:pr:opened")

	// Sanity: bob can see his task pre-revocation.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var id string
		return tx.QueryRow(`SELECT id FROM tasks WHERE id = $1`, taskB).Scan(&id)
	})
	if err != nil {
		t.Fatalf("bob pre-revocation: %v", err)
	}

	// Revoke bob's membership at BOTH levels. Removing the team
	// membership alone is no longer enough — user_has_org_access
	// queries org_memberships in the two-axis model.
	MustExec(t, h.AdminDB, `DELETE FROM memberships WHERE user_id = $1`, bob)
	MustExec(t, h.AdminDB, `DELETE FROM org_memberships WHERE user_id = $1`, bob)

	// Bob's session still carries claims {sub: bob, org_id: orgA}
	// but org_memberships row is gone → tf.user_has_org_access(orgA) now
	// returns false. Task SELECT must return 0 rows.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id FROM tasks`)
		if err != nil {
			return err
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("bob saw %d tasks after membership revoked, want 0", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob post-revocation: %v", err)
	}
}

// TestRLS_OrgAdminGate — non-admin members can read orgs/teams but
// can't UPDATE the org row (rename, etc.) or CREATE/DELETE teams.
// Catches the original "any member could mutate org-wide attributes"
// privilege escalation.
func TestRLS_OrgAdminGate(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Alice is the org owner; bob is a plain org+team member.
	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice (owner) can rename the org.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'renamed' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("alice (owner) UPDATE affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice rename: %v", err)
	}

	// Bob (member) CANNOT rename the org. RLS UPDATE policy filters
	// the row out; UPDATE affects 0 rows.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'bob-takeover' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob (member) UPDATE affected %d rows, want 0 (admin gate)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob rename attempt: %v", err)
	}

	// Bob CANNOT create a new team — INSERT WITH CHECK fails.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'bob-team', 'Bob Team')`, orgA,
		)
		return err
	})
	if err == nil {
		t.Errorf("bob (member) created a new team — admin gate broken")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("expected RLS violation (SQLSTATE 42501) on team INSERT, got: %v", err)
		}
	}

	// Bob CANNOT delete the existing team.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM teams WHERE id = $1`, teamA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob (member) DELETE on team affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob team delete: %v", err)
	}

	// Bob (member) CAN still SELECT the org + team — read access
	// is still org-wide.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRow(`SELECT name FROM orgs WHERE id = $1`, orgA).Scan(&name); err != nil {
			t.Errorf("bob SELECT org: %v", err)
		}
		if err := tx.QueryRow(`SELECT name FROM teams WHERE id = $1`, teamA).Scan(&name); err != nil {
			t.Errorf("bob SELECT team: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob read-only access: %v", err)
	}
}

// TestRLS_SettingsAdminOnly — non-admin members can read org_settings
// + jira_project_status_rules but can't write them.
func TestRLS_SettingsAdminOnly(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice (owner) creates an org_settings row.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO org_settings (org_id) VALUES ($1)`, orgA)
		return err
	})
	if err != nil {
		t.Fatalf("alice (owner) INSERT org_settings: %v", err)
	}

	// Bob can SELECT (org member).
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		var proto string
		return tx.QueryRow(
			`SELECT github_clone_protocol FROM org_settings WHERE org_id = $1`, orgA,
		).Scan(&proto)
	})
	if err != nil {
		t.Errorf("bob SELECT org_settings: %v", err)
	}

	// Bob cannot UPDATE — UPDATE policy admin-gated; filters row out.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE org_settings SET github_clone_protocol = 'https' WHERE org_id = $1`, orgA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob UPDATE org_settings affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob update attempt: %v", err)
	}

	// Bob cannot INSERT a jira rule — WITH CHECK fails. (jira rules
	// are team-keyed; bob is a team member but not a team admin.) Use
	// fully-populated values so we surface the RLS error rather than
	// the jpsr_*_populated CHECK constraints.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jira_project_status_rules (
				team_id, project_key,
				pickup_members, in_progress_members, in_progress_canonical,
				done_members, done_canonical
			) VALUES ($1, 'SKY',
				'[{"id":"10000","name":"To Do"}]'::jsonb,
				'[{"id":"10001","name":"In Progress"}]'::jsonb, '{"id":"10001","name":"In Progress"}'::jsonb,
				'[{"id":"10002","name":"Done"}]'::jsonb, '{"id":"10002","name":"Done"}'::jsonb)
		`, teamA)
		return err
	})
	if err == nil {
		t.Errorf("bob INSERT jira rule succeeded — admin gate broken")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("expected PostgreSQL permission error, got: %v", err)
		} else if pgErr.Code != "42501" {
			t.Errorf("expected SQLSTATE 42501 for RLS violation, got %s: %v", pgErr.Code, err)
		}
	}

	// Alice (owner) can INSERT a jira rule — owner is implicitly
	// admin on every team in the org.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO jira_project_status_rules (
				team_id, project_key,
				pickup_members, in_progress_members, in_progress_canonical,
				done_members, done_canonical
			) VALUES ($1, 'SKY',
				'[{"id":"10000","name":"To Do"}]'::jsonb,
				'[{"id":"10001","name":"In Progress"}]'::jsonb, '{"id":"10001","name":"In Progress"}'::jsonb,
				'[{"id":"10002","name":"Done"}]'::jsonb, '{"id":"10002","name":"Done"}'::jsonb)
		`, teamA)
		return err
	})
	if err != nil {
		t.Fatalf("alice INSERT jira rule: %v", err)
	}
}

// TestRLS_OrgBootstrap — a logged-in user can create a brand-new org
// (where they will be owner), the first team, and their own initial
// membership row, all from within a single tf_app transaction.
// Without an INSERT policy on orgs and bootstrap-aware INSERT policy
// on memberships, the entire signup flow would fail and force
// service_role / supabase_admin code paths.
//
// Pre-bootstrap, the user has no membership anywhere, so claims
// include sub but org_id is unset — the user is "logged in but no
// active tenant context".
func TestRLS_OrgBootstrap(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Seed a fresh auth.users + public.users row but no memberships.
	dave := SeedUser(t, h, "dave")
	alice := SeedUser(t, h, "alice") // for the negative cross-owner test

	// Phase 1: dave creates an org where HE is owner. Each negative
	// assertion gets its own tx so a Postgres tx-abort doesn't
	// invalidate the prior successful INSERT.
	orgID := withDaveTx(t, h, dave, func(tx *sql.Tx) string {
		var id string
		if err := tx.QueryRow(`
			INSERT INTO orgs (slug, name, owner_user_id) VALUES ('dave-org', 'Dave Org', $1) RETURNING id
		`, dave).Scan(&id); err != nil {
			t.Fatalf("dave INSERT org: %v", err)
		}
		return id
	})

	// Phase 2 (negative): dave CANNOT create an org owned by alice.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		_, err := tx.Exec(`
			INSERT INTO orgs (slug, name, owner_user_id) VALUES ('alice-stolen', 'Stolen', $1)
		`, alice)
		if err == nil {
			t.Errorf("dave created an org owned by alice — orgs_insert policy too loose")
		} else {
			assertPgCode(t, err, "42501", "dave cross-owner org INSERT")
		}
		return struct{}{}
	})

	// Phase 3 (negative): dave CANNOT yet create a team — teams_insert
	// requires user_is_org_admin, which queries org_memberships, and
	// dave has no row there yet.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		_, err := tx.Exec(`
			INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'Default')
		`, orgID)
		if err == nil {
			t.Errorf("dave (no org_memberships yet) created a team — admin gate broken")
		}
		return struct{}{}
	})

	// Phase 4: dave self-inserts his org_memberships row as 'owner'.
	// The org_memberships_insert bootstrap branch (tf.user_owns_org)
	// permits this — he founded the org per orgs.owner_user_id.
	withDaveTx(t, h, dave, func(tx *sql.Tx) struct{} {
		if _, err := tx.Exec(`
			INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')
		`, dave, orgID); err != nil {
			t.Fatalf("dave self-insert org_memberships (bootstrap): %v", err)
		}
		return struct{}{}
	})

	// Phase 5: dave is now an org admin (via org_memberships). He can
	// create teams and self-insert team memberships, all through
	// tf_app — no supabase_admin needed.
	var teamID string
	if err := h.WithUser(t, dave, orgID, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`
			INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'Default') RETURNING id
		`, orgID).Scan(&teamID); err != nil {
			return fmt.Errorf("INSERT team: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')
		`, dave, teamID); err != nil {
			return fmt.Errorf("INSERT memberships: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("dave team + memberships bootstrap: %v", err)
	}
}

// withDaveTx runs fn in a fresh tf_app tx with no active org context
// (org_id claim = ""). Used by TestRLS_OrgBootstrap to model the
// post-signup, pre-first-org state where the user is logged in but
// hasn't joined any org yet.
func withDaveTx[T any](t *testing.T, h *Harness, userID string, fn func(tx *sql.Tx) T) T {
	t.Helper()
	ctx := context.Background()
	tx, err := h.AppDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup
	if _, err := tx.Exec(`SET LOCAL ROLE tf_app`); err != nil {
		t.Fatalf("SET LOCAL ROLE: %v", err)
	}
	claims := fmt.Sprintf(`{"sub":"%s","org_id":""}`, userID)
	if _, err := tx.Exec(`SELECT set_config('request.jwt.claims', $1, true)`, claims); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	result := fn(tx)
	// Commit so subsequent calls see what fn wrote (matters for the
	// positive-INSERT branch; negative branches abort the tx via the
	// failed statement and commit silently no-ops).
	_ = tx.Commit()
	return result
}

// TestRLS_MembershipManagement — exercises the four write policies on
// memberships. Admin can add/promote/remove; non-admin can only
// self-leave.
func TestRLS_MembershipManagement(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	carol := SeedUser(t, h, "carol")
	// Bob and carol are org members but not yet on any team. This
	// test asserts the team-membership write policies; the org-
	// membership ones are exercised by TestRLS_OrgBootstrap.
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, bob, orgA)
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, carol, orgA)

	// 1. Alice (team admin / org owner) can add bob to the team.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			bob, teamA,
		)
		return err
	})
	if err != nil {
		t.Fatalf("alice (admin) INSERT bob: %v", err)
	}

	// 2. Bob (now a plain member, not admin) cannot add carol.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			carol, teamA,
		)
		return err
	})
	if err == nil {
		t.Errorf("bob (non-admin) INSERT carol succeeded — admin gate broken")
	}

	// 3. Bob cannot promote himself.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE memberships SET role = 'admin' WHERE user_id = $1 AND team_id = $2`,
			bob, teamA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("bob self-promotion affected %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob self-promotion attempt: %v", err)
	}

	// 4. Bob CAN self-leave (DELETE his own membership).
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM memberships WHERE user_id = $1 AND team_id = $2`,
			bob, teamA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("bob self-leave affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob self-leave: %v", err)
	}
}

// TestFK_CrossOrgRejected pins the composite-FK defense-in-depth.
// Even via AdminDB (RLS bypassed!) a row cannot be inserted that
// FK-references a parent in a different org. This catches bugs in
// app code or compromised internal calls that RLS alone wouldn't.
func TestFK_CrossOrgRejected(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := SeedOrgWithUser(t, h, "alice")
	orgB, _, _ := SeedOrgWithUser(t, h, "bob")

	entityB := seedEntity(t, h, orgB, "github", "octo/repo#1")

	// Try to INSERT a task in orgA referencing entityB (which is in
	// orgB). Composite FK (entity_id, org_id) → entities(id, org_id)
	// rejects: there's no entities row with (entityB, orgA).
	// AdminDB bypasses RLS but FKs are enforced regardless of role.
	// team_id resolved inline so the test trips the entity FK violation
	// (its intent), not the team_id NOT NULL constraint.
	_, err := h.AdminDB.Exec(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1), $3, 'github:pr:opened', gen_random_uuid())
	`, orgA, alice, entityB)
	if err == nil {
		t.Fatalf("cross-org task INSERT succeeded — composite FK broken")
	}
	assertPgCode(t, err, "23503", "cross-org task→entity FK")

	// Same shape: try to INSERT an event in orgA referencing
	// entityB. Composite FK rejects.
	_, err = h.AdminDB.Exec(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened')
	`, orgA, entityB)
	if err == nil {
		t.Fatalf("cross-org event INSERT succeeded — composite FK broken")
	}
}

// TestRLS_ChildTablesInheritParentVisibility — denormalized child
// rows (task_events, messages, conversation_memory, conversation_memory_entities,
// conversation_worktrees, pending_firings) must NOT be visible to org members
// who can't see the parent task/conversation. Earlier policies gated only on
// org_id, leaking metadata across users in the same org. EXISTS-on-parent
// inherits the parent table's RLS. (artifacts is excluded: it scopes
// directly on team_id like conversations, not via EXISTS-on-conversation,
// so it has its own team-visibility coverage in artifacts_test.go.)
func TestRLS_ChildTablesInheritParentVisibility(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// Alice creates a task + a conversation + child rows. All in orgA. The
	// team-default would make these rows visible to bob too
	// (same team), which would hide the "child inherits parent
	// visibility" property this test pins. Pin the parents at
	// visibility='private' so the inheritance check is meaningful: alice
	// sees her own private rows; bob (different creator) can't.
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	prompt := seedPrompt(t, h, orgA, alice, "p1")
	var evtID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, orgA, entityA).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var taskID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, visibility, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, 'private', $4, 'github:pr:opened', $5) RETURNING id
	`, orgA, alice, teamA, entityA, evtID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	bpRun := seedBlueprintRun(t, h, orgA, alice, taskID)
	var conversationID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO conversations (org_id, creator_user_id, team_id, visibility, task_id, prompt_id, blueprint_run_id, status)
		VALUES ($1, $2, $3, 'private', $4, $5, $6, 'running') RETURNING id
	`, orgA, alice, teamA, taskID, prompt, bpRun).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// Seed one child row per parent kind we care about.
	MustExec(t, h.AdminDB, `INSERT INTO task_events (org_id, task_id, event_id, kind)
		SELECT $1, $2, e.id, 'closed' FROM events e WHERE e.entity_id = $3 LIMIT 1`,
		orgA, taskID, entityA)
	MustExec(t, h.AdminDB, `INSERT INTO messages (org_id, conversation_id, role, content) VALUES ($1, $2, 'assistant', 'hi')`,
		orgA, conversationID)
	MustExec(t, h.AdminDB, `INSERT INTO conversation_memory (org_id, conversation_id, entity_id, agent_content) VALUES ($1, $2, $3, 'note')`,
		orgA, conversationID, entityA)
	MustExec(t, h.AdminDB, `INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role) VALUES ($1, $2, $3, 'primary')`,
		orgA, conversationID, entityA)
	MustExec(t, h.AdminDB, `INSERT INTO conversation_worktrees (org_id, conversation_id, repository_id, path, ref) VALUES ($1, $2, $3, '/tmp/x', 'pr-1')`,
		orgA, conversationID, SeedRepository(t, h, orgA, "octo", "repo"))

	// Alice sees all her child rows.
	err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		for _, table := range []string{"task_events", "messages", "conversation_memory", "conversation_memory_entities", "conversation_worktrees"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("alice saw %d %s rows, want 1", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice query: %v", err)
	}

	// Bob (same org, NOT the conversation/task creator) must see ZERO of these
	// child rows — tasks_select and conversations_select gate on creator, so
	// the EXISTS-on-parent in each child policy returns false for him.
	err = h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		for _, table := range []string{"task_events", "messages", "conversation_memory", "conversation_memory_entities", "conversation_worktrees"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("bob saw %d %s rows, want 0 — child policy leaked across users", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bob query: %v", err)
	}
}

// TestGooseDBVersionLockdown — tf_app must not have any privileges
// on the goose migration tracking table. The bulk
// `GRANT ... ON ALL TABLES IN SCHEMA public TO tf_app` accidentally
// covered goose_db_version (created in public by goose itself); the
// migration's REVOKE locks it back down so the application role can't
// fake migration state.
func TestGooseDBVersionLockdown(t *testing.T) {
	h := Shared(t)

	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var has bool
		if err := h.AdminDB.QueryRow(
			`SELECT has_table_privilege('tf_app', 'public.goose_db_version', $1)`, priv,
		).Scan(&has); err != nil {
			t.Fatalf("has_table_privilege(tf_app, goose_db_version, %s): %v", priv, err)
		}
		if has {
			t.Errorf("tf_app has %s on goose_db_version — migration state tampering vector", priv)
		}
	}
	// Sequence too.
	var seqHas bool
	if err := h.AdminDB.QueryRow(
		`SELECT has_sequence_privilege('tf_app', 'public.goose_db_version_id_seq', 'USAGE')`,
	).Scan(&seqHas); err != nil {
		t.Fatalf("has_sequence_privilege: %v", err)
	}
	if seqHas {
		t.Errorf("tf_app has USAGE on goose_db_version_id_seq")
	}
}

// TestOrgOwnership_LastOwnerProtected — the trigger refuses to leave
// an org without any owner. Single-statement transfers (promote new
// in same UPDATE that demotes old) work; deleting/demoting the last
// owner does not.
func TestOrgOwnership_LastOwnerProtected(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := SeedOrgWithUser(t, h, "alice")

	// Demoting the sole owner must fail.
	_, err := h.AdminDB.Exec(
		`UPDATE org_memberships SET role = 'member' WHERE user_id = $1 AND org_id = $2`,
		alice, orgA,
	)
	if err == nil {
		t.Errorf("demoting sole owner succeeded — invariant broken")
	} else {
		// 23514 = check_violation (our trigger raises this).
		assertPgCode(t, err, "23514", "demote sole owner")
	}

	// Deleting the sole owner must fail.
	_, err = h.AdminDB.Exec(
		`DELETE FROM org_memberships WHERE user_id = $1 AND org_id = $2`,
		alice, orgA,
	)
	if err == nil {
		t.Errorf("deleting sole owner succeeded — invariant broken")
	}

	// Add a second owner; THEN demoting alice works (transfer flow).
	bob := SeedUser(t, h, "bob")
	MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`, bob, orgA)
	if _, err := h.AdminDB.Exec(
		`UPDATE org_memberships SET role = 'member' WHERE user_id = $1 AND org_id = $2`,
		alice, orgA,
	); err != nil {
		t.Errorf("demoting after second owner added failed: %v", err)
	}
}

// TestOrgOwnership_OnlyOwnerCanTransfer — orgs.owner_user_id can only
// be changed by the current owner. Closes the gap where any
// user_is_org_admin (which now includes role='admin', not just
// 'owner') could rewrite ownership and take the org over.
func TestOrgOwnership_OnlyOwnerCanTransfer(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	// Bob is an org-level admin (not owner) and a team admin.
	AddOrgMember(t, h, bob, orgA, teamA, "admin", "admin")

	// Bob tries to take over (set himself as owner_user_id). Trigger
	// refuses — only the current owner can transfer.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE orgs SET owner_user_id = $1 WHERE id = $2`, bob, orgA)
		return err
	})
	if err == nil {
		t.Fatalf("org admin (not owner) transferred ownership — privilege escalation")
	}
	// 42501 = insufficient_privilege (our trigger raises this).
	assertPgCode(t, err, "42501", "non-owner ownership transfer")

	// Alice (the actual owner) needs bob to already have 'owner'
	// role in org_memberships before transferring. Without that,
	// the trigger refuses.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE orgs SET owner_user_id = $1 WHERE id = $2`, bob, orgA)
		return err
	})
	if err == nil {
		t.Errorf("transfer to non-owner-role user succeeded — invariant broken")
	}

	// Promote bob to org_memberships owner, then transfer works.
	MustExec(t, h.AdminDB,
		`UPDATE org_memberships SET role = 'owner' WHERE user_id = $1 AND org_id = $2`,
		bob, orgA)
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE orgs SET owner_user_id = $1 WHERE id = $2`, bob, orgA)
		return err
	})
	if err != nil {
		t.Errorf("legitimate ownership transfer (alice→bob, both org_memberships=owner) failed: %v", err)
	}
}

// TestPrompts_TeamIDRequired — every prompt is team-owned (the
// visibility column was dropped; team_id is the sole scoping signal).
// A prompt with a NULL team_id is an invalid state the NOT NULL
// constraint refuses (SQLSTATE 23502), replacing the old
// prompts_team_visibility_requires_team CHECK.
func TestPrompts_TeamIDRequired(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, _ := SeedOrgWithUser(t, h, "alice")

	_, err := h.AdminDB.Exec(`
		INSERT INTO prompts (org_id, creator_user_id, name, body)
		VALUES ($1, $2, 'orphan-prompt', '')
	`, orgA, alice)
	if err == nil {
		t.Fatalf("prompts INSERT with team_id=NULL succeeded — NOT NULL constraint missing")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected PostgreSQL error, got: %T %v", err, err)
	}
	if pgErr.Code != "23502" {
		t.Fatalf("expected SQLSTATE 23502 (not_null_violation), got %q: %v", pgErr.Code, err)
	}
	if pgErr.ColumnName != "team_id" {
		t.Errorf("expected not-null violation on column team_id, got %q", pgErr.ColumnName)
	}
}

// TestUpdatedAtAutoBump — every table with an updated_at column gets
// the timestamp bumped on UPDATE without the app having to set it.
func TestUpdatedAtAutoBump(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, _ := SeedOrgWithUser(t, h, "alice")

	var before time.Time
	if err := h.AdminDB.QueryRow(
		`SELECT updated_at FROM orgs WHERE id = $1`, orgA,
	).Scan(&before); err != nil {
		t.Fatalf("read initial updated_at: %v", err)
	}

	// Sleep a moment so the bump is observable in TIMESTAMPTZ resolution.
	time.Sleep(10 * time.Millisecond)

	if _, err := h.AdminDB.Exec(
		`UPDATE orgs SET description = 'new desc' WHERE id = $1`, orgA,
	); err != nil {
		t.Fatalf("UPDATE orgs: %v", err)
	}

	var after time.Time
	if err := h.AdminDB.QueryRow(
		`SELECT updated_at FROM orgs WHERE id = $1`, orgA,
	).Scan(&after); err != nil {
		t.Fatalf("read post updated_at: %v", err)
	}
	if !after.After(before) {
		t.Errorf("updated_at not bumped on UPDATE: before=%v after=%v", before, after)
	}
}

// TestPrompts_SemanticIDsAccepted — system prompts use stable
// semantic IDs ("system-pr-review", etc.) that the application
// references by name. prompts.id is TEXT (not UUID) so those
// INSERTs work. User-generated prompts get gen_random_uuid()::text
// by default; both shapes coexist in the same table.
func TestPrompts_SemanticIDsAccepted(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")

	// System prompt with a semantic ID. creator_user_id is NULL per the
	// prompts_system_has_no_creator CHECK — shipped rows have no human
	// author. team_id required (NOT NULL, no visibility column).
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, source, name, body)
		VALUES ('system-pr-review', $1, NULL, $2, 'system', 'PR Review', '...')
	`, orgA, teamA); err != nil {
		t.Fatalf("system prompt INSERT with semantic id: %v", err)
	}
	_ = alice // user prompt INSERT below still uses alice as creator

	// User prompt picks up the default (UUID-shaped string). team_id
	// resolved inline from the org's first team.
	var userPromptID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body)
		VALUES ($1, $2, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1), 'My Prompt', 'hello') RETURNING id
	`, orgA, alice).Scan(&userPromptID); err != nil {
		t.Fatalf("user prompt INSERT: %v", err)
	}
	if len(userPromptID) != 36 {
		t.Errorf("user prompt id = %q (len %d), want UUID-shaped (36 chars)", userPromptID, len(userPromptID))
	}

	// Both coexist.
	var n int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM prompts WHERE org_id = $1`, orgA).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("got %d prompts, want 2", n)
	}
}

// TestSearchPathHardening_AllSensitiveFunctions — structural
// assertion that every SECURITY DEFINER function and every trigger
// function has the canonical hardened search_path:
//
//	pg_catalog explicit first (defense against shadowed built-ins),
//	public second (where org_memberships et al. live),
//	NO pg_temp (would let an attacker poison resolution by creating
//	  a temp object that shadows a referenced table/operator —
//	  CVE-2018-1058 class).
//
// Probes pg_proc.proconfig directly, so the test fails fast if a
// future migration adds a SECURITY DEFINER without the right
// hardening or quietly re-introduces pg_temp.
func TestSearchPathHardening_AllSensitiveFunctions(t *testing.T) {
	h := Shared(t)

	// Functions we care about: every SECURITY DEFINER and every
	// trigger function we own. (tf.current_user_id /
	// tf.current_org_id are LANGUAGE SQL STABLE and reference only
	// pg_catalog primitives — they're safe without explicit
	// search_path, so they're not in this list.)
	hardened := []struct {
		schema, name string
	}{
		{"tf", "user_has_org_access"},
		{"tf", "user_is_org_admin"},
		{"tf", "user_is_team_admin"},
		{"tf", "user_owns_org"},
		{"tf", "user_is_org_admin_via_team"},
		{"tf", "set_updated_at"},
		{"tf", "guard_org_owners"},
		{"tf", "guard_org_owner_transfer"},
	}
	for _, fn := range hardened {
		// array_to_string collapses the text[] into a single scalar
		// so we can Scan it directly without pulling in a Postgres
		// array adapter. Each entry in proconfig is "KEY=VALUE";
		// joining with '\n' makes parsing trivial.
		var config sql.NullString
		err := h.AdminDB.QueryRow(`
			SELECT array_to_string(p.proconfig, E'\n')
			  FROM pg_proc p JOIN pg_namespace n ON p.pronamespace = n.oid
			 WHERE n.nspname = $1 AND p.proname = $2
		`, fn.schema, fn.name).Scan(&config)
		if err != nil {
			t.Errorf("%s.%s: probe proconfig: %v", fn.schema, fn.name, err)
			continue
		}
		var searchPath string
		for _, line := range strings.Split(config.String, "\n") {
			if strings.HasPrefix(line, "search_path=") {
				searchPath = strings.TrimPrefix(line, "search_path=")
				break
			}
		}
		if searchPath == "" {
			t.Errorf("%s.%s: no SET search_path — vulnerable to search_path hijack", fn.schema, fn.name)
			continue
		}
		// pg_proc stores the value verbatim. Postgres normalizes
		// some forms (quoted vs unquoted identifiers) — accept either.
		if searchPath != "pg_catalog, public" && searchPath != "\"pg_catalog\", \"public\"" {
			t.Errorf("%s.%s: search_path = %q, want %q", fn.schema, fn.name, searchPath, "pg_catalog, public")
		}
		if strings.Contains(searchPath, "pg_temp") {
			t.Errorf("%s.%s: search_path contains pg_temp (%q) — hijack vector", fn.schema, fn.name, searchPath)
		}
	}
}

// TestRLS_TeamAdminNotOrgAdmin pins the two-axis role model: a team
// admin who is only an org member (not an org admin) can manage their
// own team but CANNOT mutate org-wide attributes. This is the
// scenario the previous one-axis model silently allowed — anyone
// promoted to admin of any subteam got org-wide write access.
func TestRLS_TeamAdminNotOrgAdmin(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Alice founds the org. Carol is added as a regular org member,
	// then made admin of a subteam (mobile-team).
	orgA, alice, teamMain := SeedOrgWithUser(t, h, "alice")
	carol := SeedUser(t, h, "carol")
	teamMobile := SeedTeam(t, h, orgA, "mobile-team")
	AddOrgMember(t, h, carol, orgA, teamMobile, "member", "admin")
	_ = teamMain // unused; alice's team

	// Carol CAN manage her team's settings.
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO team_settings (team_id, jira_projects)
			VALUES ($1, ARRAY['SKY','MOB'])
		`, teamMobile)
		return err
	})
	if err != nil {
		t.Errorf("carol (team admin) INSERT team_settings on her own team: %v", err)
	}

	// Carol CANNOT rename the org (org admin only).
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'carol-takeover' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("team-admin-only carol UPDATE'd orgs.name (%d rows) — two-axis broken", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol rename attempt: %v", err)
	}

	// Carol CANNOT write org_settings.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO org_settings (org_id) VALUES ($1)`, orgA)
		return err
	})
	if err != nil {
		t.Fatalf("alice seed org_settings: %v", err)
	}
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE org_settings SET github_clone_protocol = 'https' WHERE org_id = $1`, orgA,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("team-admin-only carol UPDATE'd org_settings (%d rows)", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol org_settings attempt: %v", err)
	}

	// Carol CANNOT create a new team in the org.
	err = h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO teams (org_id, slug, name) VALUES ($1, 'sneaky', 'Sneaky')`, orgA,
		)
		return err
	})
	if err == nil {
		t.Errorf("team-admin-only carol created a new team — should require org admin")
	}

	// Sanity: alice (org owner) can do all of the above.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE orgs SET name = 'renamed-by-alice' WHERE id = $1`, orgA)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			t.Errorf("alice (org owner) rename affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice sanity rename: %v", err)
	}
}

// --- fixture helpers ---
//
// SeedUser, SeedOrg, SeedTeam, SeedOrgWithUser, AddOrgMember, and
// MustExec live in seed.go (exported so postgres_test can call them).

func seedPrompt(t *testing.T, h *Harness, orgID, creatorID, name string) string {
	t.Helper()
	var id string
	// team_id resolved inline from the org's first team (team-default).
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body)
		VALUES ($1, $2, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1), $3, '') RETURNING id
	`, orgID, creatorID, name).Scan(&id); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

func seedBlueprint(t *testing.T, h *Harness, orgID, creatorID, name string) string {
	t.Helper()
	var id string
	// team_id resolved inline from the org's first team (team-default).
	if err := h.AdminDB.QueryRow(`
		INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1), $3, 'user') RETURNING id
	`, orgID, creatorID, name).Scan(&id); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	return id
}

// seedBlueprintRun mints a blueprint + blueprint_run for taskID so a
// conversations row can reference blueprint_runs(id). The column is
// nullable, but the conversations_origin_requires_parents CHECK requires it
// (plus task_id/prompt_id) to be set when origin='blueprint' (the default).
// Returns the blueprint_run id. Admin-pool insert (creator routing isn't
// under test here).
func seedBlueprintRun(t *testing.T, h *Harness, orgID, creatorID, taskID string) string {
	t.Helper()
	bpID := seedBlueprint(t, h, orgID, creatorID, "bp-"+taskID[:8])
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO blueprint_runs (org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, 'manual', 'running', '/tmp/wt', '[]') RETURNING id
	`, orgID, creatorID, bpID, taskID).Scan(&id); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return id
}

func seedEntity(t *testing.T, h *Harness, orgID, source, sourceID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO entities (org_id, source, source_id, kind, title)
		VALUES ($1, $2, $3, 'pr', 'test pr') RETURNING id
	`, orgID, source, sourceID).Scan(&id); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}

func seedTask(t *testing.T, h *Harness, orgID, creatorID, entityID, eventType string) string {
	t.Helper()
	// Insert a corresponding event first since tasks.primary_event_id is NOT NULL.
	var evtID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, $3) RETURNING id
	`, orgID, entityID, eventType).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var id string
	// team_id resolved inline from the org's first team (NOT NULL
	// on tasks). visibility defaults to 'team' from the column.
	if err := h.AdminDB.QueryRow(`
		INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, (SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1), $3, $4, $5) RETURNING id
	`, orgID, creatorID, entityID, eventType, evtID).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

func getOrgTeam(t *testing.T, h *Harness, orgID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM teams WHERE org_id = $1 LIMIT 1`, orgID,
	).Scan(&id); err != nil {
		t.Fatalf("get team for org %s: %v", orgID, err)
	}
	return id
}

func getEventForEntity(t *testing.T, h *Harness, entityID string) string {
	t.Helper()
	var id string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM events WHERE entity_id = $1 LIMIT 1`, entityID,
	).Scan(&id); err != nil {
		t.Fatalf("get event for entity %s: %v", entityID, err)
	}
	return id
}

// TestRLS_BaselineCrossOrgPin_MultiOrgUser pins the fix from migration
// 202605120007. Four cases for a user (Charlie) with admin memberships
// in two orgs, exercising the realistic shape "I'm operating in orgB
// right now but I have privileges in orgA too":
//
//  1. users_select — Charlie in orgA context queries users; can see
//     himself + Alice (orgA owner) but NOT Bob (orgB-only).
//  2. memberships_insert — Charlie's session is on orgB but he tries
//     to INSERT into teamA (which is in orgA). Pre-fix this would
//     succeed because the org-blind helper saw him as team-admin of
//     teamA. With the pin, the team_in_current_org check refuses.
//  3. org_memberships_insert — Charlie's session is on orgB but he
//     tries to add a new user to orgA's org_memberships. Pre-fix this
//     would succeed because user_is_org_admin(orgA) passed. With the
//     pin, org_id = tf.current_org_id() check refuses.
//  4. team_settings_select — Charlie in orgA cannot read orgB's
//     team_settings, only orgA's (where he's a member).
//
// Pre-202605120007 every gate passed because the helpers (user_is_team_admin,
// user_is_org_admin_via_team, user_is_org_admin) check membership/role
// in the TARGET row's org, not the active session's. The new pin via
// tf.team_in_current_org / org_id = tf.current_org_id() refuses these.
func TestRLS_BaselineCrossOrgPin_MultiOrgUser(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, aliceID, teamA := SeedOrgWithUser(t, h, "alice")
	orgB, bobID, teamB := SeedOrgWithUser(t, h, "bob")

	// Charlie: org admin in both orgA and orgB; team admin in both teams.
	charlieID := SeedUser(t, h, "charlie")
	for _, orgID := range []string{orgA, orgB} {
		MustExec(t, h.AdminDB,
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'admin')`,
			charlieID, orgID)
	}
	for _, teamID := range []string{teamA, teamB} {
		MustExec(t, h.AdminDB,
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
			charlieID, teamID)
	}
	MustExec(t, h.AdminDB,
		`INSERT INTO team_settings (team_id) VALUES ($1)`, teamA)
	MustExec(t, h.AdminDB,
		`INSERT INTO team_settings (team_id) VALUES ($1)`, teamB)

	// (1) users_select pinned to current_org_id.
	if err := h.WithUser(t, charlieID, orgA, func(tx *sql.Tx) error {
		ids := map[string]bool{}
		rows, err := tx.Query(`SELECT id FROM users`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids[id] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !ids[charlieID] {
			return fmt.Errorf("charlie not in own user list")
		}
		if !ids[aliceID] {
			return fmt.Errorf("alice (orgA owner) missing — current_org pin should still allow same-org users")
		}
		if ids[bobID] {
			return fmt.Errorf("bob (orgB-only) visible from orgA context — cross-org users_select leak")
		}
		return nil
	}); err != nil {
		t.Fatalf("users_select pin: %v", err)
	}

	// (2) memberships_insert refuses cross-org team writes.
	// Charlie's session claims org_id = orgB. He attempts to INSERT a
	// memberships row that points at teamA (in orgA). Pre-fix this
	// would pass because the user_is_team_admin helper sees him as
	// admin of teamA regardless of which org his JWT claims. With
	// the pin, tf.team_in_current_org(teamA) returns false because
	// teamA.org_id (orgA) != tf.current_org_id() (orgB). Exec error
	// propagates up through WithUser as the closure's return value
	// (returning nil from the closure after a failed Exec would try
	// to Commit on an already-aborted tx and obscure the real error).
	err := h.WithUser(t, charlieID, orgB, func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
			bobID, teamA,
		)
		return e
	})
	if err == nil {
		t.Error("memberships INSERT into teamA from orgB context succeeded — cross-org write leak")
	} else {
		assertPgCode(t, err, "42501", "memberships cross-org INSERT")
	}

	// (3) org_memberships_insert refuses cross-org admin writes.
	// Charlie's session claims org_id = orgB. He attempts to INSERT
	// into orgA's org_memberships. Pre-fix this would pass because
	// user_is_org_admin(orgA) returns true regardless of current_org_id.
	// With the pin (org_id = tf.current_org_id()), refused.
	someoneID := SeedUser(t, h, "newbie")
	err = h.WithUser(t, charlieID, orgB, func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
			someoneID, orgA,
		)
		return e
	})
	if err == nil {
		t.Error("org_memberships INSERT into orgA from orgB context succeeded — cross-org admin write leak")
	} else {
		assertPgCode(t, err, "42501", "org_memberships cross-org INSERT")
	}

	// (4) team_settings cross-org SELECT pinned. Charlie in orgA reads
	// team_settings; sees teamA's row but not teamB's.
	if err := h.WithUser(t, charlieID, orgA, func(tx *sql.Tx) error {
		teams := map[string]bool{}
		rows, err := tx.Query(`SELECT team_id FROM team_settings`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tid string
			if err := rows.Scan(&tid); err != nil {
				return err
			}
			teams[tid] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !teams[teamA] {
			return fmt.Errorf("teamA settings not visible in orgA context")
		}
		if teams[teamB] {
			return fmt.Errorf("teamB settings visible in orgA context — cross-org team_settings_select leak")
		}
		return nil
	}); err != nil {
		t.Fatalf("team_settings pin: %v", err)
	}
}

// TestRLS_OrgMembershipsBootstrapStillWorks pins that the org_memberships
// founder self-insert path survives the 202605120007 tightening. The
// bootstrap branch on org_memberships_insert is
//
//	(user_id = tf.current_user_id() AND tf.user_owns_org(...))
//
// which depends only on the caller's identity + ownership and
// intentionally does NOT require current_org_id matching. That's the
// safety net for two realistic JWT shapes the auth flow produces during
// first-signup:
//
//  1. Claims already re-issued with the new org_id (matching-claims).
//  2. Claims not yet re-issued — tf.current_org_id() returns NULL via
//     the 202605120002 GUC hardening short-circuit. The bootstrap
//     branch doesn't reference current_org_id and so still succeeds.
//
// Both shapes get a separate assertion here because both are realistic
// and we want the policy to stay tolerant of either.
func TestRLS_OrgMembershipsBootstrapStillWorks(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Shape (1): claims re-issued with new org_id.
	founder1ID := SeedUser(t, h, "founder1")
	var org1ID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ('founder-org-1', 'Founder Org 1', $1) RETURNING id
	`, founder1ID).Scan(&org1ID); err != nil {
		t.Fatalf("seed org1: %v", err)
	}
	if err := h.WithUser(t, founder1ID, org1ID, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
			founder1ID, org1ID,
		)
		return err
	}); err != nil {
		t.Fatalf("founder bootstrap (matching-claims shape): %v", err)
	}

	// Shape (2): claims not yet re-issued. WithUser still marshals
	// {"sub": ..., "org_id": ""}, and the 202605120002 hardening of
	// tf.current_org_id() short-circuits the empty string to NULL.
	// That matches the realistic no-org-claim shape during first
	// signup before the auth flow has issued an org cookie.
	founder2ID := SeedUser(t, h, "founder2")
	var org2ID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ('founder-org-2', 'Founder Org 2', $1) RETURNING id
	`, founder2ID).Scan(&org2ID); err != nil {
		t.Fatalf("seed org2: %v", err)
	}
	if err := h.WithUser(t, founder2ID, "", func(tx *sql.Tx) error {
		// Sanity probe: confirm tf.current_org_id() actually returns
		// NULL when org_id claim is empty string (not '' UUID). If the
		// helper ever stops short-circuiting, this surfaces here rather
		// than as a confusing policy failure below.
		var orgIDNull bool
		if err := tx.QueryRow(`SELECT tf.current_org_id() IS NULL`).Scan(&orgIDNull); err != nil {
			return fmt.Errorf("probe current_org_id: %w", err)
		}
		if !orgIDNull {
			return fmt.Errorf("tf.current_org_id() with empty org_id claim is not NULL — 202605120002 hardening regressed")
		}
		_, err := tx.Exec(
			`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
			founder2ID, org2ID,
		)
		return err
	}); err != nil {
		t.Fatalf("founder bootstrap (no-org-claim shape): %v", err)
	}
}

// TestRLS_TeamMembershipWithoutOrgAccessDenied pins the defense-in-depth
// guard: tf.user_in_team(team_id) only checks the
// memberships table, so a user with a memberships row pointing at a
// team in orgA but NO org_memberships row in orgA must NOT be able to
// SELECT/UPDATE/DELETE team-visible rows in orgA. The outer
// tf.user_has_org_access(org_id) guard on every team-branch policy
// catches this case.
//
// Realistic scenarios where this could matter:
//   - Stale state: an org_memberships row was deleted but the
//     corresponding memberships rows weren't cascaded.
//   - Privilege confusion: code path that adds a memberships row
//     without going through the canonical AddOrgMember flow.
//   - Attacker-controlled team_id: someone discovers a team_id in
//     orgA and tries to use a memberships row to read it.
//
// In all cases the policies must deny. This test fabricates the state
// directly via AdminDB (bypassing the AddOrgMember helper that pairs
// the two rows) and verifies the deny path on every swept table.
func TestRLS_TeamMembershipWithoutOrgAccessDenied(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")

	// Mallory has a memberships row for teamA (orgA's team) but no
	// org_memberships row in orgA. Constructed with raw INSERTs so we
	// bypass the AddOrgMember helper that pairs the two rows.
	mallory := SeedUser(t, h, "mallory")
	MustExec(t, h.AdminDB,
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`, mallory, teamA)
	// NB: intentionally NO INSERT INTO org_memberships for mallory in orgA.

	// Alice creates team-visible rows in every swept table — these are
	// the rows mallory's stolen team-membership might let her reach.
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#1")
	taskID := seedTask(t, h, orgA, alice, entityA, "github:pr:opened")
	promptID := seedPrompt(t, h, orgA, alice, "p1")
	bpRun := seedBlueprintRun(t, h, orgA, alice, taskID)
	var conversationID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO conversations (org_id, creator_user_id, team_id, task_id, prompt_id, blueprint_run_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'running') RETURNING id
	`, orgA, alice, teamA, taskID, promptID, bpRun).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	var ehID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO event_handlers (org_id, creator_user_id, team_id, kind, event_type, name, default_priority, sort_order)
		VALUES ($1, $2, $3, 'rule', 'github:pr:opened', 'r1', 0.5, 0) RETURNING id
	`, orgA, alice, teamA).Scan(&ehID); err != nil {
		t.Fatalf("seed event_handler: %v", err)
	}

	// Drive every read/write under mallory's claims pointing at orgA.
	// Every policy must deny because tf.user_has_org_access(orgA) is
	// FALSE — she has no org_memberships row. The team-branch's
	// tf.user_in_team(teamA) WOULD return TRUE (memberships exists),
	// but the outer guard short-circuits before that even matters.
	err := h.WithUser(t, mallory, orgA, func(tx *sql.Tx) error {
		// SELECT — mallory must see ZERO rows on every swept table.
		for _, tbl := range []string{"tasks", "conversations", "prompts", "event_handlers"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
				return fmt.Errorf("count %s: %w", tbl, err)
			}
			if n != 0 {
				t.Errorf("mallory saw %d %s rows despite no org membership", n, tbl)
			}
		}

		// UPDATE — must affect zero rows. Targeting the specific row
		// IDs we know exist; RLS makes the row invisible to mallory,
		// so the UPDATE matches nothing.
		updates := map[string]string{
			"tasks":          `UPDATE tasks          SET status        = 'pwned' WHERE id = $1`,
			"conversations":  `UPDATE conversations  SET park_reason   = 'pwned' WHERE id = $1`,
			"prompts":        `UPDATE prompts        SET body          = 'pwned' WHERE id = $1`,
			"event_handlers": `UPDATE event_handlers SET name          = 'pwned' WHERE id = $1`,
		}
		ids := map[string]string{
			"tasks":          taskID,
			"conversations":  conversationID,
			"prompts":        promptID,
			"event_handlers": ehID,
		}
		for tbl, stmt := range updates {
			res, err := tx.Exec(stmt, ids[tbl])
			if err != nil {
				return fmt.Errorf("update %s: %w", tbl, err)
			}
			n, _ := res.RowsAffected()
			if n != 0 {
				t.Errorf("mallory updated %d %s rows despite no org membership", n, tbl)
			}
		}

		// DELETE — same deny path.
		for tbl, id := range ids {
			res, err := tx.Exec(`DELETE FROM `+tbl+` WHERE id = $1`, id)
			if err != nil {
				return fmt.Errorf("delete %s: %w", tbl, err)
			}
			n, _ := res.RowsAffected()
			if n != 0 {
				t.Errorf("mallory deleted %d %s rows despite no org membership", n, tbl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mallory session: %v", err)
	}

	// Sanity: alice (real org owner + team admin) can still read all
	// the rows we just protected from mallory. Pins that the defense
	// doesn't over-deny.
	err = h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		for _, tbl := range []string{"tasks", "conversations", "prompts", "event_handlers"} {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
				return fmt.Errorf("count %s: %w", tbl, err)
			}
			if n == 0 {
				t.Errorf("alice (owner) saw 0 %s rows — defense over-denied", tbl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("alice sanity: %v", err)
	}
}

// TestRLS_NonAdminCannotInsertOrgVisible pins the second defense:
// non-admin org members must not be able to INSERT rows
// with visibility='org' on any of the five swept tables. The system-
// driven seed paths use the admin pool (BYPASSRLS) for shipped
// visibility='org' rows; this policy guards user-path callsites.
//
// Without the per-visibility admin gate on the WITH CHECK, any org
// member could fabricate org-visible rows that look like sanctioned
// admin-managed defaults — e.g., a "shipped" prompt or rule that
// claims org-wide authority but was actually inserted by a regular
// user. The admin gate matches the UPDATE policy shape.
func TestRLS_NonAdminCannotInsertOrgVisible(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	carol := SeedUser(t, h, "carol")
	AddOrgMember(t, h, carol, orgA, teamA, "member", "member") // org member but not admin

	// Seed an entity + event + parent task + prompt so the task/conversations
	// INSERTs below have parents to reference (created via AdminDB so
	// the seeds themselves bypass the policy we're testing).
	entityA := seedEntity(t, h, orgA, "github", "octo/repo#org-insert")
	var evtID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
	`, orgA, entityA).Scan(&evtID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// Parent task + prompt + blueprint_run for the conversations INSERT case below.
	// blueprint_run_id must be set so the conversations row clears the
	// conversations_origin_requires_parents CHECK (origin defaults to 'blueprint');
	// otherwise the INSERT would fail at the CHECK level and never exercise
	// the RLS admin gate this test is asserting.
	parentTaskID := seedTask(t, h, orgA, alice, entityA, "github:pr:opened")
	parentPromptID := seedPrompt(t, h, orgA, alice, "p-org-insert")
	parentBPRunID := seedBlueprintRun(t, h, orgA, alice, parentTaskID)

	// Each INSERT below would succeed without the admin gate (carol IS
	// an org member, IS on teamA). The gate adds:
	//   (visibility <> 'org' OR tf.user_is_org_admin(org_id))
	// so org-visible rows from a non-admin must be rejected.
	//
	// Postgres aborts the whole transaction on a CHECK violation, so
	// each attempt runs in its own SAVEPOINT — rollback to the
	// savepoint after a violation, then continue with the next
	// attempt. Without this scaffolding the second attempt would
	// fail with "current transaction is aborted" and mask the actual
	// policy behavior.
	cases := []struct {
		label string
		stmt  string
		args  []any
	}{
		{
			label: "tasks",
			stmt: `INSERT INTO tasks (org_id, creator_user_id, team_id, visibility, entity_id, event_type, primary_event_id)
				VALUES ($1, $2, $3, 'org', $4, 'github:pr:opened', $5)`,
			args: []any{orgA, carol, teamA, entityA, evtID},
		},
		{
			label: "conversations",
			stmt: `INSERT INTO conversations (org_id, creator_user_id, team_id, visibility, task_id, prompt_id, blueprint_run_id, status)
				VALUES ($1, $2, $3, 'org', $4, $5, $6, 'running')`,
			args: []any{orgA, carol, teamA, parentTaskID, parentPromptID, parentBPRunID},
		},
		{
			label: "prompts",
			stmt: `INSERT INTO prompts (org_id, creator_user_id, visibility, name, body)
				VALUES ($1, $2, 'org', 'evil-org-prompt', '')`,
			args: []any{orgA, carol},
		},
	}

	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		for _, c := range cases {
			if _, err := tx.Exec(`SAVEPOINT sp_` + c.label); err != nil {
				return fmt.Errorf("savepoint %s: %w", c.label, err)
			}
			_, err := tx.Exec(c.stmt, c.args...)
			if err == nil {
				t.Errorf("non-admin carol INSERT'd visibility='org' %s — admin gate missing", c.label)
				if _, rbErr := tx.Exec(`RELEASE SAVEPOINT sp_` + c.label); rbErr != nil {
					return fmt.Errorf("release savepoint %s: %w", c.label, rbErr)
				}
				continue
			}
			if _, rbErr := tx.Exec(`ROLLBACK TO SAVEPOINT sp_` + c.label); rbErr != nil {
				return fmt.Errorf("rollback savepoint %s: %w", c.label, rbErr)
			}
		}

		// Sanity: a team-owned prompt INSERT still succeeds for carol
		// (she's on the team) — the dropped per-table org gate doesn't
		// over-deny the legitimate team path. prompts has no visibility
		// column; team membership alone governs it.
		if _, err := tx.Exec(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body)
			VALUES ($1, $2, $3, 'carol-team-prompt', '')
		`, orgA, carol, teamA); err != nil {
			t.Errorf("non-admin carol team INSERT denied — defense over-broad: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carol session: %v", err)
	}
}

// TestRLS_TasksClaimXorRejection pins the D-Claims tasks_claim_xor
// CHECK: at most one of (claimed_by_agent_id, claimed_by_user_id) can be
// set on a row. Both NULL is the unclaimed-in-queue state; either one set
// is the current-claimant state; both set is forbidden.
//
// This is the schema-level invariant the claim-flip helpers
// (SetClaimedByAgent / SetClaimedByUser) rely on: each does a
// single UPDATE that sets one column AND clears the other in the same
// statement, so the XOR is never temporarily violated. A direct SQL
// attempt to set both at once must be rejected.
func TestRLS_TasksClaimXorRejection(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	// agents row is required so the FK on tasks.claimed_by_agent_id
	// resolves. SeedOrgWithUser doesn't auto-create one.
	if _, err := h.AdminDB.Exec(`
		INSERT INTO agents (id, org_id, display_name)
		VALUES (gen_random_uuid(), $1, 'Test Bot')
	`, orgA); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	var agentID string
	if err := h.AdminDB.QueryRow(
		`SELECT id FROM agents WHERE org_id = $1`, orgA,
	).Scan(&agentID); err != nil {
		t.Fatalf("read agent id: %v", err)
	}

	entityA := seedEntity(t, h, orgA, "github", "octo/repo#xor")
	taskID := seedTask(t, h, orgA, alice, entityA, "github:pr:opened")

	// Direct AdminDB UPDATE (BYPASSRLS) that sets BOTH claim columns
	// must be rejected by the CHECK. This is the schema-level
	// invariant — it fires regardless of the policy + regardless of
	// the user. The claim-stamp helpers rely on it as the safety net.
	_, err := h.AdminDB.Exec(`
		UPDATE tasks SET claimed_by_agent_id = $1, claimed_by_user_id = $2
		WHERE id = $3
	`, agentID, alice, taskID)
	if err == nil {
		t.Fatal("tasks_claim_xor allowed both columns set — CHECK constraint broken")
	}
	assertPgCode(t, err, "23514", "tasks_claim_xor double-set")

	// Single-column UPDATEs of course succeed. Sanity check.
	if _, err := h.AdminDB.Exec(`UPDATE tasks SET claimed_by_agent_id = $1 WHERE id = $2`,
		agentID, taskID); err != nil {
		t.Errorf("single agent claim UPDATE rejected: %v", err)
	}
	// Setting user clears agent (single UPDATE — the helper pattern):
	if _, err := h.AdminDB.Exec(`UPDATE tasks SET claimed_by_user_id = $1, claimed_by_agent_id = NULL WHERE id = $2`,
		alice, taskID); err != nil {
		t.Errorf("user-claim flip with explicit agent-clear rejected: %v", err)
	}

	_ = teamA
}

// TestRLS_UserSelfWriteWithoutOrgClaim pins the invariant that a
// follow-up sweep relies on: a tf_app caller can write to
// their own public.users row even when the JWT claim's org_id is
// empty. The users_modify policy gates USING + WITH CHECK on
// (id = tf.current_user_id()) and never references
// tf.current_org_id() — pre-org users (multi-mode signups before
// active_org_id is set on the session) need this path to work so
// integrations setup, Jira connect, and settings updates can persist
// per-user identity (display_name, plus the host-scoped GitHub and Jira
// bindings in user_github_identities / user_jira_identities — see
// TestRLS_UserGitHubIdentitySelfAccess and TestRLS_UserJiraIdentitySelfAccess)
// before the user has joined an org.
//
// The settings.go / credentials.go handlers extract orgID via
// OrgIDFrom(r.Context()) and pass it through to s.tx.WithTx; in the
// pre-org multi-mode case that orgID is empty. This test exercises
// the same shape directly on the users table under tf_app via the
// pgtest WithUser helper so a future users_modify policy change that
// introduces an org_id dependency breaks the test instead of silently
// breaking the production handler chain.
func TestRLS_UserSelfWriteWithoutOrgClaim(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// Seed a fresh user with no org membership — mirrors the multi-mode
	// "JWT verified, callback handled, no active_org_id yet" state.
	userID := SeedUser(t, h, "no-org-user")

	// Bare self-write under empty org claim must succeed (users_modify
	// USING/WITH CHECK both gate only on id = tf.current_user_id()).
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(),
			`UPDATE public.users SET display_name = $1 WHERE id = $2`,
			"test-name", userID)
		return e
	}); err != nil {
		t.Fatalf("self-write with empty org claim should succeed under users_modify; got: %v", err)
	}

	// Confirm the write landed.
	var landed sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT display_name FROM public.users WHERE id = $1`, userID,
	).Scan(&landed); err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if landed.String != "test-name" {
		t.Errorf("display_name = %q after self-write, want %q", landed.String, "test-name")
	}

	// Negative half: same empty-org claim must NOT let userA touch
	// userB's row. The user-id branch of users_modify is the safety
	// net here — without it, an empty-org caller could spray any
	// users.id they guessed. UPDATE under USING filter returns 0 rows
	// affected on policy violation (no SQLSTATE), so check the row
	// count rather than expecting an error.
	otherUser := SeedUser(t, h, "other-user")
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		res, e := tx.ExecContext(context.Background(),
			`UPDATE public.users SET display_name = $1 WHERE id = $2`,
			"spoofed", otherUser)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("cross-user UPDATE under empty-org claim affected %d rows, want 0 (users_modify USING should filter)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-user write under empty org claim should silently no-op, not error: %v", err)
	}
}

// TestRLS_UserGitHubIdentitySelfAccess pins the acceptance
// criterion: a user can read/write only their own user_github_identities
// rows. The policies (user_github_identities_modify /
// _select) gate purely on (user_id = tf.current_user_id()) with no org
// leg, so a pre-org signup can bind a PAT-derived / login-claim identity
// before joining an org — the same empty-org-claim path the users table
// test above guards. Two halves: self insert+read succeeds under an empty
// org claim; a cross-user insert is refused by the WITH CHECK.
func TestRLS_UserGitHubIdentitySelfAccess(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	userID := SeedUser(t, h, "ident-self")
	otherUser := SeedUser(t, h, "ident-other")

	// Self write under empty org claim must succeed.
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_github_identities
				(user_id, github_base_url, login, source, verified_at)
			VALUES ($1, 'https://github.com', $2, 'pat', now())
		`, userID, "self-login")
		return e
	}); err != nil {
		t.Fatalf("self identity write under empty org claim should succeed; got: %v", err)
	}

	// Self read returns the row under the same claim.
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		var login string
		if e := tx.QueryRowContext(context.Background(),
			`SELECT login FROM public.user_github_identities WHERE user_id = $1 AND github_base_url = 'https://github.com'`,
			userID,
		).Scan(&login); e != nil {
			return e
		}
		if login != "self-login" {
			t.Errorf("self identity read = %q, want %q", login, "self-login")
		}
		return nil
	}); err != nil {
		t.Fatalf("self identity read under empty org claim should succeed; got: %v", err)
	}

	// Cross-user read is filtered to zero rows by user_github_identities_select's
	// USING clause (a SELECT denial is a 0-row result, not a 42501). otherUser
	// must not see userID's row even when naming the key explicitly — the
	// self-only read contract the multi-mode team-members endpoint leans on.
	if err := h.WithUser(t, otherUser, "", func(tx *sql.Tx) error {
		var login string
		e := tx.QueryRowContext(context.Background(),
			`SELECT login FROM public.user_github_identities WHERE user_id = $1 AND github_base_url = 'https://github.com'`,
			userID,
		).Scan(&login)
		if e == nil {
			t.Errorf("cross-user identity read returned %q; want no rows (RLS USING filter)", login)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-user read should filter to zero rows, not error: %v", err)
	}

	// Cross-user insert must be refused by user_github_identities_modify's
	// WITH CHECK (user_id != current_user_id() → policy violation, SQLSTATE
	// 42501).
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_github_identities
				(user_id, github_base_url, login, source, verified_at)
			VALUES ($1, 'https://github.com', $2, 'pat', now())
		`, otherUser, "spoofed")
		return e
	}); err == nil {
		t.Fatal("cross-user identity insert should violate WITH CHECK, but succeeded")
	} else {
		assertPgCode(t, err, "42501", "cross-user identity insert")
	}

	// And the spoofed row must not exist.
	var n int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM public.user_github_identities WHERE user_id = $1`, otherUser,
	).Scan(&n); err != nil {
		t.Fatalf("read-back count: %v", err)
	}
	if n != 0 {
		t.Errorf("otherUser identity rows = %d, want 0", n)
	}
}

// TestRLS_UserJiraIdentitySelfAccess pins the acceptance
// criterion: a user can read/write only their own user_jira_identities
// rows, mirroring the GitHub sibling above. The policies
// (user_jira_identities_modify / _select) gate purely on
// (user_id = tf.current_user_id()) with no org leg, so a pre-org signup
// can bind a PAT-derived identity before joining an org. Two halves: self
// insert+read succeeds under an empty org claim; a cross-user insert is
// refused by the WITH CHECK.
func TestRLS_UserJiraIdentitySelfAccess(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	userID := SeedUser(t, h, "jira-ident-self")
	otherUser := SeedUser(t, h, "jira-ident-other")

	const host = "https://jira.example.com"

	// Self write under empty org claim must succeed.
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_jira_identities
				(user_id, jira_base_url, account_id, display_name, source, verified_at)
			VALUES ($1, $2, $3, $4, 'pat', now())
		`, userID, host, "acc-self", "Self Jira")
		return e
	}); err != nil {
		t.Fatalf("self identity write under empty org claim should succeed; got: %v", err)
	}

	// Self read returns the row under the same claim.
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		var accountID string
		if e := tx.QueryRowContext(context.Background(),
			`SELECT account_id FROM public.user_jira_identities WHERE user_id = $1 AND jira_base_url = $2`,
			userID, host,
		).Scan(&accountID); e != nil {
			return e
		}
		if accountID != "acc-self" {
			t.Errorf("self identity read = %q, want %q", accountID, "acc-self")
		}
		return nil
	}); err != nil {
		t.Fatalf("self identity read under empty org claim should succeed; got: %v", err)
	}

	// Cross-user read is filtered to zero rows by user_jira_identities_select's
	// USING clause (a SELECT denial is a 0-row result, not a 42501). otherUser
	// must not see userID's row even when naming the key explicitly — the
	// self-only read contract the multi-mode team-members endpoint leans on.
	if err := h.WithUser(t, otherUser, "", func(tx *sql.Tx) error {
		var accountID string
		e := tx.QueryRowContext(context.Background(),
			`SELECT account_id FROM public.user_jira_identities WHERE user_id = $1 AND jira_base_url = $2`,
			userID, host,
		).Scan(&accountID)
		if e == nil {
			t.Errorf("cross-user identity read returned %q; want no rows (RLS USING filter)", accountID)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-user read should filter to zero rows, not error: %v", err)
	}

	// A GitHub row for the same user on the same backend must coexist with
	// the Jira row — distinct sibling tables, no collision (acceptance
	// criterion: one user holds a GitHub row and a Jira row simultaneously).
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_github_identities
				(user_id, github_base_url, login, source, verified_at)
			VALUES ($1, 'https://github.com', 'octo', 'pat', now())
		`, userID)
		return e
	}); err != nil {
		t.Fatalf("coexisting GitHub identity write should succeed; got: %v", err)
	}

	// A second Jira site for the same user is a distinct row (the UNIQUE is
	// (user_id, jira_base_url), so two hosts don't collide).
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_jira_identities
				(user_id, jira_base_url, account_id, display_name, source, verified_at)
			VALUES ($1, 'https://other.atlassian.net', 'acc-other-site', 'Self Elsewhere', 'pat', now())
		`, userID)
		return e
	}); err != nil {
		t.Fatalf("second Jira-site identity write should succeed (distinct host); got: %v", err)
	}

	// Cross-user insert must be refused by user_jira_identities_modify's
	// WITH CHECK (user_id != current_user_id() → policy violation, SQLSTATE
	// 42501).
	if err := h.WithUser(t, userID, "", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(context.Background(), `
			INSERT INTO public.user_jira_identities
				(user_id, jira_base_url, account_id, display_name, source, verified_at)
			VALUES ($1, $2, $3, $4, 'pat', now())
		`, otherUser, host, "acc-spoof", "Spoofed")
		return e
	}); err == nil {
		t.Fatal("cross-user identity insert should violate WITH CHECK, but succeeded")
	} else {
		assertPgCode(t, err, "42501", "cross-user Jira identity insert")
	}

	// And the spoofed row must not exist.
	var n int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM public.user_jira_identities WHERE user_id = $1`, otherUser,
	).Scan(&n); err != nil {
		t.Fatalf("read-back count: %v", err)
	}
	if n != 0 {
		t.Errorf("otherUser Jira identity rows = %d, want 0", n)
	}
}

// assertPgCode asserts that err is a *pgconn.PgError with the given
// SQLSTATE. Postgres error message text drifts across versions, but
// SQLSTATE codes are spec-stable — assert codes, not text. The
// `what` arg is just a label for the failure message.
func assertPgCode(t *testing.T, err error, code, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: got nil error, want SQLSTATE %s", what, code)
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s: err is not *pgconn.PgError: %v", what, err)
		return
	}
	if pgErr.Code != code {
		t.Errorf("%s: SQLSTATE = %s (msg %q), want %s", what, pgErr.Code, pgErr.Message, code)
	}
}

// TestRLS_TeamGitHubRepos pins the team_github_repos RLS contract:
// SELECT is gated by team membership, INSERT/DELETE by team
// admin, and rows are isolated cross-team. The mirror of the
// jira_project_status_rules policies.
func TestRLS_TeamGitHubRepos(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")

	// A second team in the same org with its own admin (carol). Bob is
	// NOT a member of teamB, so its rows must stay invisible to him.
	teamB := SeedTeam(t, h, orgA, "team-b")
	carol := SeedUser(t, h, "carol")
	AddOrgMember(t, h, carol, orgA, teamB, "member", "admin")

	// The registry rows the tracking rows reference. Minted on the admin pool
	// because this test is about the team_github_repos policies, not about who
	// may create a repository.
	repoA := SeedRepository(t, h, orgA, "acme", "a-repo")
	repoB := SeedRepository(t, h, orgA, "acme", "b-repo")

	// carol (admin of teamB) tracks a repo for teamB.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`, teamB, repoB, orgA)
		return e
	}); err != nil {
		t.Fatalf("carol INSERT teamB repo: %v", err)
	}

	// alice (admin of teamA — owner is implicitly team admin) tracks a
	// repo for teamA.
	if err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`, teamA, repoA, orgA)
		return e
	}); err != nil {
		t.Fatalf("alice INSERT teamA repo: %v", err)
	}

	// bob (member of teamA only) sees exactly teamA's row — teamB's row
	// is filtered out by the membership semi-join in the SELECT policy.
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		rows, e := tx.Query(`
			SELECT r.repo FROM team_github_repos g
			JOIN repositories r ON r.id = g.repository_id
			ORDER BY r.repo`)
		if e != nil {
			return e
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var r string
			if e := rows.Scan(&r); e != nil {
				return e
			}
			got = append(got, r)
		}
		if len(got) != 1 || got[0] != "a-repo" {
			t.Errorf("bob sees %v; want only [a-repo] (teamB isolated)", got)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("bob SELECT: %v", err)
	}

	// bob (non-admin) cannot INSERT into teamA — INSERT WITH CHECK is
	// admin-gated → SQLSTATE 42501.
	err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`, teamA, repoA, orgA)
		return e
	})
	assertPgCode(t, err, "42501", "bob INSERT teamA repo (non-admin)")

	// bob cannot DELETE teamA's row either — DELETE policy admin-gated,
	// so the row is filtered out and 0 rows are affected (no error).
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		res, e := tx.Exec(`DELETE FROM team_github_repos WHERE team_id = $1`, teamA)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("bob DELETE affected %d rows; want 0 (admin-gated)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("bob DELETE attempt: %v", err)
	}

	// carol cannot see teamA's row (she's not a member of teamA).
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM team_github_repos WHERE team_id = $1`, teamA).Scan(&n); e != nil {
			return e
		}
		if n != 0 {
			t.Errorf("carol sees %d teamA rows; want 0 (cross-team isolation)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("carol cross-team SELECT: %v", err)
	}
}

// TestTeamGitHubRepos_TrackingSurvivesUntrackingTheRegistryRow pins the
// negative space of the tracked-set foreign key: untracking is a delete on
// team_github_repos and leaves the registry row standing, while deleting the
// registry row cascades the tracking rows away with it.
//
// It replaces the old tf.org_tracked_repos() bypass test. That SECURITY
// DEFINER helper existed so a team admin's app-pool transaction could read
// every team's tracked repos while reconciling the repositories table from
// their union; the reconcile is gone (the foreign key keeps the two in step),
// and so is the helper.
func TestTeamGitHubRepos_TrackingSurvivesUntrackingTheRegistryRow(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, teamA := SeedOrgWithUser(t, h, "alice")
	teamA2 := SeedTeam(t, h, orgA, "team-a2")

	repoID := SeedTrackedRepo(t, h, orgA, teamA, "acme", "a1")
	SeedTrackedRepo(t, h, orgA, teamA2, "acme", "a1") // shared by both teams

	// Untracking on one team leaves the registry row and the other team's
	// tracking row alone.
	MustExec(t, h.AdminDB, `DELETE FROM team_github_repos WHERE team_id = $1`, teamA)
	var registryRows, trackingRows int
	if err := h.AdminDB.QueryRow(`SELECT count(*) FROM repositories WHERE id = $1`, repoID).Scan(&registryRows); err != nil {
		t.Fatalf("count registry rows: %v", err)
	}
	if registryRows != 1 {
		t.Errorf("registry rows after untracking = %d; want 1 — untracking must not delete the repository", registryRows)
	}
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM team_github_repos WHERE repository_id = $1`, repoID,
	).Scan(&trackingRows); err != nil {
		t.Fatalf("count tracking rows: %v", err)
	}
	if trackingRows != 1 {
		t.Errorf("tracking rows after one team untracked = %d; want 1 (the other team)", trackingRows)
	}

	// The other direction cascades: a tracking row is a statement about a
	// repository and cannot outlive it.
	MustExec(t, h.AdminDB, `DELETE FROM repositories WHERE id = $1`, repoID)
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM team_github_repos WHERE repository_id = $1`, repoID,
	).Scan(&trackingRows); err != nil {
		t.Fatalf("count tracking rows after registry delete: %v", err)
	}
	if trackingRows != 0 {
		t.Errorf("tracking rows after the registry row went = %d; want 0 (ON DELETE CASCADE)", trackingRows)
	}
}

// TestTeamGitHubRepos_CrossTenantRowIsUnrepresentable pins the composite
// foreign keys. A tracking row joins a team to a repository and both belong to
// an org, so the row carries org_id and references (team_id, org_id) and
// (repository_id, org_id) rather than the two ids alone.
//
// This is asserted on the ADMIN pool on purpose: RLS refuses a cross-org write
// on the app pool, and the store refuses one before it reaches the database at
// all. Both of those are checks somebody has to remember to keep. The
// constraint is the one that holds when they are bypassed, and the admin pool
// is where "bypassed" is testable.
//
// The failure this prevents is quiet rather than loud. A row pairing org A's
// team with org B's repository satisfies both single-column keys, and every
// org-scoped read filters it out — so the save reports success and the repo is
// never tracked, never polled, never profiled.
func TestTeamGitHubRepos_CrossTenantRowIsUnrepresentable(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, _, teamA := SeedOrgWithUser(t, h, "alice")
	orgB, _, teamB := SeedOrgWithUser(t, h, "bob")
	repoA := SeedRepository(t, h, orgA, "acme", "shared")
	repoB := SeedRepository(t, h, orgB, "acme", "shared")

	// org A's team pointed at org B's repository, stamped with either org.
	// Whichever org_id is written, one of the two composite keys has no parent.
	for _, tc := range []struct{ name, org string }{
		{"stamped with the team's org", orgA},
		{"stamped with the repository's org", orgB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.AdminDB.Exec(
				`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`,
				teamA, repoB, tc.org)
			if err == nil {
				t.Fatal("a cross-tenant tracking row was accepted; the composite foreign keys are not doing their job")
			}
			if !strings.Contains(err.Error(), "foreign key") && !strings.Contains(err.Error(), "23503") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}

	// The same-tenant pairs still insert, so the constraint is discriminating
	// rather than just strict.
	MustExec(t, h.AdminDB,
		`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`,
		teamA, repoA, orgA)
	MustExec(t, h.AdminDB,
		`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES ($1, $2, $3)`,
		teamB, repoB, orgB)
}
