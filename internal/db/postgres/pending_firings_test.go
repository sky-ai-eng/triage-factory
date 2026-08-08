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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPendingFiringsStore_Postgres runs the shared conformance suite
// against the Postgres PendingFiringsStore impl. Wires both pools
// against AdminDB (BYPASSRLS) so behavior tests stay independent of
// the auth path; the cross-org leakage test below exercises the
// org_id filter directly.
func TestPendingFiringsStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunPendingFiringsStoreConformance(t, func(t *testing.T) (db.PendingFiringsStore, string, dbtest.PendingFiringsSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
		return stores.PendingFirings, orgID, newPgPendingFiringsSeeder(h, orgID, userID, agentID)
	})
}

// TestPendingFiringsStore_Postgres_CrossOrgLeakage pins the defense-
// in-depth org_id filter on every read + mutation path. The
// pending_firings_all RLS policy gates via an EXISTS-against-tasks
// subquery — the org_id = $N clause in each query is the belt to
// RLS's suspenders.
func TestPendingFiringsStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, userA, agentA := seedPgPendingFiringsOrg(t, h)
	seedA := newPgPendingFiringsSeeder(h, orgA, userA, agentA)
	tupA := seedA.Tuple(t)

	orgB, userB, agentB := seedPgPendingFiringsOrg(t, h)
	seedB := newPgPendingFiringsSeeder(h, orgB, userB, agentB)
	tupB := seedB.Tuple(t)

	// Seed a pending firing in orgA only.
	if _, _, err := stores.PendingFirings.Enqueue(ctx, orgA, userA, tupA.EntityID, tupA.TaskID, tupA.TriggerID, tupA.EventID, db.AgentClaimStamp{}); err != nil {
		t.Fatalf("Enqueue orgA: %v", err)
	}

	// PopForTask scoped to orgB must NOT see orgA's row.
	if got, err := stores.PendingFirings.PopForTask(ctx, orgB, tupA.TaskID); err != nil {
		t.Fatalf("PopForTask cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Pop returned orgA firing %d", got.ID)
	}

	// HasPendingForTask scoped to orgB must be false.
	if has, _ := stores.PendingFirings.HasPendingForTask(ctx, orgB, tupA.TaskID); has {
		t.Errorf("orgB HasPendingForTask returned true for orgA's task")
	}

	// ListForEntity scoped to orgB must be empty.
	if rows, _ := stores.PendingFirings.ListForEntity(ctx, orgB, tupA.EntityID); len(rows) != 0 {
		t.Errorf("orgB ListForEntity returned %d rows for orgA's entity", len(rows))
	}

	// ListTasksWithPending scoped to orgB must be empty (despite
	// orgA having pending rows). orgB has its own clean task (tupB)
	// so the test exercises the org_id filter, not the empty-table
	// path.
	if ids, _ := stores.PendingFirings.ListTasksWithPending(ctx, orgB); len(ids) != 0 {
		t.Errorf("orgB ListTasksWithPending = %v, want empty", ids)
	}

	// MarkFired/MarkSkipped cross-org must NOT mutate orgA's row.
	// Read orgA's firing id first.
	rowsA, _ := stores.PendingFirings.ListForEntity(ctx, orgA, tupA.EntityID)
	if len(rowsA) != 1 {
		t.Fatalf("expected one orgA firing, got %d", len(rowsA))
	}
	firingID := rowsA[0].ID
	runIDInOrgB := seedB.RunForTask(t, tupB.TaskID)
	if err := stores.PendingFirings.MarkFired(ctx, orgB, firingID, runIDInOrgB); err != nil {
		t.Fatalf("MarkFired cross-org: %v", err)
	}
	if err := stores.PendingFirings.MarkSkipped(ctx, orgB, firingID, "hack"); err != nil {
		t.Fatalf("MarkSkipped cross-org: %v", err)
	}
	rowsAAfter, _ := stores.PendingFirings.ListForEntity(ctx, orgA, tupA.EntityID)
	if rowsAAfter[0].Status != "pending" {
		t.Errorf("orgA's firing was mutated by cross-org Mark*: status=%q, want pending", rowsAAfter[0].Status)
	}
}

// TestPendingFiringsStore_Postgres_EnqueueWithLocalSentinelUser is the
// regression test for the SQLite-only LocalDefaultUserID sentinel
// leaking into Postgres. The router still passes runmode.LocalDefault
// UserID until D9 retrofits handler-level claims; binding it directly
// would trip pending_firings_creator_user_id_fkey because that uuid
// has no row in the multi-mode users table. The store normalizes the
// sentinel to empty so the COALESCE walks to org-owner.
func TestPendingFiringsStore_Postgres_EnqueueWithLocalSentinelUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, ownerUserID, agentID := seedPgPendingFiringsOrg(t, h)
	tup := newPgPendingFiringsSeeder(h, orgID, ownerUserID, agentID).Tuple(t)

	// Caller passes the SQLite-only sentinel rather than ownerUserID.
	inserted, _, err := stores.PendingFirings.Enqueue(ctx, orgID, runmode.LocalDefaultUserID,
		tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{})
	if err != nil {
		t.Fatalf("Enqueue with LocalDefaultUserID sentinel: %v (FK would trip if sentinel weren't filtered)", err)
	}
	if !inserted {
		t.Fatal("Enqueue should report inserted=true on fresh row")
	}

	// Verify creator_user_id resolved to the org owner, not the sentinel.
	var creator string
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id FROM pending_firings WHERE task_id = $1 AND trigger_id = $2`,
		tup.TaskID, tup.TriggerID,
	).Scan(&creator); err != nil {
		t.Fatalf("read creator_user_id: %v", err)
	}
	if creator != ownerUserID {
		t.Errorf("creator_user_id = %q, want org owner %q (sentinel should fall through COALESCE to org-owner)",
			creator, ownerUserID)
	}
}

// TestPendingFiringsStore_Postgres_ConcurrentPopNeverDoublePops is the
// Concurrently draining a task from several goroutines (simulating separate
// DrainTask calls — different processes, or a leader-failover overlap within
// one) must never hand the same pending_firings row to two callers. The claiming pop (UPDATE
// ... FOR UPDATE SKIP LOCKED ... RETURNING) is what makes this safe; a bare
// SELECT would let two concurrent drains observe and each act on the same
// row.
func TestPendingFiringsStore_Postgres_ConcurrentPopNeverDoublePops(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
	seed := newPgPendingFiringsSeeder(h, orgID, userID, agentID)

	// Every row on ONE task (distinct triggers), since the queue the drain
	// pops from is the task's.
	const rows = 6
	var entityID, taskID string
	for i := 0; i < rows; i++ {
		tup := seed.Tuple(t)
		if i == 0 {
			entityID, taskID = tup.EntityID, tup.TaskID
		}
		if _, _, err := stores.PendingFirings.Enqueue(ctx, orgID, tup.UserID, entityID, taskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// More concurrent poppers than rows, so every row gets claimed and the
	// excess poppers reliably observe an empty queue (got == nil) rather
	// than the test being sensitive to goroutine scheduling luck.
	const poppers = 12
	var wg sync.WaitGroup
	claimed := make([]int64, poppers)
	gotErr := make([]error, poppers)
	var ready sync.WaitGroup
	ready.Add(poppers)
	start := make(chan struct{})
	for i := 0; i < poppers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			row, err := stores.PendingFirings.PopForTask(ctx, orgID, taskID)
			if err != nil {
				gotErr[i] = err
				return
			}
			if row == nil {
				claimed[i] = 0
				return
			}
			claimed[i] = row.ID
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	seen := map[int64]int{}
	for i, id := range claimed {
		if gotErr[i] != nil {
			t.Fatalf("popper %d: %v", i, gotErr[i])
		}
		if id != 0 {
			seen[id]++
		}
	}
	if len(seen) != rows {
		t.Fatalf("expected %d distinct rows claimed, got %d (claimed=%v)", rows, len(seen), claimed)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("firing %d was popped by %d poppers, want exactly 1 (double-pop)", id, n)
		}
	}

	// Every claimed row must have been atomically flipped to 'draining' —
	// not left 'pending' (which would mean the pop wasn't a real claim).
	all, err := stores.PendingFirings.ListForEntity(ctx, orgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	draining := 0
	for _, r := range all {
		if r.Status == "draining" {
			draining++
		}
	}
	if draining != rows {
		t.Errorf("expected all %d rows draining after the claim race, got %d", rows, draining)
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

// newPgPendingFiringsSeeder builds the seeder bag against AdminDB so
// raw inserts bypass RLS. Every Tuple call creates a fresh chain
// (entity → prompt → event → task → event_handler[trigger]) so dedup
// keys stay distinct across subtests.
func newPgPendingFiringsSeeder(h *pgtest.Harness, orgID, userID, agentID string) dbtest.PendingFiringsSeeder {
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
			UserID:    userID,
		}
	}

	// runForTask inserts a blueprint + blueprint_run row so MarkFired's
	// fired_run_id FK to blueprint_runs(id) is satisfied — the firing unit is
	// the blueprint_run now. The conformance suite doesn't probe gate semantics
	// here — those live in ConversationStore's own tests.
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

	claimTaskForUser := func(t *testing.T, taskID string) {
		t.Helper()
		if _, err := conn.Exec(`
			UPDATE tasks SET claimed_by_user_id = $1, claimed_by_agent_id = NULL WHERE id = $2
		`, userID, taskID); err != nil {
			t.Fatalf("claim task for user: %v", err)
		}
	}

	return dbtest.PendingFiringsSeeder{
		Tuple:            tuple,
		RunForTask:       runForTask,
		AgentID:          agentID,
		TaskClaim:        taskClaim,
		ClaimTaskForUser: claimTaskForUser,
	}
}
