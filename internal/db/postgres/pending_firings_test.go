package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestPendingFiringsStore_Postgres runs the shared conformance suite against
// the Postgres PendingFiringsStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path; the
// cross-org leakage test below exercises the org_id filter directly.
func TestPendingFiringsStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunPendingFiringsStoreConformance(t, func(t *testing.T) (db.PendingFiringsStore, string, dbtest.PendingFiringsSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		return stores.PendingFirings, orgID, newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)
	})
}

// TestPendingFiringsStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every org-scoped read and on the receipt-addressed
// writes. The pending_firings_all RLS policy gates via an EXISTS-against-
// tasks subquery — the org_id bound in each statement is the belt to RLS's
// suspenders.
func TestPendingFiringsStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, userA, agentA := seedPgPendingFiringsOrg(t, h)
	seedA := newPgPendingFiringsSeeder(h, stores, orgA, userA, agentA)
	tupA := seedA.Tuple(t)

	orgB, userB, agentB := seedPgPendingFiringsOrg(t, h)
	seedB := newPgPendingFiringsSeeder(h, stores, orgB, userB, agentB)
	tupB := seedB.Tuple(t)

	if _, _, err := stores.PendingFirings.Enqueue(ctx, orgA, tupA.EntityID, tupA.TaskID, tupA.TriggerID, tupA.EventID, db.AgentClaimStamp{}); err != nil {
		t.Fatalf("Enqueue orgA: %v", err)
	}
	if has, _ := stores.PendingFirings.HasUnsettledForTask(ctx, orgB, tupA.TaskID); has {
		t.Errorf("orgB HasUnsettledForTask returned true for orgA's task")
	}
	if rows, _ := stores.PendingFirings.ListForEntity(ctx, orgB, tupA.EntityID); len(rows) != 0 {
		t.Errorf("orgB ListForEntity returned %d rows for orgA's entity", len(rows))
	}

	// A receipt forged with orgB's id over orgA's row matches nothing.
	rowsA, _ := stores.PendingFirings.ListForEntity(ctx, orgA, tupA.EntityID)
	if len(rowsA) != 1 {
		t.Fatalf("expected one orgA firing, got %d", len(rowsA))
	}
	batch, err := stores.PendingFirings.Claim(ctx, workitem.Owner{ID: "w", Epoch: 1}, 10)
	if err != nil || len(batch.Firings) != 1 {
		t.Fatalf("Claim: %+v err=%v", batch, err)
	}
	forged := batch.Firings[0].Receipt
	forged.OrgID = orgB
	runIDInOrgB := seedB.RunForTask(t, tupB.TaskID)
	if err := stores.PendingFirings.MarkFired(ctx, forged, runIDInOrgB); err == nil {
		t.Error("MarkFired with another org's id on the receipt succeeded")
	}
	if err := stores.PendingFirings.MarkSkipped(ctx, forged, "hack"); err == nil {
		t.Error("MarkSkipped with another org's id on the receipt succeeded")
	}
	rowsAAfter, _ := stores.PendingFirings.ListForEntity(ctx, orgA, tupA.EntityID)
	if rowsAAfter[0].Status != workitem.StatusLeased || rowsAAfter[0].SkipReason != "" || rowsAAfter[0].FiredBlueprintRunID != nil {
		t.Errorf("orgA's firing was mutated through a cross-org receipt: %+v", rowsAAfter[0])
	}
	if subjects, err := stores.PendingFirings.(db.WorkKindHandle).Describe(ctx, orgB, []int64{rowsA[0].ID}); err != nil || len(subjects) != 0 {
		t.Errorf("orgB Describe of orgA's row = %+v err=%v, want nothing", subjects, err)
	}
}

// TestPendingFiringsStore_Postgres_ConcurrentClaimsNeverDoubleLease drives
// several claimers at one task's queue at once. The claim's FOR UPDATE SKIP
// LOCKED is what keeps two claimers from leasing the same row; a bare
// SELECT would let both observe and each act on it.
func TestPendingFiringsStore_Postgres_ConcurrentClaimsNeverDoubleLease(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
	seed := newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)

	const rows = 6
	var entityID, taskID string
	for i := 0; i < rows; i++ {
		tup := seed.Tuple(t)
		if i == 0 {
			entityID, taskID = tup.EntityID, tup.TaskID
		}
		if _, _, err := stores.PendingFirings.Enqueue(ctx, orgID, entityID, taskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	const claimers = 12
	var wg sync.WaitGroup
	claimed := make([][]int64, claimers)
	gotErr := make([]error, claimers)
	var ready sync.WaitGroup
	ready.Add(claimers)
	start := make(chan struct{})
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			batch, err := stores.PendingFirings.Claim(ctx, workitem.Owner{ID: fmt.Sprintf("claimer-%d", i), Epoch: 1}, 1)
			if err != nil {
				gotErr[i] = err
				return
			}
			for _, cf := range batch.Firings {
				claimed[i] = append(claimed[i], cf.Firing.ID)
			}
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	seen := map[int64]int{}
	for i, ids := range claimed {
		if gotErr[i] != nil {
			t.Fatalf("claimer %d: %v", i, gotErr[i])
		}
		for _, id := range ids {
			seen[id]++
		}
	}
	if len(seen) != rows {
		t.Fatalf("expected %d distinct rows leased, got %d (%v)", rows, len(seen), claimed)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("firing %d was leased by %d claimers, want exactly 1", id, n)
		}
	}
	all, err := stores.PendingFirings.ListForEntity(ctx, orgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	for _, f := range all {
		if f.Status != workitem.StatusLeased {
			t.Errorf("row %d = %q after the claim race, want leased", f.ID, f.Status)
		}
	}
}

func seedPgPendingFiringsOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("pending-firings-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "PendingFirings Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "PendingFirings Org "+orgID[:8], "pf-"+orgID[:8], userID,
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
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'PendingFirings Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// newPgPendingFiringsSeeder builds the seeder bag against AdminDB so raw
// inserts bypass RLS. Every Tuple call creates a fresh chain (entity →
// prompt → event → task → event_handler[trigger]) so dedup keys stay
// distinct across subtests.
func newPgPendingFiringsSeeder(h *pgtest.Harness, stores db.Stores, orgID, userID, agentID string) dbtest.PendingFiringsSeeder {
	conn := h.AdminDB
	var teamID string
	if err := conn.QueryRow(
		`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID,
	).Scan(&teamID); err != nil {
		panic(fmt.Sprintf("newPgPendingFiringsSeeder: lookup default team for org %s: %v", orgID, err))
	}

	tuple := func(t *testing.T) dbtest.PendingFiringsTuple {
		t.Helper()
		entityID := uuid.New().String()
		eventID := uuid.New().String()
		taskID := uuid.New().String()
		triggerID := uuid.New().String()
		promptID := uuid.New().String()
		blueprintID := uuid.New().String()
		sourceID := fmt.Sprintf("owner/repo#%s", entityID[:8])

		if _, err := conn.Exec(`
			INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES ($1, $2, 'github', $3, 'pr', 'Test PR', '', '{}'::jsonb, now())
		`, entityID, orgID, sourceID); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'PendingFirings Test', 'body', 'user', '', now(), now())
		`, promptID, orgID, userID, teamID); err != nil {
			t.Fatalf("seed prompt: %v", err)
		}
		// The trigger's blueprint_id FKs to blueprints(id, org_id) AND the
		// same-team blueprints(id, team_id), so seed a team-owned blueprint
		// wrapping the prompt above.
		if _, err := conn.Exec(`
			INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'PendingFirings BP', 'user', now(), now())
		`, blueprintID, orgID, userID, teamID); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id)
			VALUES ($1, $2, $3, 0, $4)
		`, orgID, teamID, blueprintID, promptID); err != nil {
			t.Fatalf("seed blueprint step: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES ($1, $2, $3, 'github:pr:ci_check_failed', '', '{}'::jsonb, now())
		`, eventID, orgID, entityID); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO tasks (id, org_id, creator_user_id, team_id, visibility, entity_id, event_type, dedup_key, primary_event_id, status, scoring_status, priority_score)
			VALUES ($1, $2, $3, $4, 'team', $5, 'github:pr:ci_check_failed', '', $6, 'queued', 'pending', 0.5)
		`, taskID, orgID, userID, teamID, entityID, eventID); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		if _, err := conn.Exec(`
			INSERT INTO event_handlers (id, org_id, creator_user_id, team_id, kind, event_type, source, blueprint_id, breaker_threshold, min_autonomy_suitability, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'trigger', 'github:pr:ci_check_failed', 'user', $5, 4, 0, true, now(), now())
		`, triggerID, orgID, userID, teamID, blueprintID); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}

		return dbtest.PendingFiringsTuple{
			EntityID:  entityID,
			TaskID:    taskID,
			TriggerID: triggerID,
			EventID:   eventID,
			PromptID:  promptID,
		}
	}

	// runForTask inserts a blueprint + blueprint_run row so MarkFired's
	// fired_run_id FK to blueprint_runs(id) is satisfied — the firing unit
	// is the blueprint_run.
	runForTask := func(t *testing.T, taskID string) string {
		t.Helper()
		bpID := uuid.New().String()
		if _, err := conn.Exec(`
			INSERT INTO blueprints (id, org_id, creator_user_id, team_id, source, name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'user', 'BP', now(), now())
		`, bpID, orgID, userID, teamID); err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		brID := uuid.New().String()
		if _, err := conn.Exec(`
			INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
			VALUES ($1, $2, $3, $4, $5, 'manual', 'running', '/tmp/wt', now(), '[]')
		`, brID, orgID, userID, bpID, taskID); err != nil {
			t.Fatalf("seed blueprint_run: %v", err)
		}
		return brID
	}

	taskClaim := func(t *testing.T, taskID string) (string, string) {
		t.Helper()
		var agent, user sql.NullString
		if err := conn.QueryRow(
			`SELECT claimed_by_agent_id, claimed_by_user_id FROM tasks WHERE id = $1`, taskID,
		).Scan(&agent, &user); err != nil {
			t.Fatalf("read task claim: %v", err)
		}
		return agent.String, user.String
	}

	// A claim lands the row in progress, the way every claim door does —
	// the tasks_queue_unclaimed CHECK refuses the held-queued row otherwise.
	claimTaskForUser := func(t *testing.T, taskID string) {
		t.Helper()
		if _, err := conn.Exec(`
			UPDATE tasks
			   SET claimed_by_user_id = $1, claimed_by_agent_id = NULL,
			       status = CASE WHEN status IN ('queued', 'snoozed') THEN 'in_progress' ELSE status END
			 WHERE id = $2
		`, userID, taskID); err != nil {
			t.Fatalf("claim task for user: %v", err)
		}
	}

	return dbtest.PendingFiringsSeeder{
		Tuple:      tuple,
		RunForTask: runForTask,
		SettleRuns: func(t *testing.T, taskID string) {
			t.Helper()
			settlePgRuns(t, h, orgID, taskID)
		},
		AgentID:          agentID,
		TaskClaim:        taskClaim,
		ClaimTaskForUser: claimTaskForUser,
		LiveConversation: func(t *testing.T, taskID, promptID string) string {
			t.Helper()
			return seedPgLiveConversation(t, h, orgID, userID, taskID, promptID)
		},
		EndConversation: func(t *testing.T, conversationID string) {
			t.Helper()
			pgExecOne(t, h, "end conversation",
				`UPDATE conversations SET status = 'completed', ended_at = now(), ended_reason = $1 WHERE id = $2`,
				string(domain.EndedStepAdvanced), conversationID)
		},
		ExpireLease: func(t *testing.T, firingID int64) {
			t.Helper()
			// Rewound against the server clock — the one the claim stamped
			// the lease from and the guard compares against.
			pgExecOne(t, h, "expire lease",
				`UPDATE pending_firings SET lease_expires_at = clock_timestamp() - interval '1 hour' WHERE id = $1 AND status = 'leased'`, firingID)
		},
		Ripen: func(t *testing.T, firingID int64) {
			t.Helper()
			pgExecOne(t, h, "ripen", `UPDATE pending_firings SET next_attempt_at = NULL WHERE id = $1 AND status = 'ready'`, firingID)
		},
		InTx: func(t *testing.T, fn func(s db.PendingFiringsStore) error) error {
			t.Helper()
			return stores.Tx.SyntheticClaimsWithTx(context.Background(), orgID, userID, func(tx db.TxStores) error {
				return fn(tx.PendingFirings)
			})
		},
	}
}

// seedPgLiveConversation stages a live top-level conversation on the task:
// a blueprint run of its own (the origin CHECK wants one, and a step
// conversation names its run), no creator, no terminal status, no end. The
// run is settled once the conversation names it, so the conversation is the
// only thing holding the task — the shape a conversation resumed after its
// run completed has, and the one that isolates the conversation half of the
// firing gate from the run half.
func seedPgLiveConversation(t *testing.T, h *pgtest.Harness, orgID, userID, taskID, promptID string) string {
	t.Helper()
	brID := seedPgBlueprintRun(t, h, orgID, userID, taskID)
	stepIdx := 0
	id := seedPgConversation(t, h.AdminDB, orgID, domain.Conversation{
		TaskID: taskID, PromptID: promptID, Model: "m", TriggerType: "event",
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})
	settlePgRuns(t, h, orgID, taskID)
	return id
}

// settlePgRuns marks every running blueprint_run on the task completed.
func settlePgRuns(t *testing.T, h *pgtest.Harness, orgID, taskID string) {
	t.Helper()
	if _, err := h.AdminDB.Exec(`UPDATE blueprint_runs SET status = 'completed' WHERE org_id = $1 AND task_id = $2 AND status = 'running'`, orgID, taskID); err != nil {
		t.Fatalf("settle blueprint_runs: %v", err)
	}
}
