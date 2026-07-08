package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentRunStore_Postgres runs the shared conformance suite
// against the Postgres AgentRunStore impl. Each subtest gets a
// fresh org + team + user + prompt + agent seed; the suite drives
// every method through its happy and edge paths.
func TestAgentRunStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the run lifecycle
	// statements run without a JWT-claims tx. Production wiring
	// uses the app pool, but the conformance suite is about
	// behavior, not auth; the cross-org leakage test below
	// exercises the org_id defense-in-depth filter directly.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunAgentRunStoreConformance(t, func(t *testing.T) (db.AgentRunStore, string, string, dbtest.AgentRunSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgAgentRunOrg(t, h)
		promptID := seedPgAgentRunPrompt(t, h, orgID, userID)
		seeder := newPgAgentRunSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.AgentRuns, orgID, userID, seeder
	})
}

// seedPgAgentRunOrg builds the auth.user + public.user + org +
// org_membership + default team + agent graph the AgentRunStore
// needs. Mirrors seedPgOrgUserAgent from tasks_test.go.
func seedPgAgentRunOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("agentrun-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "AgentRun Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "AgentRun Org "+orgID[:8], "ar-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Conformance Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// seedPgAgentRunPrompt inserts a user-source prompt the conformance
// suite's runs FK into. Stable id `p_agentrun_test` matches the
// constant the shared harness expects.
func seedPgAgentRunPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ('p_agentrun_test', $1, $2, $3, 'AgentRun Test', 'body', 'user', '', now(), now())
	`, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return "p_agentrun_test"
}

// newPgAgentRunSeeder builds the FactorySeeder-style callbacks the
// conformance harness uses to stage non-run fixture rows. INSERTs
// carry org_id explicitly so the cross-org leakage test below can
// reuse the same seeder for two orgs in parallel.
func newPgAgentRunSeeder(conn *sql.DB, orgID, userID, agentID, promptID string) dbtest.AgentRunSeeder {
	_ = promptID // referenced via the conformance suite's constant
	return dbtest.AgentRunSeeder{
		Entity: func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			sourceID := fmt.Sprintf("agentrun-%s-%s", suffix, id[:8])
			if _, err := conn.Exec(`
				INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES ($1, $2, 'github', $3, 'pr', $4, $5, '{}'::jsonb, $6)
			`, id, orgID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity %s: %v", suffix, err)
			}
			return id
		},
		Event: func(t *testing.T, entityID, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES ($1, $2, $3, $4, '', '{}'::jsonb, $5)
			`, id, orgID, entityID, eventType, time.Now().UTC()); err != nil {
				t.Fatalf("seed event %s: %v", eventType, err)
			}
			return id
		},
		Task: func(t *testing.T, entityID, eventType, primaryEventID string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id,
				                   status, scoring_status, priority_score, created_at)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'team', $4, $5, '', $6, 'queued', 'pending', 0.5, $7)
			`, id, orgID, userID, entityID, eventType, primaryEventID, time.Now().UTC()); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			return id
		},
		StampAgentClaim: func(t *testing.T, taskID, agent string) {
			t.Helper()
			if _, err := conn.Exec(
				`UPDATE tasks SET claimed_by_agent_id = $1::uuid, claimed_by_user_id = NULL WHERE id = $2 AND org_id = $3`,
				agent, taskID, orgID,
			); err != nil {
				t.Fatalf("stamp claim: %v", err)
			}
		},
		EventHandler: func(t *testing.T, eventType string) string {
			t.Helper()
			id := uuid.New().String()
			// Minimal rule shape. source='user' requires a non-NULL
			// creator_user_id (event_handlers_system_has_no_creator CHECK);
			// team_id resolves from the org's sole seeded team, mirroring
			// the Task seeder's subquery.
			if _, err := conn.Exec(`
				INSERT INTO event_handlers
					(id, org_id, creator_user_id, team_id, kind, event_type, enabled, source,
					 name, default_priority, sort_order)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'rule', $4, true, 'user', 'fence-fk', 0.5, 100)
			`, id, orgID, userID, eventType); err != nil {
				t.Fatalf("seed event_handler: %v", err)
			}
			return id
		},
		BlueprintRun: func(t *testing.T, taskID string) string {
			t.Helper()
			// runs.blueprint_run_id is NOT NULL — every run needs a parent
			// blueprint_run. Mint a fresh blueprint + blueprint_run per
			// call. Postgres requires org_id on both; blueprint_runs with
			// trigger_type='manual' also needs a non-NULL creator_user_id
			// (blueprint_runs_creator_matches_trigger_type CHECK). team_id
			// resolves from the org's sole seeded team, mirroring the other
			// seeders.
			bpID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
				VALUES ($1, $2, $3,
				        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
				        'Conformance BP', 'user')
			`, bpID, orgID, userID); err != nil {
				t.Fatalf("seed blueprint: %v", err)
			}
			brID := uuid.New().String()
			if _, err := conn.Exec(`
				INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
				VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', '[]')
			`, brID, orgID, userID, bpID, taskID); err != nil {
				t.Fatalf("seed blueprint_run: %v", err)
			}
			return brID
		},
		SetBlueprintRunStatus: func(t *testing.T, blueprintRunID, status string) {
			t.Helper()
			// Raw UPDATE — must NOT cascade onto child runs (unlike
			// BlueprintStore.MarkRunStatus), so the parked child stays parked.
			if _, err := conn.Exec(`UPDATE blueprint_runs SET status = $1 WHERE id = $2`, status, blueprintRunID); err != nil {
				t.Fatalf("set blueprint_run status: %v", err)
			}
		},
		SetRunMemory: func(t *testing.T, runID, entityID, content string) {
			t.Helper()
			memID := uuid.New().String()
			if content == dbtest.NullMemorySentinel {
				if _, err := conn.Exec(`
					INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, runID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO run_memory (id, org_id, run_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, runID, entityID, content); err != nil {
				t.Fatalf("seed memory: %v", err)
			}
		},
		AgentID: agentID,
	}
}

// TestAgentRunStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org_id filter as the only line of
// defense (AdminDB bypasses RLS), org A's queries can't see org B's
// runs. In production the RLS policies add a second layer; this test
// validates the WHERE-clause filter on its own so a regression there
// can't silently rely on RLS to compensate.
func TestAgentRunStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, agentA := seedPgAgentRunOrg(t, h)
	orgB, userB, agentB := seedPgAgentRunOrg(t, h)
	_ = agentA
	_ = agentB
	seedPgAgentRunPromptIn(t, h, "p_xleak_A", orgA, userA)
	seedPgAgentRunPromptIn(t, h, "p_xleak_B", orgB, userB)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	// Seed an entity + task + run in each org via the AdminDB so
	// the FK chain is satisfied.
	mkChain := func(t *testing.T, orgID, userID, promptID, runID string) (taskID string) {
		t.Helper()
		entityID := uuid.New().String()
		eventID := uuid.New().String()
		taskID = uuid.New().String()
		if _, err := h.AdminDB.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', 'Cross-org test', '', '{}'::jsonb, now())
		`, entityID, orgID, "xleak-"+orgID[:8]); err != nil {
			t.Fatalf("entity: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, 'github:pr:opened', '', '{}'::jsonb, now())
		`, eventID, orgID, entityID); err != nil {
			t.Fatalf("event: %v", err)
		}
		if _, err := h.AdminDB.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
			VALUES ($1, $2, $3,
			        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
			        'team', $4, 'github:pr:opened', '', $5, 'queued', 'pending', 0.5)
		`, taskID, orgID, userID, entityID, eventID); err != nil {
			t.Fatalf("task: %v", err)
		}
		if err := stores.AgentRuns.Create(ctx, orgID, domain.AgentRun{
			ID: runID, TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
			CreatorUserID:  userID,
			BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
		}); err != nil {
			t.Fatalf("Create run: %v", err)
		}
		return taskID
	}
	runA := uuid.New().String()
	runB := uuid.New().String()
	taskA := mkChain(t, orgA, userA, "p_xleak_A", runA)
	_ = mkChain(t, orgB, userB, "p_xleak_B", runB)

	// Org A's view must NOT see B's run.
	if got, err := stores.AgentRuns.Get(ctx, orgA, runB); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgA Get returned orgB run %s; defense-in-depth filter leaked", runB)
	}
	if got, err := stores.AgentRuns.Get(ctx, orgB, runA); err != nil {
		t.Fatalf("Get cross-org reverse: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA run %s", runA)
	}

	// ListForTask scoped to orgB looking at orgA's task must
	// return nothing.
	if runs, err := stores.AgentRuns.ListForTask(ctx, orgB, taskA); err != nil {
		t.Fatalf("ListForTask cross-org: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("orgB ListForTask(orgA task) returned %d runs; want 0", len(runs))
	}
}

// TestAgentRunStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for runs. Where TestAgentRunStore_Postgres_CrossOrgLeakage
// above wires both pools against AdminDB to prove the defense-in-depth
// WHERE-clause filter is intact, this test runs the store through the
// app pool under tf_app with real JWT claims so the actual
// runs_select / runs_insert policies are exercised. Same-org reads
// succeed; cross-org reads are silently filtered (USING); cross-org
// Create raises 42501 from runs_insert WITH CHECK.
func TestAgentRunStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgAgentRunOrg(t, h)
	orgB, bob, _ := seedPgAgentRunOrg(t, h)
	seedPgAgentRunPromptIn(t, h, "p_rls_A", orgA, alice)
	seedPgAgentRunPromptIn(t, h, "p_rls_B", orgB, bob)

	// Seed entity + event + task + run in orgA via admin so the row
	// exists. Whether bob (claims orgB) can see/mutate it is the
	// question.
	entityA := uuid.New().String()
	eventA := uuid.New().String()
	taskA := uuid.New().String()
	runA := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'RLS Cross-org', '', '{}'::jsonb, now())
	`, entityA, orgA, "rls-cross-"+orgA[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventA, orgA, entityA); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskA, orgA, alice, entityA, eventA); err != nil {
		t.Fatalf("task: %v", err)
	}
	blueprintRunA := seedPgBlueprintRun(t, h, orgA, alice, taskA)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, task_id, team_id, prompt_id, status, model, creator_user_id, trigger_type, blueprint_run_id)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'p_rls_A', 'running', 'm', $4, 'manual', $5)
	`, runA, orgA, taskA, alice, blueprintRunA); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			run, err := pgstore.NewForTx(tx, pgtest.SecretKey).AgentRuns.Get(ctx, orgA, runA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if run == nil {
				t.Errorf("alice Get(orgA, runA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			run, err := pgstore.NewForTx(tx, pgtest.SecretKey).AgentRuns.Get(ctx, orgA, runA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if run != nil {
				t.Errorf("bob Get(orgA, runA) returned %+v; RLS USING filter leaked orgA's run to orgB", run)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; the row would land with
		// org_id=orgA referencing orgA's task. We want runs_insert
		// WITH CHECK to reject, not a missing FK target.
		newRunID := uuid.New().String()
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx, pgtest.SecretKey).AgentRuns.Create(ctx, orgA, domain.AgentRun{
				ID: newRunID, TaskID: taskA, PromptID: "p_rls_A",
				Status: "running", Model: "m",
				TriggerType: "manual", CreatorUserID: bob,
				// Valid FK target in orgA so the rejection is the
				// runs_insert WITH CHECK, not a missing blueprint_run.
				BlueprintRunID: blueprintRunA,
			})
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// TestAgentRunStore_Postgres_Create_UnderAppPoolRLS pins the two
// app-pool fixes against actual RLS, not the AdminDB-bypassed
// conformance setup:
//
//  1. Event-triggered Create routes to the admin pool. Wired
//     against AppDB for app-half + AdminDB for admin-half, calling
//     Create with trigger_type='event' must succeed even though
//     the runs_insert RLS policy would reject a null-creator row
//     under tf_app.
//
//  2. Manual Create's COALESCE walks past the LocalDefaultUserID
//     sentinel. Wired same way, with JWT claims bound to a real
//     org member; if the caller passes the sentinel as
//     CreatorUserID, the SQL strips it (via the Go-side filter)
//     and tf.current_user_id() supplies the right value so the
//     RLS predicate (creator_user_id = tf.current_user_id())
//     passes.
func TestAgentRunStore_Postgres_Create_UnderAppPoolRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgAgentRunOrg(t, h)
	seedPgAgentRunPromptIn(t, h, "p_rls_test", orgID, userID)
	// Seed entity + task on the admin side so the FK chain exists
	// before the app-pool Create lands. (The Create itself is the
	// thing under test; setup uses admin.)
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'RLS Test', '', '{}'::jsonb, now())
	`, entityID, orgID, "rls-"+orgID[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("task: %v", err)
	}

	// Wire AgentRunStore against the real admin pool (BYPASSRLS)
	// for the system-write path and the real app pool (RLS-active
	// under tf_app via WithTx) for request-equivalent paths. Note
	// that pgstore.New takes (admin, app) — admin first — so
	// passing in the order shown matches the production shape.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	// ---- Event-triggered Create (fix #5) ----
	// No JWT claims tx needed because the admin pool is used for
	// the insert. The bare context call should succeed.
	eventRunID := uuid.New().String()
	if err := stores.AgentRuns.Create(context.Background(), orgID, domain.AgentRun{
		ID: eventRunID, TaskID: taskID, PromptID: "p_rls_test", Status: "running", Model: "m",
		TriggerType:    "event",
		BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
		// CreatorUserID empty — CHECK requires NULL for event runs.
	}); err != nil {
		t.Fatalf("event-triggered Create under app-pool wiring: %v", err)
	}
	// Verify it landed.
	var landedTrigger string
	var landedCreator sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT trigger_type, creator_user_id::text FROM runs WHERE id = $1`,
		eventRunID,
	).Scan(&landedTrigger, &landedCreator); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if landedTrigger != "event" {
		t.Errorf("trigger_type = %q, want event", landedTrigger)
	}
	if landedCreator.Valid {
		t.Errorf("creator_user_id = %q, want NULL (event-trigger CHECK)", landedCreator.String)
	}

	// ---- Manual Create with LocalDefaultUserID sentinel (fix #6) ----
	// Run inside WithTx so JWT claims are set; the COALESCE in
	// Create then resolves tf.current_user_id() to userID. With
	// the sentinel filter, the manual path lands with the real
	// claimed user.
	manualRunID := uuid.New().String()
	manualBlueprintRun := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	if err := stores.Tx.WithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
		return tx.AgentRuns.Create(context.Background(), orgID, domain.AgentRun{
			ID: manualRunID, TaskID: taskID, PromptID: "p_rls_test", Status: "running", Model: "m",
			TriggerType:    "manual",
			BlueprintRunID: manualBlueprintRun,
			CreatorUserID:  runmode.LocalDefaultUserID, // the sentinel the pre-store spawner still passes
		})
	}); err != nil {
		t.Fatalf("manual Create with sentinel under app-pool: %v", err)
	}
	var manualCreator sql.NullString
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM runs WHERE id = $1`,
		manualRunID,
	).Scan(&manualCreator); err != nil {
		t.Fatalf("read back manual: %v", err)
	}
	if !manualCreator.Valid {
		t.Fatalf("manual creator_user_id is NULL; want %s (resolved from JWT claims)", userID)
	}
	if manualCreator.String != userID {
		t.Errorf("manual creator_user_id = %q, want %q (JWT-claimed user, not the SQLite sentinel)",
			manualCreator.String, userID)
	}
}

// TestAgentRunStore_Postgres_LifecycleWrites_UnderSyntheticClaims
// pins the routing the delegate spawner uses for manual-run
// bookkeeping: lifecycle writes (Complete, MarkCancelledIfActive,
// MarkResuming) wrapped in SyntheticClaimsWithTx must pass RLS under
// tf_app and land the expected status. Mirrors the spawner's per-call-site
// branch:
//
//	if triggerType == "manual" {
//	    s.tx.SyntheticClaimsWithTx(...) // this path
//	} else {
//	    s.agentRuns.XxxSystem(...)
//	}
//
// The admin-pool System variants are already pgtested via the
// conformance suite. This test specifically exercises the app-pool
// arm under realistic RLS — the only way the manual-routing path
// can succeed in multi-mode without the tx wrap is if the
// row's creator_user_id matches tf.current_user_id() under the
// claims set by WithTx.
func TestAgentRunStore_Postgres_LifecycleWrites_UnderSyntheticClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgAgentRunOrg(t, h)
	seedPgAgentRunPromptIn(t, h, "p_lc_test", orgID, userID)

	// FK chain on admin (same pattern as
	// TestAgentRunStore_Postgres_Create_UnderAppPoolRLS).
	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'LC Test', '', '{}'::jsonb, now())
	`, entityID, orgID, "lc-"+orgID[:8]); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
	`, eventID, orgID, entityID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'team', $4, 'github:pr:ci_check_failed', '', $5, 'queued', 'pending', 0.5)
	`, taskID, orgID, userID, entityID, eventID); err != nil {
		t.Fatalf("task: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	// Seed a manual run row owned by userID — the spawner does this
	// via Create at goroutine spawn time, before the goroutine
	// reaches any of the lifecycle writes under test.
	runID := uuid.New().String()
	lcBlueprintRun := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	if err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.AgentRuns.Create(ctx, orgID, domain.AgentRun{
			ID: runID, TaskID: taskID, PromptID: "p_lc_test", Status: "running", Model: "m",
			TriggerType: "manual", CreatorUserID: userID,
			BlueprintRunID: lcBlueprintRun,
		})
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Drive each lifecycle write through SyntheticClaimsWithTx — the
	// shape the spawner uses for every manual-run bookkeeping point.

	// MarkResuming (open-run resume entry).
	var resumed bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Park the run `open` first so MarkResuming's guard fires.
		if err := tx.AgentRuns.SetStatus(ctx, orgID, runID, "open"); err != nil {
			return err
		}
		r, mErr := tx.AgentRuns.MarkResuming(ctx, orgID, runID)
		resumed = r
		return mErr
	}); err != nil {
		t.Fatalf("MarkResuming under synth claims: %v", err)
	}
	if !resumed {
		t.Errorf("MarkResuming: flipped=false, want true (was open)")
	}

	// AddPartialTotals (per-turn accumulation).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.AgentRuns.AddPartialTotals(ctx, orgID, runID, 0.5, 1500, 3)
	}); err != nil {
		t.Fatalf("AddPartialTotals under synth claims: %v", err)
	}

	// Complete (terminal write — the largest of processCompletion's
	// routed writes).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.AgentRuns.Complete(ctx, orgID, runID, "completed", 0.25, 500, 2, "end_turn", "ok", "finish", "", "")
	}); err != nil {
		t.Fatalf("Complete under synth claims: %v", err)
	}

	// Verify on admin: row landed in completed (runs never park in
	// pending_approval anymore), totals reflect the
	// AddPartialTotals + Complete merge, creator stayed the original user.
	var (
		status        string
		totalCostUSD  float64
		durationMs    int
		numTurns      int
		stopReason    string
		creatorUserID sql.NullString
	)
	if err := h.AdminDB.QueryRow(`
		SELECT status, total_cost_usd, duration_ms, num_turns, stop_reason, creator_user_id::text
		FROM runs WHERE id = $1
	`, runID).Scan(&status, &totalCostUSD, &durationMs, &numTurns, &stopReason, &creatorUserID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
	if totalCostUSD != 0.75 {
		t.Errorf("total_cost_usd = %v, want 0.75 (0.5 partial + 0.25 complete)", totalCostUSD)
	}
	if durationMs != 2000 {
		t.Errorf("duration_ms = %d, want 2000 (1500 partial + 500 complete)", durationMs)
	}
	if numTurns != 5 {
		t.Errorf("num_turns = %d, want 5 (3 partial + 2 complete)", numTurns)
	}
	if stopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", stopReason)
	}
	if !creatorUserID.Valid || creatorUserID.String != userID {
		t.Errorf("creator_user_id = %v, want %s", creatorUserID, userID)
	}

	// MarkFailedIfActive on a terminal row is a no-op (guarded
	// transition). Verifies the System variant's guard fires even
	// though we never wrapped in claims for this call (spawner uses
	// it goroutine-internally with no user identity).
	failed, err := stores.AgentRuns.MarkFailedIfActiveSystem(ctx, orgID, runID, "")
	if err != nil {
		t.Fatalf("MarkFailedIfActiveSystem: %v", err)
	}
	if failed {
		t.Errorf("MarkFailedIfActiveSystem on terminal row: flipped=true, want false (guard)")
	}
}

// seedPgBlueprintRun mints a blueprint + blueprint_run pointed at the
// given task so a standalone `runs` insert can satisfy the now NOT-NULL
// runs.blueprint_run_id FK (→ blueprint_runs(id)). Mirrors the
// conformance seeder's BlueprintRun, but exposed as a plain helper for
// the RLS/cross-org tests that seed runs outside the conformance suite.
// Postgres requires org_id on both rows; trigger_type='manual' also
// requires a non-NULL creator_user_id (blueprint_runs_creator_matches_trigger_type
// CHECK).
func seedPgBlueprintRun(t *testing.T, h *pgtest.Harness, orgID, userID, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'Conformance BP', 'user')
	`, bpID, orgID, userID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', '[]')
	`, brID, orgID, userID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedPgAgentRunPromptIn is a small variant that inserts a prompt
// with an explicit id. Used by cross-org leakage which needs two
// prompts in two orgs with distinct ids.
func seedPgAgentRunPromptIn(t *testing.T, h *pgtest.Harness, id, orgID, userID string) {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'X-leak Test', 'body', 'user', '', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}
