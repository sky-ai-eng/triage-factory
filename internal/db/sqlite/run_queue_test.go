package sqlite_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// sqliteRQExecutorID/sqliteRQBootEpoch are the fixed ownership identity most
// claims in this file stamp a row with.
const (
	sqliteRQExecutorID = "sqlite-test-executor"
	sqliteRQBootEpoch  = int64(1)
)

func TestRunQueueStore_SQLite_EnqueueClaim(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-claim")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rq-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rq-bp", "RQ Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rq-bp", []string{"rq-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rq-br", BlueprintID: "rq-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rq",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Empty queue: nothing claimable.
	if got, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on empty queue = (%v, %v), want (nil, nil)", got, err)
	}

	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rq-run-0", TaskID: task.ID, PromptID: "rq-p0", Model: "claude-sonnet-4-6",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Simulate a run that got partway (captured a session) before being
	// re-queued by a crash reconcile, so the claim must carry session_id back
	// for runAgent's resume-on-reclaim path.
	if _, err := conn.Exec(`UPDATE conversations SET sdk_session_id = 'rq-sess' WHERE id = 'rq-run-0'`); err != nil {
		t.Fatalf("set session_id: %v", err)
	}

	got, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("ClaimNextRun: %v", err)
	}
	if got == nil {
		t.Fatal("ClaimNextRun returned nil for a queued run")
	}
	if got.ID != "rq-run-0" || got.BlueprintRunID != brID || got.Status != "running" {
		t.Fatalf("claimed run = %+v", got)
	}
	if got.SessionID != "rq-sess" {
		t.Fatalf("claimed session_id = %q, want rq-sess (resume-on-reclaim plumbing)", got.SessionID)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.Attempts)
	}
	if got.BlueprintStepIndex == nil || *got.BlueprintStepIndex != 0 {
		t.Fatalf("step index = %v, want 0", got.BlueprintStepIndex)
	}
	// team_id rides back on the claim (TFAC-458): the dispatcher stamps
	// cfg.teamID = run.TeamID off the claimed row, which becomes the
	// construction-path RunInfo.TeamID the capture writers attribute by.
	if got.TeamID != runmode.LocalDefaultTeamID {
		t.Fatalf("claimed team_id = %q, want %q", got.TeamID, runmode.LocalDefaultTeamID)
	}

	// Claimed → no longer queued, so a second claim finds nothing.
	if got2, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got2 != nil {
		t.Fatalf("second ClaimNextRun = (%v, %v), want (nil, nil)", got2, err)
	}
}

func TestRunQueueStore_SQLite_CancelRequestedNotClaimed(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-cancel")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqc-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqc-bp", "RQ Cancel Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqc-bp", []string{"rqc-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqc-br", BlueprintID: "rqc-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqc",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqc-run-0", TaskID: task.ID, PromptID: "rqc-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Raise the sequence-cancel signal: the queued step must not be claimed.
	changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID)
	if err != nil || !changed {
		t.Fatalf("RequestRunCancelSystem = (%v, %v), want (true, nil)", changed, err)
	}
	if got, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextRun on cancel-requested blueprint = (%v, %v), want (nil, nil)", got, err)
	}
	// Idempotent: re-requesting on an already-flagged running row reports no change.
	if changed, err := stores.Blueprints.RequestRunCancelSystem(ctx, org, brID); err != nil || changed {
		t.Fatalf("second RequestRunCancelSystem = (%v, %v), want (false, nil)", changed, err)
	}
	// The flag is readable back through GetRun.
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil || !br.CancelRequested {
		t.Fatalf("GetRunSystem cancel_requested = %v (br=%+v, err=%v)", br != nil && br.CancelRequested, br, err)
	}
}

func TestRunQueueStore_SQLite_RequeueAndReset(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-reset")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqr-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqr-bp", "RQ Reset Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqr-bp", []string{"rqr-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqr-br", BlueprintID: "rqr-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqr",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqr-run-0", TaskID: task.ID, PromptID: "rqr-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	// Claim it → running, attempts=1.
	claimed, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextRun: (%v, %v)", claimed, err)
	}

	// RequeueRun puts it back to queued (attempts retained), re-claimable.
	if err := stores.RunQueue.RequeueRun(ctx, org, "rqr-run-0", "transient setup error"); err != nil {
		t.Fatalf("RequeueRun: %v", err)
	}
	reclaimed, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || reclaimed == nil {
		t.Fatalf("re-ClaimNextRun: (%v, %v)", reclaimed, err)
	}
	if reclaimed.Attempts != 2 {
		t.Fatalf("attempts after requeue+reclaim = %d, want 2", reclaimed.Attempts)
	}

	// Now the run is 'running' again (mid-flight). ResetProcessingRuns should
	// flip it back to 'queued'.
	n, err := stores.RunQueue.ResetProcessingRuns(ctx, sqliteRQExecutorID, sqliteRQBootEpoch+1)
	if err != nil {
		t.Fatalf("ResetProcessingRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetProcessingRuns reset %d rows, want 1", n)
	}
	afterReset, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{})
	if err != nil || afterReset == nil {
		t.Fatalf("ClaimNextRun after reset: (%v, %v)", afterReset, err)
	}
	if afterReset.Attempts != 3 {
		t.Fatalf("attempts after reset+reclaim = %d, want 3 (reset retains attempts)", afterReset.Attempts)
	}
}

// TestRunQueueStore_SQLite_RequeueFromSetupStatus is the regression for a run
// stranded in `cloning`: the claim sets `running`, but the dispatcher then
// advances the row through granular setup statuses (initializing → cloning →
// fetching → …) before the agent runs, so a workspace-setup failure requeues
// from one of those — not from `running`. A guard matching only `running` left
// the UPDATE hitting zero rows, so the run sat active-but-idle forever ("awaiting
// agent"). RequeueRun must requeue from every mid-setup status.
func TestRunQueueStore_SQLite_RequeueFromSetupStatus(t *testing.T) {
	for _, setupStatus := range []string{"initializing", "cloning", "fetching", "worktree_created", "agent_starting"} {
		t.Run(setupStatus, func(t *testing.T) {
			conn := openSQLiteForTest(t)
			stores := sqlitestore.New(conn)
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID

			task := seedEntityEventTask(t, conn, "rq-setup-"+setupStatus)
			insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqs-p0", Name: "Step 0", Body: "b", Source: "user"})
			insertBlueprintForTest(t, conn, "rqs-bp", "RQ Setup Blueprint")
			if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqs-bp", []string{"rqs-p0"}, nil); err != nil {
				t.Fatalf("ReplaceSteps: %v", err)
			}
			brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
				ID: "rqs-br", BlueprintID: "rqs-bp", TaskID: task.ID,
				TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
				WorktreePath: "/tmp/wt-rqs",
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			step0 := 0
			if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
				ID: "rqs-run-0", TaskID: task.ID, PromptID: "rqs-p0", Model: "m",
				TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
			}); err != nil {
				t.Fatalf("EnqueueRun: %v", err)
			}
			if claimed, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || claimed == nil {
				t.Fatalf("ClaimNextRun: (%v, %v)", claimed, err)
			}
			// Advance to the granular setup status the dispatcher would have set
			// before the workspace-setup failure fired the requeue.
			if _, err := conn.ExecContext(ctx, `UPDATE conversations SET status = ? WHERE id = 'rqs-run-0'`, setupStatus); err != nil {
				t.Fatalf("advance to %s: %v", setupStatus, err)
			}

			if err := stores.RunQueue.RequeueRun(ctx, org, "rqs-run-0", "workspace setup: boom"); err != nil {
				t.Fatalf("RequeueRun: %v", err)
			}
			after, err := stores.AgentRuns.GetSystem(ctx, org, "rqs-run-0")
			if err != nil || after == nil {
				t.Fatalf("GetSystem after requeue: (%v, %v)", after, err)
			}
			if after.Status != "queued" {
				t.Fatalf("status after requeue from %q = %q, want queued (requeue must not no-op on a mid-setup status)", setupStatus, after.Status)
			}
			if reclaimed, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || reclaimed == nil {
				t.Fatalf("re-ClaimNextRun after requeue from %q: (%v, %v)", setupStatus, reclaimed, err)
			}
		})
	}
}

func TestRunQueueStore_SQLite_ResetLeavesDormantAlone(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-dormant")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqd-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqd-bp", "RQ Dormant Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqd-bp", []string{"rqd-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqd-br", BlueprintID: "rqd-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqd",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A parked (dormant) run — directly insert with status open, then stamp
	// it as owned by THIS instance from an EARLIER boot (epoch 0 < the
	// sweep's epoch). Without the stamp the ownership predicate alone would
	// exclude the row and this test would pass even with 'open' dropped
	// from the status exclusion list; with it, only the status list
	// protects the row — which is exactly the invariant being pinned
	// (parked rows stay parked through a self-sweep of prior-boot orphans).
	step0 := 0
	insertConversationForTest(t, conn, domain.AgentRun{
		ID: "rqd-run-0", TaskID: task.ID, PromptID: "rqd-p0", Status: "open",
		Model: "m", TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})
	insertActiveClaimForTest(t, conn, "rqd-run-0", sqliteRQExecutorID, 0)

	n, err := stores.RunQueue.ResetProcessingRuns(ctx, sqliteRQExecutorID, sqliteRQBootEpoch)
	if err != nil {
		t.Fatalf("ResetProcessingRuns: %v", err)
	}
	if n != 0 {
		t.Fatalf("ResetProcessingRuns reset %d rows, want 0 (dormant run must stay parked)", n)
	}
	// And it is not claimable (it's not 'queued').
	if got, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got != nil {
		t.Fatalf("ClaimNextRun = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestRunQueueStore_SQLite_ResetProcessingRuns_ScopedToOwner pins the
// TFAC-578 ownership predicate on the SQLite impl too — SQLite is N=1 (there
// is never a live sibling to protect in practice), but the predicate must
// hold identically to Postgres: a different executor_id is never swept, and
// the same executor/epoch (not yet an earlier boot) is left untouched.
func TestRunQueueStore_SQLite_ResetProcessingRuns_ScopedToOwner(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-scoped")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqso-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqso-bp", "RQ Scoped Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqso-bp", []string{"rqso-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqso-br", BlueprintID: "rqso-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqso",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqso-run-0", TaskID: task.ID, PromptID: "rqso-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if _, err := stores.RunQueue.ClaimNextRun(ctx, "instance-a", 3, db.ClaimPlacement{}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A different executor_id (never happens at real N=1, but the predicate
	// must not match) leaves the row untouched.
	if n, err := stores.RunQueue.ResetProcessingRuns(ctx, "instance-b", 4); err != nil {
		t.Fatalf("ResetProcessingRuns (other executor): %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingRuns for a different executor_id reset %d rows, want 0", n)
	}
	// The same executor at the SAME epoch (not an earlier boot) also leaves
	// it untouched.
	if n, err := stores.RunQueue.ResetProcessingRuns(ctx, "instance-a", 3); err != nil {
		t.Fatalf("ResetProcessingRuns (same epoch): %v", err)
	} else if n != 0 {
		t.Errorf("ResetProcessingRuns at the same epoch reset %d rows, want 0", n)
	}
	// The same executor at a LATER epoch (a restart) sweeps it.
	if n, err := stores.RunQueue.ResetProcessingRuns(ctx, "instance-a", 4); err != nil {
		t.Fatalf("ResetProcessingRuns (restart): %v", err)
	} else if n != 1 {
		t.Errorf("ResetProcessingRuns on restart reset %d rows, want 1", n)
	}
}

func TestRunQueueStore_SQLite_SetCurrentStep(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-step")
	insertBlueprintForTest(t, conn, "rqs-bp", "RQ Step Blueprint")
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqs-br", BlueprintID: "rqs-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqs",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := stores.Blueprints.SetRunCurrentStepSystem(ctx, org, brID, 3); err != nil {
		t.Fatalf("SetRunCurrentStepSystem: %v", err)
	}
	br, err := stores.Blueprints.GetRunSystem(ctx, org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	if br.CurrentStepIndex != 3 {
		t.Fatalf("CurrentStepIndex = %d, want 3", br.CurrentStepIndex)
	}
}

// TestRunQueueStore_SQLite_EnqueueStampsActorAgent pins the audit gap fix:
// EnqueueRun (the live run-creation path) persists runs.actor_agent_id, and the
// AgentRunStore.Get read projection JOINs agents to surface the bot's display
// name as ActorAgentName for the "Ran as: {name}" UI. A run enqueued with no
// actor reads back with both fields empty (the column's nullable contract).
func TestRunQueueStore_SQLite_EnqueueStampsActorAgent(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// One org agent backs runs.actor_agent_id (FK) and carries the display name
	// the read JOIN denormalizes.
	agentID, err := stores.Agents.Create(ctx, org, domain.Agent{DisplayName: "Triage Bot"})
	if err != nil {
		t.Fatalf("Agents.Create: %v", err)
	}

	task := seedEntityEventTask(t, conn, "rq-actor")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqa-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rqa-bp", "RQ Actor Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqa-bp", []string{"rqa-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rqa-br", BlueprintID: "rqa-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rqa",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqa-run-0", TaskID: task.ID, PromptID: "rqa-p0", Model: "m",
		TriggerType: "manual", ActorAgentID: agentID,
		BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	got, err := stores.AgentRuns.Get(ctx, org, "rqa-run-0")
	if err != nil || got == nil {
		t.Fatalf("AgentRuns.Get: (%v, %v)", got, err)
	}
	if got.ActorAgentID != agentID {
		t.Errorf("ActorAgentID = %q, want %q (EnqueueRun must persist the actor)", got.ActorAgentID, agentID)
	}
	if got.ActorAgentName != "Triage Bot" {
		t.Errorf("ActorAgentName = %q, want %q (read JOIN must denormalize agents.display_name)", got.ActorAgentName, "Triage Bot")
	}

	// A run with no actor reads back with both fields empty — the nullable
	// column + LEFT JOIN degrade to "" rather than erroring.
	step1 := 1
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rqa-run-1", TaskID: task.ID, PromptID: "rqa-p0", Model: "m",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step1,
	}); err != nil {
		t.Fatalf("EnqueueRun (no actor): %v", err)
	}
	noActor, err := stores.AgentRuns.Get(ctx, org, "rqa-run-1")
	if err != nil || noActor == nil {
		t.Fatalf("AgentRuns.Get (no actor): (%v, %v)", noActor, err)
	}
	if noActor.ActorAgentID != "" || noActor.ActorAgentName != "" {
		t.Errorf("no-actor run = (id %q, name %q), want both empty", noActor.ActorAgentID, noActor.ActorAgentName)
	}
}

// TestRunQueueStore_SQLite_Credentials runs the shared awaiting-credentials
// pubkey conformance suite against the SQLite impl. Each factory call opens
// a fresh in-memory DB so subtests don't share state.
func TestRunQueueStore_SQLite_Credentials(t *testing.T) {
	dbtest.RunRunQueueCredentialsConformance(t, func(t *testing.T) (db.RunQueueStore, string, dbtest.RunQueueCredentialsSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		task := seedEntityEventTask(t, conn, "rq-cred")
		insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqcr-p0", Name: "Step 0", Body: "b", Source: "user"})
		insertBlueprintForTest(t, conn, "rqcr-bp", "RQ Cred Blueprint")
		if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqcr-bp", []string{"rqcr-p0"}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
			ID: "rqcr-br", BlueprintID: "rqcr-bp", TaskID: task.ID,
			TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
			WorktreePath: "/tmp/wt-rqcr",
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		nextStep := 0
		seed := dbtest.RunQueueCredentialsSeeder{
			EnqueueRun: func(t *testing.T) string {
				t.Helper()
				idx := nextStep
				nextStep++
				runID := uuid.New().String()
				if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
					ID: runID, TaskID: task.ID, PromptID: "rqcr-p0", Model: "m",
					TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueRun: %v", err)
				}
				return runID
			},
			ForceStatus: func(t *testing.T, runID, status string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, runID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
		}
		return stores.RunQueue, org, seed
	})
}

// TestRunQueueStore_SQLite_FleetQueueShares runs the shared per-org queue-share
// conformance against the SQLite impl (N=1 — one local org).
func TestRunQueueStore_SQLite_FleetQueueShares(t *testing.T) {
	dbtest.RunFleetQueueSharesConformance(t, func(t *testing.T) (db.RunQueueStore, string, dbtest.FleetQueueSharesSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		ctx := context.Background()
		org := runmode.LocalDefaultOrgID

		task := seedEntityEventTask(t, conn, "rq-fqs")
		insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rqfqs-p0", Name: "Step 0", Body: "b", Source: "user"})
		insertBlueprintForTest(t, conn, "rqfqs-bp", "RQ FQS Blueprint")
		if err := stores.Blueprints.ReplaceSteps(ctx, org, "rqfqs-bp", []string{"rqfqs-p0"}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
			ID: "rqfqs-br", BlueprintID: "rqfqs-bp", TaskID: task.ID,
			TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
			WorktreePath: "/tmp/wt-rqfqs",
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		nextStep := 0
		seed := dbtest.FleetQueueSharesSeeder{
			EnqueueRun: func(t *testing.T) string {
				t.Helper()
				idx := nextStep
				nextStep++
				runID := uuid.New().String()
				if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
					ID: runID, TaskID: task.ID, PromptID: "rqfqs-p0", Model: "m",
					TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &idx,
				}); err != nil {
					t.Fatalf("EnqueueRun: %v", err)
				}
				return runID
			},
			ForceStatus: func(t *testing.T, runID, status string) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, runID); err != nil {
					t.Fatalf("force status %q: %v", status, err)
				}
			},
			SetMaxConcurrentRuns: func(t *testing.T, cap *int) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO org_settings (org_id, max_concurrent_runs) VALUES (?, ?)
					ON CONFLICT(org_id) DO UPDATE SET max_concurrent_runs = excluded.max_concurrent_runs
				`, org, cap); err != nil {
					t.Fatalf("set max_concurrent_runs: %v", err)
				}
			},
		}
		return stores.RunQueue, org, seed
	})
}

func TestRunQueueStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const bogusOrg = "11111111-1111-1111-1111-111111111111"

	if err := stores.RunQueue.EnqueueRun(ctx, bogusOrg, domain.AgentRun{}); err == nil {
		t.Errorf("EnqueueRun with non-local orgID should error")
	}
	if err := stores.RunQueue.RequeueRun(ctx, bogusOrg, "r", "x"); err == nil {
		t.Errorf("RequeueRun with non-local orgID should error")
	}
}

// TestRunQueueStore_SQLite_QueuedAtStamps pins the queue-dwell timestamps the
// UI's queue timer reads: enqueue stamps queued_at, a claim mints the claims
// row whose claimed_at Get derives, and a requeue re-stamps queued_at while
// releasing the claim — history is kept, ownership is not.
func TestRunQueueStore_SQLite_QueuedAtStamps(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "rq-dwell")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "rq-dwell-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "rq-dwell-bp", "RQ Dwell Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "rq-dwell-bp", []string{"rq-dwell-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rq-dwell-br", BlueprintID: "rq-dwell-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-rq-dwell",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
		ID: "rq-dwell-run", TaskID: task.ID, PromptID: "rq-dwell-p0", Model: "claude-sonnet-4-6",
		TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	queued, err := stores.AgentRuns.Get(ctx, org, "rq-dwell-run")
	if err != nil || queued == nil {
		t.Fatalf("Get after enqueue: (%v, %v)", queued, err)
	}
	if queued.QueuedAt == nil {
		t.Fatal("QueuedAt = nil after enqueue; the enqueue must stamp queue entry")
	}
	if queued.ClaimedAt != nil {
		t.Fatalf("ClaimedAt = %v on a queued run, want nil", queued.ClaimedAt)
	}
	firstQueuedAt := *queued.QueuedAt

	if got, err := stores.RunQueue.ClaimNextRun(ctx, sqliteRQExecutorID, sqliteRQBootEpoch, db.ClaimPlacement{}); err != nil || got == nil {
		t.Fatalf("ClaimNextRun: (%v, %v)", got, err)
	}
	claimed, err := stores.AgentRuns.Get(ctx, org, "rq-dwell-run")
	if err != nil || claimed == nil {
		t.Fatalf("Get after claim: (%v, %v)", claimed, err)
	}
	if claimed.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after claim")
	}
	if claimed.ClaimedAt.Before(firstQueuedAt) {
		t.Fatalf("ClaimedAt %v precedes QueuedAt %v", claimed.ClaimedAt, firstQueuedAt)
	}

	if err := stores.RunQueue.RequeueRun(ctx, org, "rq-dwell-run", "transient setup error"); err != nil {
		t.Fatalf("RequeueRun: %v", err)
	}
	requeued, err := stores.AgentRuns.Get(ctx, org, "rq-dwell-run")
	if err != nil || requeued == nil {
		t.Fatalf("Get after requeue: (%v, %v)", requeued, err)
	}
	if requeued.QueuedAt == nil || requeued.QueuedAt.Before(firstQueuedAt) {
		t.Fatalf("QueuedAt after requeue = %v, want re-stamped at/after the first stamp %v", requeued.QueuedAt, firstQueuedAt)
	}
	// The claims model keeps engagement history: ClaimedAt stays the released
	// claim's stamp, but the row reads unowned (no active claim).
	if requeued.ClaimedAt == nil {
		t.Fatal("ClaimedAt = nil after requeue, want the released engagement's stamp retained")
	}
	if requeued.ExecutorID != "" {
		t.Fatalf("ExecutorID = %q after requeue, want empty (a queued row has no active claim)", requeued.ExecutorID)
	}
}
