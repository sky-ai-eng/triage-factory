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
)

// TestConversationStore_Postgres runs the shared conformance suite
// against the Postgres ConversationStore impl. Each subtest gets a
// fresh org + team + user + prompt + agent seed; the suite drives
// every method through its happy and edge paths.
func TestConversationStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	// Wire both pools against AdminDB so the run lifecycle
	// statements run without a JWT-claims tx. Production wiring
	// uses the app pool, but the conformance suite is about
	// behavior, not auth; the cross-org leakage test below
	// exercises the org_id defense-in-depth filter directly.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunConversationStoreConformance(t, func(t *testing.T) (db.ConversationStore, string, string, dbtest.ConversationSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgConversationOrg(t, h)
		promptID := seedPgConversationPrompt(t, h, orgID, userID)
		seeder := newPgConversationSeeder(h.AdminDB, orgID, userID, agentID, promptID)
		return stores.Conversations, orgID, userID, seeder
	})
}

// seedPgConversationOrg builds the auth.user + public.user + org +
// org_membership + default team + agent graph the ConversationStore
// needs. Mirrors seedPgOrgUserAgent from tasks_test.go.
func seedPgConversationOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("agentrun-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Conversation Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Conversation Org "+orgID[:8], "ar-"+orgID[:8], userID,
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

// seedPgConversationPrompt inserts a user-source prompt the conformance
// suite's runs FK into. Stable id `p_agentrun_test` matches the
// constant the shared harness expects.
func seedPgConversationPrompt(t *testing.T, h *pgtest.Harness, orgID, userID string) string {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ('p_agentrun_test', $1, $2, $3, 'Conversation Test', 'body', 'user', '', now(), now())
	`, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return "p_agentrun_test"
}

// newPgConversationSeeder builds the FactorySeeder-style callbacks the
// conformance harness uses to stage non-run fixture rows. INSERTs
// carry org_id explicitly so the cross-org leakage test below can
// reuse the same seeder for two orgs in parallel.
func newPgConversationSeeder(conn *sql.DB, orgID, userID, agentID, promptID string) dbtest.ConversationSeeder {
	_ = promptID // referenced via the conformance suite's constant
	return dbtest.ConversationSeeder{
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
		Run: func(t *testing.T, run domain.Conversation) string {
			t.Helper()
			if run.CreatorUserID == "" && run.TriggerType != "event" {
				run.CreatorUserID = userID
			}
			return seedPgConversation(t, conn, orgID, run)
		},
		ClaimRows: func(t *testing.T, conversationID string) []dbtest.ClaimRow {
			t.Helper()
			rows, err := conn.Query(`
				SELECT id::text, executor_id, boot_epoch, COALESCE(phase, ''), released_at IS NOT NULL, COALESCE(outcome, '')
				FROM claims WHERE conversation_id = $1 ORDER BY claimed_at ASC, created_at ASC
			`, conversationID)
			if err != nil {
				t.Fatalf("read claims: %v", err)
			}
			defer rows.Close()
			var out []dbtest.ClaimRow
			for rows.Next() {
				var c dbtest.ClaimRow
				if err := rows.Scan(&c.ID, &c.ExecutorID, &c.BootEpoch, &c.Phase, &c.Released, &c.Outcome); err != nil {
					t.Fatalf("scan claim: %v", err)
				}
				out = append(out, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("claims rows: %v", err)
			}
			return out
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
		BlueprintRun: func(t *testing.T, taskID string) string {
			t.Helper()
			// The origin CHECK requires blueprint_run_id — every run needs a parent
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
					INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, NULL)
				`, memID, orgID, runID, entityID); err != nil {
					t.Fatalf("seed null memory: %v", err)
				}
				return
			}
			if _, err := conn.Exec(`
				INSERT INTO conversation_memory (id, org_id, conversation_id, entity_id, agent_content) VALUES ($1, $2, $3, $4, $5)
			`, memID, orgID, runID, entityID, content); err != nil {
				t.Fatalf("seed memory: %v", err)
			}
		},
		SeedRawMessage: func(t *testing.T, runID, column, rawJSON string) int64 {
			t.Helper()
			if column != "reasoning" && column != "content_blocks" {
				t.Fatalf("SeedRawMessage: unsupported column %q", column)
			}
			// column is a fixed test-controlled name (not user input), so
			// string-building the column into the statement is safe here.
			// rawJSON must be syntactically valid JSON — jsonb enforces that
			// at the storage layer — but can be any shape, including one
			// that fails to unmarshal into the target Go slice.
			var id int64
			if err := conn.QueryRow(
				`INSERT INTO messages (org_id, conversation_id, role, subtype, content, `+column+`)
				 VALUES ($1, $2, 'assistant', 'text', 'x', $3::jsonb) RETURNING id`,
				orgID, runID, rawJSON,
			).Scan(&id); err != nil {
				t.Fatalf("seed raw message (%s): %v", column, err)
			}
			return id
		},
		AgentID: agentID,
	}
}

// TestConversationStore_Postgres_CrossOrgLeakage pins the defense-in-
// depth guarantee: even with the org_id filter as the only line of
// defense (AdminDB bypasses RLS), org A's queries can't see org B's
// runs. In production the RLS policies add a second layer; this test
// validates the WHERE-clause filter on its own so a regression there
// can't silently rely on RLS to compensate.
func TestConversationStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, userA, agentA := seedPgConversationOrg(t, h)
	orgB, userB, agentB := seedPgConversationOrg(t, h)
	_ = agentA
	_ = agentB
	seedPgConversationPromptIn(t, h, "p_xleak_A", orgA, userA)
	seedPgConversationPromptIn(t, h, "p_xleak_B", orgB, userB)

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
		seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
			ID: runID, TaskID: taskID, PromptID: promptID, Status: "running", Model: "m",
			CreatorUserID:  userID,
			BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
		})
		return taskID
	}
	runA := uuid.New().String()
	runB := uuid.New().String()
	taskA := mkChain(t, orgA, userA, "p_xleak_A", runA)
	_ = mkChain(t, orgB, userB, "p_xleak_B", runB)

	// Org A's view must NOT see B's run.
	if got, err := stores.Conversations.Get(ctx, orgA, runB); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgA Get returned orgB run %s; defense-in-depth filter leaked", runB)
	}
	if got, err := stores.Conversations.Get(ctx, orgB, runA); err != nil {
		t.Fatalf("Get cross-org reverse: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA run %s", runA)
	}

	// ListForTask scoped to orgB looking at orgA's task must
	// return nothing.
	if runs, err := stores.Conversations.ListForTask(ctx, orgB, taskA); err != nil {
		t.Fatalf("ListForTask cross-org: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("orgB ListForTask(orgA task) returned %d runs; want 0", len(runs))
	}
}

// TestConversationStore_Postgres_CrossOrgRLSDenied pins the production
// RLS layer for runs. Where TestConversationStore_Postgres_CrossOrgLeakage
// above wires both pools against AdminDB to prove the defense-in-depth
// WHERE-clause filter is intact, this test runs the store through the
// app pool under tf_app with real JWT claims so the actual
// runs_select / runs_insert policies are exercised. Same-org reads
// succeed; cross-org reads are silently filtered (USING); cross-org
// Create raises 42501 from runs_insert WITH CHECK.
func TestConversationStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgConversationOrg(t, h)
	orgB, bob, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_rls_A", orgA, alice)
	seedPgConversationPromptIn(t, h, "p_rls_B", orgB, bob)

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
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model, creator_user_id, trigger_type, blueprint_run_id)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        'p_rls_A', 'running', 'm', $4, 'manual', $5)
	`, runA, orgA, taskA, alice, blueprintRunA); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	ctx := context.Background()

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			run, err := pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.Get(ctx, orgA, runA)
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
			run, err := pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.Get(ctx, orgA, runA)
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

	t.Run("cross_org_write_filtered", func(t *testing.T) {
		// The insert path moved to the admin-pool run queue (nothing
		// mints conversations under tf_app), so the write arm to pin is
		// an UPDATE: bob's lifecycle write against orgA's run must be
		// silently filtered by the USING clause — no rows touched.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx, pgtest.SecretKey).Conversations.SetWorktreePath(ctx, orgA, runA, "/tmp/bob-was-here")
		})
		if err != nil {
			t.Fatalf("cross-org SetWorktreePath: %v", err)
		}
		var wt sql.NullString
		if err := h.AdminDB.QueryRow(`SELECT worktree_path FROM conversations WHERE id = $1`, runA).Scan(&wt); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if wt.Valid && wt.String == "/tmp/bob-was-here" {
			t.Errorf("cross-org UPDATE landed; RLS USING filter leaked orgA's run to orgB")
		}
	})
}

// TestConversationStore_Postgres_LifecycleWrites_UnderSyntheticClaims
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
func TestConversationStore_Postgres_LifecycleWrites_UnderSyntheticClaims(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_lc_test", orgID, userID)

	// FK chain on admin (same pattern as
	// TestConversationStore_Postgres_Create_UnderAppPoolRLS).
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

	// Seed a manual run row owned by userID — the queue's EnqueueRun does
	// this in production before any lifecycle write runs.
	lcBlueprintRun := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	runID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_lc_test", Status: "running", Model: "m",
		TriggerType: "manual", CreatorUserID: userID,
		BlueprintRunID: lcBlueprintRun,
	})

	// Drive each lifecycle write through SyntheticClaimsWithTx — the
	// shape the spawner uses for every manual-run bookkeeping point.

	// MarkOpen (park) then MarkQueuedForResume (resume-by-enqueue) — the
	// open→queued CAS the resume path drives under the user's claims.
	var parked, requeued bool
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		p, mErr := tx.Conversations.MarkOpen(ctx, orgID, runID)
		parked = p
		if mErr != nil {
			return mErr
		}
		r, mErr := tx.Conversations.MarkQueuedForResume(ctx, orgID, runID)
		requeued = r
		return mErr
	}); err != nil {
		t.Fatalf("MarkOpen/MarkQueuedForResume under synth claims: %v", err)
	}
	if !parked || !requeued {
		t.Errorf("park/requeue = (%v, %v), want (true, true)", parked, requeued)
	}
	// Back to running for the terminal writes below (the claim path is
	// admin-side; here we only need the status precondition).
	if _, err := h.AdminDB.Exec(`UPDATE conversations SET status = 'running' WHERE id = $1`, runID); err != nil {
		t.Fatalf("reset status to running: %v", err)
	}

	// Complete twice — two invocation cycles, each going live first (the
	// claim mint) so its streamed row is claim-stamped under RLS, then
	// settling its own cost lump on that row and stamping its telemetry
	// onto the claim it releases; the derived totals then ADD across the
	// cycles.
	settle := func(cost float64, durationMs, numTurns int, stopReason, resultSummary, outcome string) {
		t.Helper()
		if err := stores.Conversations.SetExecutorSystem(ctx, orgID, runID, "exec-lc", 1); err != nil {
			t.Fatalf("SetExecutorSystem: %v", err)
		}
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			_, mErr := tx.Conversations.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: runID, Role: "assistant", Subtype: "text", Content: "work",
			})
			return mErr
		}); err != nil {
			t.Fatalf("InsertMessage under synth claims: %v", err)
		}
		if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(tx db.TxStores) error {
			return tx.Conversations.Complete(ctx, orgID, runID, "completed", cost, durationMs, numTurns, stopReason, resultSummary, outcome, "", "")
		}); err != nil {
			t.Fatalf("Complete under synth claims: %v", err)
		}
	}
	settle(0.5, 1500, 3, "partial", "", "")
	settle(0.25, 500, 2, "end_turn", "ok", "finish")

	// Verify through the derived projection: row landed in completed, the
	// totals reflect both cycles, creator stayed the original user.
	got, err := stores.Conversations.GetSystem(ctx, orgID, runID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem: err=%v got=%v", err, got)
	}
	if got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.TotalCostUSD == nil || *got.TotalCostUSD != 0.75 {
		t.Errorf("total_cost_usd = %v, want 0.75 (0.5 lump + 0.25 lump)", got.TotalCostUSD)
	}
	if got.DurationMs == nil || *got.DurationMs != 2000 {
		t.Errorf("duration_ms = %v, want 2000 (1500 + 500 across claims)", got.DurationMs)
	}
	if got.NumTurns == nil || *got.NumTurns != 5 {
		t.Errorf("num_turns = %v, want 5 (3 + 2 across claims)", got.NumTurns)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", got.StopReason)
	}
	if got.CreatorUserID != userID {
		t.Errorf("creator_user_id = %v, want %s", got.CreatorUserID, userID)
	}

	// MarkFailedIfActive on a terminal row is a no-op (guarded
	// transition). Verifies the System variant's guard fires even
	// though we never wrapped in claims for this call (spawner uses
	// it goroutine-internally with no user identity).
	failed, err := stores.Conversations.MarkFailedIfActiveSystem(ctx, orgID, runID, "")
	if err != nil {
		t.Fatalf("MarkFailedIfActiveSystem: %v", err)
	}
	if failed {
		t.Errorf("MarkFailedIfActiveSystem on terminal row: flipped=true, want false (guard)")
	}
}

// seedPgConversation inserts a conversations row directly — the test
// fixture stand-in for the queue's EnqueueRun mint, staging rows in
// arbitrary status. The trigger_type↔creator CHECK is satisfied by the
// caller: manual rows carry CreatorUserID, event rows leave it empty.
func seedPgConversation(t *testing.T, conn *sql.DB, orgID string, run domain.Conversation) string {
	t.Helper()
	id := run.ID
	if id == "" {
		id = uuid.New().String()
	}
	trigger := run.TriggerType
	if trigger == "" {
		trigger = "manual"
	}
	var stepIdx any
	if run.BlueprintStepIndex != nil {
		stepIdx = *run.BlueprintStepIndex
	}
	var creator any
	if run.CreatorUserID != "" {
		creator = run.CreatorUserID
	}
	if _, err := conn.Exec(`
		INSERT INTO conversations (id, org_id, task_id, team_id, prompt_id, status, model,
		                           trigger_type, trigger_id, visibility, creator_user_id,
		                           blueprint_run_id, blueprint_step_index)
		VALUES ($1, $2, $3,
		        (SELECT id FROM teams WHERE org_id = $2 ORDER BY created_at ASC LIMIT 1),
		        $4, $5, $6, $7, NULLIF($8, '')::uuid, 'team', $9, $10, $11)
	`, id, orgID, run.TaskID, run.PromptID, run.Status, run.Model,
		trigger, run.TriggerID, creator, run.BlueprintRunID, stepIdx); err != nil {
		t.Fatalf("seed conversation %s: %v", id, err)
	}
	return id
}

// seedPgBlueprintRun mints a blueprint + blueprint_run pointed at the
// given task so a standalone conversations insert can satisfy the origin
// CHECK's blueprint_run_id FK (→ blueprint_runs(id)). Mirrors the
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

// TestConversationStore_Postgres_RuntimeDefaultsToSDK pins the
// conversations.runtime schema fact the columnar-canon epic depends on: a
// freshly seeded delegation row lands as 'sdk', not the native loop's
// 'native'. domain.Conversation has no write path for Runtime (nothing writes
// 'native' until the executor-side loop lands), so this reads the column
// directly.
func TestConversationStore_Postgres_RuntimeDefaultsToSDK(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID, _ := seedPgConversationOrg(t, h)
	seedPgConversationPromptIn(t, h, "p_runtime_test", orgID, userID)

	entityID := uuid.New().String()
	eventID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Runtime Test', '', '{}'::jsonb, now())
	`, entityID, orgID, "runtime-"+orgID[:8]); err != nil {
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

	runID := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: "p_runtime_test", Status: "running", Model: "m",
		CreatorUserID:  userID,
		BlueprintRunID: seedPgBlueprintRun(t, h, orgID, userID, taskID),
	})

	var runtime string
	if err := h.AdminDB.QueryRow(`SELECT runtime FROM conversations WHERE id = $1`, runID).Scan(&runtime); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if runtime != "sdk" {
		t.Errorf("runtime = %q, want sdk", runtime)
	}
}

// seedPgConversationPromptIn is a small variant that inserts a prompt
// with an explicit id. Used by cross-org leakage which needs two
// prompts in two orgs with distinct ids.
func seedPgConversationPromptIn(t *testing.T, h *pgtest.Harness, id, orgID, userID string) {
	t.Helper()
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(`
		INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'X-leak Test', 'body', 'user', '', now(), now())
	`, id, orgID, userID, teamID); err != nil {
		t.Fatalf("seed prompt %s: %v", id, err)
	}
}
