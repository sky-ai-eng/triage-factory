package delegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
)

// fleetFixture mints a full org/team/task/prompt/blueprint chain and one
// EnqueueRun'd runs row against real Postgres — the two-Spawner harness
// TFAC-586 calls for ("the reaper needs it anyway"): boot-overlap,
// reaper-requeue, fence, and drain scenarios all start from the same
// realistic shape, then diverge in how they manipulate/observe it.
type fleetFixture struct {
	stores                 db.Stores
	orgID, userID, teamID  string
	taskID, promptID, brID string
	runID                  string
}

func seedFleetFixture(t *testing.T, h *pgtest.Harness) fleetFixture {
	t.Helper()
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "fleet-"+uuid.New().String()[:8])

	blueprintID := "fleet-bp-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'user', now(), now())
	`, blueprintID, orgID, teamID, userID)

	promptID := "fleet-p-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'body', 'user', '[]'::jsonb, now(), now())
	`, promptID, orgID, teamID, userID)

	entityID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Fleet Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "fleet-test-"+entityID[:8])

	eventID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed)

	taskID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, dedup_key, primary_event_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, 'queued', now())
	`, taskID, orgID, userID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID)

	blueprintRunID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprint_runs (id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, status, worktree_path, started_at, step_plan)
		VALUES ($1, $2, $3, $4, $5, 'manual', 'running', $6, now(), '[]')
	`, blueprintRunID, orgID, userID, blueprintID, taskID, "/tmp/wt-"+blueprintRunID)

	runID := uuid.New().String()
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: blueprintRunID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	return fleetFixture{
		stores: stores, orgID: orgID, userID: userID, teamID: teamID,
		taskID: taskID, promptID: promptID, brID: blueprintRunID, runID: runID,
	}
}

// newFleetSpawner builds a real *Spawner against the fixture's Postgres
// stores, stamped with its own persistent instance identity — one call per
// "process" a scenario needs. Every scenario in this file needs at least
// two of these against the SAME Postgres, which is the point: this is the
// two-Spawner-against-one-Postgres harness TFAC-586 calls for.
func newFleetSpawner(t *testing.T, h *pgtest.Harness, fx fleetFixture, executorID string) *Spawner {
	t.Helper()
	s := NewSpawner(h.AdminDB, fx.stores, nil, nil, "")
	epoch, err := fx.stores.Instances.Register(context.Background(), executorID, domain.InstanceRoleExecutor, "v1", "")
	if err != nil {
		t.Fatalf("register %s: %v", executorID, err)
	}
	s.SetExecutorID(executorID, epoch)
	return s
}

func backdateFleetHeartbeat(t *testing.T, h *pgtest.Harness, executorID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE instances SET last_heartbeat_at = now() - $2::interval WHERE id = $1`,
		executorID, age.String())
}

// TestFleet_BootOverlap_TwoInstancesRegisterConcurrently pins the P0
// substrate this whole ticket sits on top of: two "processes" racing
// Register against the same Postgres converge on two distinct rows with
// independent, correctly-minted epochs — never a lost update, never a
// shared identity. The rolling-deploy sim's baseline case.
func TestFleet_BootOverlap_TwoInstancesRegisterConcurrently(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	fx := seedFleetFixture(t, h)

	const idA, idB = "fleet-boot-a", "fleet-boot-b"
	var wg sync.WaitGroup
	var epochA, epochB int64
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		epochA, errA = fx.stores.Instances.Register(context.Background(), idA, domain.InstanceRoleExecutor, "v1", "")
	}()
	go func() {
		defer wg.Done()
		epochB, errB = fx.stores.Instances.Register(context.Background(), idB, domain.InstanceRoleExecutor, "v1", "")
	}()
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("concurrent Register: errA=%v errB=%v", errA, errB)
	}
	if epochA != 1 || epochB != 1 {
		t.Fatalf("first-boot epochs = (%d, %d), want (1, 1) — each id is its own row", epochA, epochB)
	}

	sA := newFleetSpawner(t, h, fx, idA)
	sB := newFleetSpawner(t, h, fx, idB)
	// A second Register through newFleetSpawner bumped each id's epoch to
	// 2 — restart semantics, not a collision between A and B.
	idOf, epochOf := sA.executorIdentity()
	if idOf != idA || epochOf != 2 {
		t.Errorf("spawner A identity = (%q, %d), want (%q, 2)", idOf, epochOf, idA)
	}
	idOf, epochOf = sB.executorIdentity()
	if idOf != idB || epochOf != 2 {
		t.Errorf("spawner B identity = (%q, %d), want (%q, 2)", idOf, epochOf, idB)
	}
}

// TestFleet_ReaperRequeue_DeadExecutorRunClaimedBySurvivor is the ticket's
// headline acceptance criterion end to end: instance A claims the fixture's
// run, goes silent (heartbeat backdated past the threshold — standing in
// for `kill -9`), the leader reaper requeues its row, and instance B — a
// SEPARATE Spawner against the SAME Postgres — successfully claims and
// would complete it. Exactly one live owner at a time; the dead owner's
// claim never resurfaces.
func TestFleet_ReaperRequeue_DeadExecutorRunClaimedBySurvivor(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	fx := seedFleetFixture(t, h)
	ctx := context.Background()

	const idA, idB = "fleet-reap-a", "fleet-reap-b"
	sA := newFleetSpawner(t, h, fx, idA)
	sB := newFleetSpawner(t, h, fx, idB)

	execA, epochA := sA.executorIdentity()
	claimed, err := fx.stores.RunQueue.ClaimNextRun(ctx, execA, epochA, db.ClaimPlacement{})
	if err != nil || claimed == nil || claimed.ID != fx.runID {
		t.Fatalf("A claims: claimed=%v err=%v", claimed, err)
	}

	// A goes dark: kill -9 stops its heartbeat loop entirely, so its
	// registry row simply stops advancing.
	backdateFleetHeartbeat(t, h, idA, time.Hour)

	reap := reaper.NewPostgresStore(h.AdminDB)
	counts, err := reap.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 1 {
		t.Fatalf("counts = %+v, want {Requeued:1}", counts)
	}

	execB, epochB := sB.executorIdentity()
	claimedByB, err := fx.stores.RunQueue.ClaimNextRun(ctx, execB, epochB, db.ClaimPlacement{})
	if err != nil || claimedByB == nil || claimedByB.ID != fx.runID {
		t.Fatalf("B claims after reap: claimed=%v err=%v", claimedByB, err)
	}
	if claimedByB.Attempts != 2 {
		t.Errorf("attempts after A's claim + reap-requeue + B's claim = %d, want 2 (the reaper itself must not bump attempts — only claims do)", claimedByB.Attempts)
	}

	var executorID string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT executor_id FROM runs WHERE id = $1`, fx.runID).Scan(&executorID); err != nil {
		t.Fatalf("read back owner: %v", err)
	}
	if executorID != execB {
		t.Errorf("runs.executor_id = %q, want B's id %q — A's dead claim must never resurface", executorID, execB)
	}
}

// TestFleet_Fence_SupersededInstanceStopsClaimingAndKillsSandboxes pins the
// split-identity fence end to end against real Postgres: a duplicated
// state root (simulated by re-registering A's id — a distinct "process"
// booting from a copy of A's volume) makes A's own heartbeat observe
// supersession, latch IdentityFenced, kill A's live sandboxes, and invoke
// the exit hook — while a wholly separate instance B is completely
// unaffected and can still claim work.
func TestFleet_Fence_SupersededInstanceStopsClaimingAndKillsSandboxes(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	fx := seedFleetFixture(t, h)
	ctx := context.Background()

	const idA, idB = "fleet-fence-a", "fleet-fence-b"
	sA := newFleetSpawner(t, h, fx, idA)
	sB := newFleetSpawner(t, h, fx, idB)

	killed := false
	sA.mu.Lock()
	sA.cancels["run-live"] = func() { killed = true }
	sA.mu.Unlock()
	var exited bool
	sA.SetOnSupersessionFence(func() { exited = true })

	// A duplicated state root boots a second copy under A's own id — the
	// registry has no way to know it isn't A restarting normally.
	if _, err := fx.stores.Instances.Register(ctx, idA, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("duplicate register of A's id: %v", err)
	}

	if sA.heartbeatOnce(ctx) {
		t.Fatal("A's heartbeat must observe supersession and stop the loop")
	}
	if !sA.IdentityFenced() {
		t.Fatal("A must be identity-fenced")
	}
	if !killed {
		t.Error("A's live sandbox must be killed on fence completion")
	}
	if !exited {
		t.Error("A's supersession exit hook must fire")
	}

	// B is a completely different identity — untouched, and still able to
	// claim the fixture's run.
	if sB.IdentityFenced() {
		t.Fatal("B must not be affected by A's supersession")
	}
	execB, epochB := sB.executorIdentity()
	claimed, err := fx.stores.RunQueue.ClaimNextRun(ctx, execB, epochB, db.ClaimPlacement{})
	if err != nil || claimed == nil {
		t.Fatalf("B claims after A's fence: claimed=%v err=%v", claimed, err)
	}
}

// TestFleet_Drain_DrainedInstanceStopsClaimingSurvivorDoesNot pins the
// drain scenario end to end: instance A is drained via the same store
// write the CLI verb performs; A's own heartbeat reads the flag back and
// its dispatcher claim loop stops claiming, while sibling instance B —
// never drained — keeps claiming normally against the same Postgres.
func TestFleet_Drain_DrainedInstanceStopsClaimingSurvivorDoesNot(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	fx := seedFleetFixture(t, h)
	ctx := context.Background()

	const idA, idB = "fleet-drain-a", "fleet-drain-b"
	sA := newFleetSpawner(t, h, fx, idA)
	sB := newFleetSpawner(t, h, fx, idB)

	if matched, err := fx.stores.Instances.SetDraining(ctx, idA, true); err != nil || !matched {
		t.Fatalf("SetDraining(A, true): matched=%v err=%v", matched, err)
	}

	if !sA.heartbeatOnce(ctx) {
		t.Fatal("a drained instance's heartbeat must keep the loop running")
	}
	if !sA.Draining() {
		t.Fatal("A must read back draining=true from its own heartbeat")
	}

	sA.drainRunQueue(ctx)
	var status string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, fx.runID).Scan(&status); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status = %q after A's (drained) drainRunQueue, want queued — A must not claim", status)
	}

	// B was never drained and claims normally.
	sB.drainRunQueue(ctx)
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, fx.runID).Scan(&status); err != nil {
		t.Fatalf("read back run after B's drainRunQueue: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q after B's drainRunQueue, want running — an undrained sibling must claim normally", status)
	}
}
