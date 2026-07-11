package reaper_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/reaper"
)

// reaperFixture is one claimed run + its owning blueprint_run + the
// executor instance that (nominally) owns it, ready for a test to
// manipulate (backdate the heartbeat, bump attempts, request a cancel)
// before calling ReapDeadExecutors.
type reaperFixture struct {
	runID          string
	blueprintRunID string
	executorID     string
	orgID, userID  string
}

// seedReaperFixture mints an org, a blueprint/task/prompt chain, a running
// blueprint_run, and one claimed run under a freshly-registered executor
// instance. attempts is set via repeated claims (ClaimNextRun bumps
// attempts on every claim — the real production mechanism, not a raw
// UPDATE) so the fixture exercises the same column the reaper reads.
func seedReaperFixture(t *testing.T, h *pgtest.Harness, attempts int) reaperFixture {
	t.Helper()
	ctx := context.Background()
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgID, userID, _ := pgtest.SeedOrgWithUser(t, h, "reaper-"+uuid.New().String()[:8])

	teamID := reaperFirstTeamForOrg(t, h, orgID)
	blueprintID := "reaper-bp-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO blueprints (id, org_id, team_id, creator_user_id, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'user', now(), now())
	`, blueprintID, orgID, teamID, userID)

	promptID := "reaper-p-" + uuid.New().String()[:8]
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO prompts (id, org_id, team_id, creator_user_id, name, body, source, allowed_tools, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $1, 'body', 'user', '[]'::jsonb, now(), now())
	`, promptID, orgID, teamID, userID)

	entityID := uuid.New().String()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO entities (id, org_id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES ($1, $2, 'github', $3, 'pr', 'Reaper Test Entity', 'https://example/x', '{}'::jsonb, now())
	`, entityID, orgID, "reaper-test-"+entityID[:8])

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

	executorID := "reaper-executor-" + uuid.New().String()[:8]
	if _, err := stores.Instances.Register(ctx, executorID, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("register executor: %v", err)
	}

	runID := uuid.New().String()
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, orgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: userID, BlueprintRunID: blueprintRunID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	for i := 0; i < attempts; i++ {
		got, err := stores.RunQueue.ClaimNextRun(ctx, executorID, 1)
		if err != nil || got == nil {
			t.Fatalf("ClaimNextRun (attempt %d): got=%v err=%v", i+1, got, err)
		}
		if i < attempts-1 {
			// A mid-loop requeue simulates the run bouncing back to 'queued'
			// so the NEXT claim can bump attempts again — attempts is a
			// monotonic per-row counter, not "how many times currently
			// claimed".
			if err := stores.RunQueue.RequeueRun(ctx, orgID, runID, "test churn"); err != nil {
				t.Fatalf("RequeueRun (attempt %d): %v", i+1, err)
			}
		}
	}

	return reaperFixture{runID: runID, blueprintRunID: blueprintRunID, executorID: executorID, orgID: orgID, userID: userID}
}

func reaperFirstTeamForOrg(t *testing.T, h *pgtest.Harness, orgID string) string {
	t.Helper()
	var teamID string
	if err := h.AdminDB.QueryRow(`SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at ASC LIMIT 1`, orgID).Scan(&teamID); err != nil {
		t.Fatalf("resolve team for org %s: %v", orgID, err)
	}
	return teamID
}

// backdateHeartbeat pushes an instance's last_heartbeat_at far enough into
// the past that it reads as stale under any threshold this test file uses.
func backdateHeartbeat(t *testing.T, h *pgtest.Harness, executorID string, age time.Duration) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `UPDATE instances SET last_heartbeat_at = now() - $2::interval WHERE id = $1`,
		executorID, age.String())
}

// TestReapDeadExecutors_RequeuesUnderAttemptBudget pins the primary
// recovery path: a claimed run under a stale-heartbeat executor, still
// within its attempt budget, is requeued (ownership stamp cleared,
// attempts left untouched) rather than failed.
func TestReapDeadExecutors_RequeuesUnderAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 1 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Requeued:1}", counts)
	}

	var status string
	var executorID, bootEpoch, attempts any
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status, executor_id, boot_epoch, attempts FROM runs WHERE id = $1`, fx.runID,
	).Scan(&status, &executorID, &bootEpoch, &attempts); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if executorID != nil || bootEpoch != nil {
		t.Errorf("ownership stamp not cleared: executor_id=%v boot_epoch=%v", executorID, bootEpoch)
	}
	if attempts != int64(1) {
		t.Errorf("attempts = %v, want 1 (the reaper's requeue must not bump it — only a re-claim does)", attempts)
	}
}

// TestReapDeadExecutors_TerminalFailsExecutorLostPastAttemptBudget pins the
// attempts-exhausted path: attempts >= maxAttempts terminal-fails the run
// with failure_kind='executor_lost' and finalizes the owning blueprint_run
// failed, instead of requeuing it forever.
func TestReapDeadExecutors_TerminalFailsExecutorLostPastAttemptBudget(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 2) // attempts == maxAttempts below
	backdateHeartbeat(t, h, fx.executorID, time.Hour)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Failed != 1 || counts.Requeued != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want {Failed:1}", counts)
	}

	var status, failureKind string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status, COALESCE(failure_kind, '') FROM runs WHERE id = $1`, fx.runID,
	).Scan(&status, &failureKind); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "failed" || failureKind != string(domain.RunFailureExecutorLost) {
		t.Errorf("run (status=%q failure_kind=%q), want (failed, executor_lost)", status, failureKind)
	}

	var brStatus string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID,
	).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if brStatus != "failed" {
		t.Errorf("blueprint_run status = %q, want failed", brStatus)
	}
}

// TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued pins
// "cancel-requested rows go through the existing cancel finalization
// instead of requeue" — even a run still within its attempt budget must
// finalize cancelled, never come back as queued work.
func TestReapDeadExecutors_CancelRequestedFinalizesCancelledNotRequeued(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, time.Hour)
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET cancel_requested = true WHERE id = $1`, fx.blueprintRunID)

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Cancelled != 1 || counts.Requeued != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want {Cancelled:1}", counts)
	}

	var runStatus, brStatus string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, fx.runID).Scan(&runStatus); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM blueprint_runs WHERE id = $1`, fx.blueprintRunID).Scan(&brStatus); err != nil {
		t.Fatalf("read back blueprint_run: %v", err)
	}
	if runStatus != "cancelled" || brStatus != "cancelled" {
		t.Errorf("(run=%q, blueprint_run=%q), want both cancelled", runStatus, brStatus)
	}
}

// TestReapDeadExecutors_FreshHeartbeatNeverReaped pins the negative case a
// draining executor relies on: a claimed run under an executor whose
// heartbeat is still fresh is left completely untouched, regardless of
// draining — the predicate is heartbeat-staleness only, so "draining is not
// death" holds without the reaper needing to know about drain at all.
func TestReapDeadExecutors_FreshHeartbeatNeverReaped(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	// Simulate an operator draining this executor — still heartbeating,
	// just refusing new claims locally. The reaper must not care.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Instances.SetDraining(ctx, fx.executorID, true); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	counts, err := store.ReapDeadExecutors(ctx, 30*time.Second, 2)
	if err != nil {
		t.Fatalf("ReapDeadExecutors: %v", err)
	}
	if counts.Requeued != 0 || counts.Failed != 0 || counts.Cancelled != 0 {
		t.Fatalf("counts = %+v, want all-zero (fresh heartbeat, draining or not)", counts)
	}

	var status string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = $1`, fx.runID).Scan(&status); err != nil {
		t.Fatalf("read back run: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running (untouched)", status)
	}
}

// TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesRunsExecutorID pins
// the registry GC: only rows past staleAfter are deleted, and a run row
// that referenced the GC'd instance survives with its executor_id intact —
// runs.executor_id carries no FK to instances (verified separately by
// TestRunsExecutorID_HasNoForeignKeyToInstances), so a delete here can never
// cascade into audit history.
func TestDeleteStaleInstances_DeletesOnlyStaleAndPreservesRunsExecutorID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	fx := seedReaperFixture(t, h, 1)
	backdateHeartbeat(t, h, fx.executorID, 8*24*time.Hour) // 8 days stale

	freshID := "reaper-fresh-" + uuid.New().String()[:8]
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	if _, err := stores.Instances.Register(ctx, freshID, domain.InstanceRoleExecutor, "v1", ""); err != nil {
		t.Fatalf("register fresh instance: %v", err)
	}

	store := reaper.NewPostgresStore(h.AdminDB)
	n, err := store.DeleteStaleInstances(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteStaleInstances: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}

	if got, err := stores.Instances.Get(ctx, fx.executorID); err != nil || got != nil {
		t.Fatalf("stale instance still present after GC: got=%v err=%v", got, err)
	}
	if got, err := stores.Instances.Get(ctx, freshID); err != nil || got == nil {
		t.Fatalf("fresh instance was deleted by GC: got=%v err=%v", got, err)
	}

	var executorID string
	if err := h.AdminDB.QueryRowContext(ctx, `SELECT executor_id FROM runs WHERE id = $1`, fx.runID).Scan(&executorID); err != nil {
		t.Fatalf("read back run after GC'd its executor: %v", err)
	}
	if executorID != fx.executorID {
		t.Errorf("runs.executor_id = %q after GC, want it to survive unchanged as %q", executorID, fx.executorID)
	}

	// A GC'd id that comes back alive simply re-registers at boot_epoch 1
	// (the epoch counter has no memory of the deleted row) — harmless per
	// the ticket's decision log.
	epoch, err := stores.Instances.Register(ctx, fx.executorID, domain.InstanceRoleExecutor, "v2", "")
	if err != nil {
		t.Fatalf("re-register a GC'd id: %v", err)
	}
	if epoch != 1 {
		t.Errorf("re-registered epoch = %d, want 1 (fresh row, no memory of the deleted one)", epoch)
	}
}

// TestRunsExecutorID_HasNoForeignKeyToInstances pins the schema invariant
// the GC's safety argument depends on: runs.executor_id is a plain text
// column with no FK constraint into instances, so deleting an instances row
// can never cascade into (or be blocked by) runs.
func TestRunsExecutorID_HasNoForeignKeyToInstances(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	var count int
	if err := h.AdminDB.QueryRow(`
		SELECT COUNT(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
		WHERE tc.table_name = 'runs' AND kcu.column_name = 'executor_id' AND tc.constraint_type = 'FOREIGN KEY'
	`).Scan(&count); err != nil {
		t.Fatalf("query constraints: %v", err)
	}
	if count != 0 {
		t.Fatalf("runs.executor_id has %d foreign key constraint(s), want 0 — a FK here would make GC deletes cascade into (or be blocked by) audit history", count)
	}
}
