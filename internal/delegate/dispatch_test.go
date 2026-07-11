package delegate

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// reactorFixture stands up a spawner + an N-step blueprint with a running
// blueprint_run on a fresh task, and inserts the step-0 run as the just-finished
// step (status + outcome set by the caller). Returns the spawner, db,
// blueprint_run id, task id, and the step-0 run id.
func reactorFixture(t *testing.T, suffix string, nSteps int, step0Status, step0Outcome string) (*Spawner, *sql.DB, string, string, string) {
	t.Helper()
	database := newDelegateTestDB(t)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "owner/repo#"+suffix, "pr", "T", "https://x/"+suffix)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}

	bpID := "rbp-" + suffix
	if err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	promptIDs := make([]string, nSteps)
	for i := 0; i < nSteps; i++ {
		pid := fmt.Sprintf("%s-p%d", suffix, i)
		ensureTestPrompt(t, database, domain.Prompt{ID: pid, Name: pid, Body: "b", Source: "user"})
		promptIDs[i] = pid
	}
	if err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, promptIDs, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	// Freeze the plan onto the run exactly as the mint path does — the reactor
	// now sequences off br.StepPlan, so an empty plan would fail the advance.
	plan := make([]domain.BlueprintPlanStep, nSteps)
	for i := 0; i < nSteps; i++ {
		plan[i] = domain.BlueprintPlanStep{
			StepIndex: i, PromptID: promptIDs[i], PromptName: promptIDs[i],
			PromptBody: "b", Source: "user",
		}
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rbpr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-" + suffix, StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	run0 := "rrun0-" + suffix
	if err := stores.AgentRuns.Create(ctx, org, domain.AgentRun{
		ID: run0, TaskID: task.ID, PromptID: promptIDs[0], Status: step0Status,
		Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	}); err != nil {
		t.Fatalf("create step0 run: %v", err)
	}
	if _, err := database.Exec(`UPDATE runs SET status = ?, outcome = ? WHERE id = ?`,
		step0Status, sql.NullString{String: step0Outcome, Valid: step0Outcome != ""}, run0); err != nil {
		t.Fatalf("set step0 outcome: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	return s, database, brID, task.ID, run0
}

func queuedStepRuns(t *testing.T, database *sql.DB, brID string) []int {
	t.Helper()
	rows, err := database.Query(`SELECT blueprint_step_index FROM runs WHERE blueprint_run_id = ? AND status = 'queued' ORDER BY blueprint_step_index`, brID)
	if err != nil {
		t.Fatalf("query queued runs: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, idx)
	}
	return out
}

// TestDrainRunQueue_DrainingSkipsClaim pins the drain verb's dispatcher-side
// consequence (TFAC-586): drainRunQueue must not claim a queued row while
// Draining() is true, and must resume claiming the moment it flips back —
// proving the gate, not fixture setup, is what blocked the first attempt.
func TestDrainRunQueue_DrainingSkipsClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	seedRun(t, database, "run-drain-skip", "sess-1", "/tmp/wt-run-drain-skip")
	if _, err := database.Exec(`UPDATE runs SET status = 'queued', claimed_at = NULL, executor_id = NULL, boot_epoch = NULL WHERE id = ?`, "run-drain-skip"); err != nil {
		t.Fatalf("force queued: %v", err)
	}

	s.SetDraining(true)
	s.drainRunQueue(context.Background())

	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = ?`, "run-drain-skip").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued — a draining instance must not claim", status)
	}

	s.SetDraining(false)
	s.drainRunQueue(context.Background())
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = ?`, "run-drain-skip").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "running" {
		t.Errorf("status = %q, want running once undrained", status)
	}
}

// TestDrainRunQueue_PartitionFencedSkipsClaim mirrors the draining test for
// the partition self-fence gate: a fenced instance must not claim either,
// even though (unlike IdentityFenced) the fence isn't sticky.
func TestDrainRunQueue_PartitionFencedSkipsClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	seedRun(t, database, "run-pfence-skip", "sess-1", "/tmp/wt-run-pfence-skip")
	if _, err := database.Exec(`UPDATE runs SET status = 'queued', claimed_at = NULL, executor_id = NULL, boot_epoch = NULL WHERE id = ?`, "run-pfence-skip"); err != nil {
		t.Fatalf("force queued: %v", err)
	}

	s.partitionFenced.Store(true)
	s.drainRunQueue(context.Background())

	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = ?`, "run-pfence-skip").Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued — a partition-fenced instance must not claim", status)
	}
}

// TestReconcileRunQueue_CancelsOrphanUnderTerminalBlueprint is the
// boot-reconcile integration check: a child run left 'running' under an
// already-terminal blueprint_run (the desync) must be cancelled by the boot
// sweep, so the dispatcher stops treating it as live work and its feature
// branch is no longer pinned in a worktree.
func TestReconcileRunQueue_CancelsOrphanUnderTerminalBlueprint(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "recon-orphan", 1, "running", "")

	// Force the desync directly, mimicking a DB broken before the atomic cancel
	// in MarkRunStatus landed: terminal parent, child still running.
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = 0 WHERE id = ?`, brID); err != nil {
		t.Fatalf("force blueprint cancelled: %v", err)
	}

	s.reconcileRunQueue(context.Background())

	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id = ?`, run0).Scan(&status); err != nil {
		t.Fatalf("read child status: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("orphan child status = %q, want cancelled after boot reconcile", status)
	}
}

// TestReactor_AdvanceEnqueuesNextStep: a non-final step that completes with
// 'continue' enqueues the next step and bumps current_step_index, leaving the
// blueprint running.
func TestReactor_AdvanceEnqueuesNextStep(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "adv", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step runs = %v, want [1]", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running", br.Status)
	}
	if br.CurrentStepIndex != 1 {
		t.Errorf("current_step_index = %d, want 1", br.CurrentStepIndex)
	}
}

// TestReactor_AdvanceInheritsActorAgent pins the freeze-and-inherit design: the
// next step the reactor enqueues carries the blueprint_run's frozen
// actor_agent_id, NOT a value re-derived from the task claim. The task is left
// UNCLAIMED (the simulated post-takeover state, where tasks.claimed_by_agent_id
// was cleared), and the enqueued step must still attribute to the bot that has
// been executing the blueprint — proving the actor is stable across steps and
// immune to a mid-blueprint claim change.
func TestReactor_AdvanceInheritsActorAgent(t *testing.T) {
	s, database, brID, taskID, run0 := reactorFixture(t, "actor-inherit", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()

	// Freeze an actor on the blueprint_run (as Delegate does at mint) while the
	// task stays unclaimed — so the only place the actor can come from is the br.
	agentID, err := sqlitestore.New(database).Agents.Create(ctx, org, domain.Agent{DisplayName: "Bot"})
	if err != nil {
		t.Fatalf("Agents.Create: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET actor_agent_id = ? WHERE id = ?`, agentID, brID); err != nil {
		t.Fatalf("freeze actor on blueprint_run: %v", err)
	}
	var claim string
	if err := database.QueryRow(`SELECT COALESCE(claimed_by_agent_id, '') FROM tasks WHERE id = ?`, taskID).Scan(&claim); err != nil {
		t.Fatalf("read task claim: %v", err)
	}
	if claim != "" {
		t.Fatalf("precondition: task must be unclaimed to prove inheritance, got claim %q", claim)
	}

	stepRun, _ := s.agentRuns.GetSystem(ctx, org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	var actor string
	if err := database.QueryRow(`SELECT COALESCE(actor_agent_id, '') FROM runs WHERE blueprint_run_id = ? AND blueprint_step_index = 1`, brID).Scan(&actor); err != nil {
		t.Fatalf("read step-1 actor: %v", err)
	}
	if actor != agentID {
		t.Errorf("step-1 run actor_agent_id = %q, want %q (inherited from blueprint_run, not the empty task claim)", actor, agentID)
	}
}

// TestReactor_AdvanceInheritsTriggerID pins the by-rule spend attribution
// (TFAC-478): the next step the reactor enqueues inherits the blueprint_run's
// frozen trigger_id onto runs.trigger_id, so an event-fired step lands in the
// JOIN-free llm_spend view attributable to its firing rule. Without the
// inheritance, every autonomous step run carried NULL there — autonomous cost
// in the usage by-category split with an empty by-rule breakdown.
func TestReactor_AdvanceInheritsTriggerID(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "trig-inherit", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()

	// Freeze a firing trigger on the blueprint_run, as the event path does at
	// mint. The event_handlers row satisfies the trigger_id FK; the br flips to
	// the event shape (creator NULL) so its trigger-type CHECK holds.
	breaker, minSuit := 3, 0.5
	trigID := "trig-inherit-handler"
	if err := sqlitestore.New(database).EventHandlers.Create(ctx, org, runmode.LocalDefaultTeamID, domain.EventHandler{
		ID: trigID, Kind: domain.EventHandlerKindTrigger, EventType: domain.EventGitHubPRCICheckFailed,
		BlueprintID: "rbp-trig-inherit", BreakerThreshold: &breaker, MinAutonomySuitability: &minSuit,
		Enabled: true,
	}); err != nil {
		t.Fatalf("EventHandlers.Create: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET trigger_type = 'event', trigger_id = ?, creator_user_id = NULL WHERE id = ?`, trigID, brID); err != nil {
		t.Fatalf("freeze trigger on blueprint_run: %v", err)
	}

	stepRun, _ := s.agentRuns.GetSystem(ctx, org, run0)
	stepRun.TriggerType = "event"
	stepRun.CreatorUserID = ""
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	var gotTrig string
	if err := database.QueryRow(`SELECT COALESCE(trigger_id, '') FROM runs WHERE blueprint_run_id = ? AND blueprint_step_index = 1`, brID).Scan(&gotTrig); err != nil {
		t.Fatalf("read step-1 trigger_id: %v", err)
	}
	if gotTrig != trigID {
		t.Errorf("step-1 run trigger_id = %q, want %q (inherited from blueprint_run for by-rule spend attribution)", gotTrig, trigID)
	}
}

// TestReactor_FinalStepFinishCompletes: the final step completing with 'finish'
// terminates the blueprint completed and closes the task.
func TestReactor_FinalStepFinishCompletes(t *testing.T) {
	s, database, brID, taskID, run0 := reactorFixture(t, "fin", 1, "completed", "finish")
	org := runmode.LocalDefaultOrgID

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusCompleted {
		t.Errorf("blueprint status = %q, want completed", br.Status)
	}
	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Errorf("task status = %q, want done", got)
	}
}

// TestReactor_CancelRequestedTerminates: a continue outcome on a cancel-requested
// blueprint does NOT advance — it finalizes the blueprint cancelled.
func TestReactor_CancelRequestedTerminates(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "can", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), org, brID); err != nil {
		t.Fatalf("RequestRunCancelSystem: %v", err)
	}

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (cancel must not advance)", q)
	}
	br := mustGetRun(t, s, org, brID)
	if br.Status != domain.BlueprintRunStatusCancelled {
		t.Errorf("blueprint status = %q, want cancelled", br.Status)
	}
}

// TestReactor_ParkedStepLeavesRunning: a step parked `open` leaves the
// blueprint running and enqueues nothing (resume drives it later).
func TestReactor_ParkedStepLeavesRunning(t *testing.T) {
	s, database, brID, _, run0 := reactorFixture(t, "park", 2, "open", "")
	org := runmode.LocalDefaultOrgID

	stepRun, _ := s.agentRuns.GetSystem(context.Background(), org, run0)
	stepRun.TriggerType = "manual"
	stepRun.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(org, mustGetRun(t, s, org, brID), *stepRun, runConfig{orgID: org}, time.Now())

	if q := queuedStepRuns(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (parked step waits for resume)", q)
	}
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running (parked)", br.Status)
	}
}

func mustGetRun(t *testing.T, s *Spawner, org, brID string) *domain.BlueprintRun {
	t.Helper()
	br, err := s.blueprints.GetRunSystem(context.Background(), org, brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	return br
}

// TestStepModelOrInherit pins the per-step model resolution used when a
// blueprint advances: a per-step Prompt.Model override is
// downgrade-only — it applies only when it names a known, cheaper tier than the
// run's already tier-capped inherited model, so the shipped PR-review aggregator
// can drop to Haiku while no per-step value can ever escalate past the org cap
// baked into `inherited`. An empty override inherits unchanged.
func TestStepModelOrInherit(t *testing.T) {
	cases := []struct {
		name      string
		step      string
		inherited string
		want      string
	}{
		{"empty inherits", "", "opus", "opus"},
		{"empty inherits sonnet", "", "sonnet", "sonnet"},
		{"haiku downgrades from opus", "haiku", "opus", "haiku"},
		{"sonnet downgrades from opus", "sonnet", "opus", "sonnet"},
		{"haiku downgrades from sonnet", "haiku", "sonnet", "haiku"},
		{"opus cannot escalate from sonnet", "opus", "sonnet", "sonnet"},
		{"opus cannot escalate from haiku", "opus", "haiku", "haiku"},
		{"same tier is a no-op", "opus", "opus", "opus"},
		{"unknown override is ignored", "gpt-9", "opus", "opus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stepModelOrInherit(c.step, c.inherited); got != c.want {
				t.Errorf("stepModelOrInherit(%q, %q) = %q; want %q", c.step, c.inherited, got, c.want)
			}
		})
	}
}
