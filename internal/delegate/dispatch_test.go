package delegate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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
	if _, err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID}); err != nil {
		t.Fatalf("blueprint: %v", err)
	}
	promptIDs := make([]string, nSteps)
	for i := 0; i < nSteps; i++ {
		pid := fmt.Sprintf("%s-p%d", suffix, i)
		ensureTestPrompt(t, database, domain.Prompt{ID: pid, Name: pid, Body: "b", Source: "user"})
		promptIDs[i] = pid
	}
	if _, err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, promptIDs, nil); err != nil {
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
	created, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "rbpr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-" + suffix, StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	brID := created.ID

	step0 := 0
	step0ConversationID := "rrun0-" + suffix
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: step0ConversationID, TaskID: task.ID, PromptID: promptIDs[0], Status: step0Status,
		Outcome: step0Outcome,
		Model:   "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step0,
	})

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	return s, database, brID, task.ID, step0ConversationID
}

func queuedStepConversations(t *testing.T, database *sql.DB, brID string) []int {
	t.Helper()
	rows, err := database.Query(`SELECT blueprint_step_index FROM conversations WHERE blueprint_run_id = ? AND status IS NULL ORDER BY blueprint_step_index`, brID)
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
// consequence (TFAC-586): drainConversationQueue must not claim a queued row while
// Draining() is true, and must resume claiming the moment it flips back —
// proving the gate, not fixture setup, is what blocked the first attempt.
func TestDrainRunQueue_DrainingSkipsClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	seedConversation(t, database, "run-drain-skip", "sess-1", "/tmp/wt-run-drain-skip")
	forceClaimable(t, database, "run-drain-skip")

	s.SetDraining(true)
	s.drainConversationQueue(context.Background())

	if claims := claimCountFor(t, database, "run-drain-skip"); claims != 0 {
		t.Errorf("claims = %d, want 0 — a draining instance must not claim", claims)
	}

	s.SetDraining(false)
	s.drainConversationQueue(context.Background())
	// The claim is minted synchronously; drainConversationQueue then dispatches the
	// claimed run on a goroutine. This fixture's blueprint has an empty step
	// plan, so that goroutine deterministically cancels the run — and may
	// already have released the claim by the time we read (a race -race
	// exposes). The claim ROW is what proves the gate let the claim through,
	// and a released claim is still a claim.
	if claims := claimCountFor(t, database, "run-drain-skip"); claims != 1 {
		t.Errorf("claims = %d, want 1 once undrained", claims)
	}
}

// forceClaimable puts a seeded run into the mid-flight state the claim
// predicate matches: no stored outcome, no active claim.
func forceClaimable(t *testing.T, database *sql.DB, conversationID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, conversationID); err != nil {
		t.Fatalf("force claimable: %v", err)
	}
}

// claimCountFor counts every claim ever minted on a conversation — the
// derived answer to "was this claimed", stable against a dispatch goroutine
// that may already have released it.
func claimCountFor(t *testing.T, database *sql.DB, conversationID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM claims WHERE conversation_id = ?`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	return n
}

// TestDrainRunQueue_PartitionFencedSkipsClaim mirrors the draining test for
// the partition self-fence gate: a fenced instance must not claim either,
// even though (unlike IdentityFenced) the fence isn't sticky.
func TestDrainRunQueue_PartitionFencedSkipsClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")
	seedConversation(t, database, "run-pfence-skip", "sess-1", "/tmp/wt-run-pfence-skip")
	forceClaimable(t, database, "run-pfence-skip")

	s.partitionFenced.Store(true)
	s.drainConversationQueue(context.Background())

	if claims := claimCountFor(t, database, "run-pfence-skip"); claims != 0 {
		t.Errorf("claims = %d, want 0 — a partition-fenced instance must not claim", claims)
	}
}

// TestReconcileRunQueue_ParksOrphanUnderTerminalBlueprint is the
// boot-reconcile integration check: a child run left mid-flight under an
// already-terminal blueprint_run (the desync) must be healed by the boot
// sweep, so the dispatcher stops treating it as live work and its feature
// branch is no longer pinned in a worktree.
//
// It parks `open` rather than writing a terminal, because nothing about an
// orphan failed and nothing about it concluded. The park is NOT a claim that
// the row is resumable: its blueprint is terminal, so the claim gate refuses
// it — which the second half of this test pins.
func TestReconcileRunQueue_ParksOrphanUnderTerminalBlueprint(t *testing.T) {
	s, database, brID, _, step0ConversationID := reactorFixture(t, "recon-orphan", 1, "running", "")

	// Force the desync directly, mimicking a DB broken before the atomic park
	// in MarkRunStatus landed: terminal parent, child still mid-flight.
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id = ?`, step0ConversationID); err != nil {
		t.Fatalf("force child mid-flight: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'cancelled', cancel_requested = 0 WHERE id = ?`, brID); err != nil {
		t.Fatalf("force blueprint cancelled: %v", err)
	}

	s.reconcileConversationQueue(context.Background())

	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, step0ConversationID).Scan(&status); err != nil {
		t.Fatalf("read child status: %v", err)
	}
	if status != "open" {
		t.Errorf("orphan child status = %q, want open after boot reconcile", status)
	}

	// An `open` row with undelivered input is the one shape the claim
	// predicate would otherwise drive. The blueprint gate is what stops it.
	if _, err := database.Exec(`
		INSERT INTO messages (conversation_id, org_id, role, subtype, content, delivered, window_state)
		VALUES (?, ?, 'user', '', 'pick this back up', 0, 'active')`,
		step0ConversationID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("queue input on the parked orphan: %v", err)
	}
	claimed, err := s.conversationQueue.ClaimNextConversation(context.Background(), "exec-orphan", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Errorf("claimed %q — a parked orphan under a terminal blueprint must never be driven", claimed.ID)
	}
}

// TestReactor_AdvanceEnqueuesNextStep: a non-final step that completes with
// 'continue' enqueues the next step and bumps current_step_index, leaving the
// blueprint running.
func TestReactor_AdvanceEnqueuesNextStep(t *testing.T) {
	s, database, brID, _, step0ConversationID := reactorFixture(t, "adv", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
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
	s, database, brID, taskID, step0ConversationID := reactorFixture(t, "actor-inherit", 2, "completed", "continue")
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

	stepConversation, _ := s.conversations.GetSystem(ctx, org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	var actor string
	if err := database.QueryRow(`SELECT COALESCE(actor_agent_id, '') FROM conversations WHERE blueprint_run_id = ? AND blueprint_step_index = 1`, brID).Scan(&actor); err != nil {
		t.Fatalf("read step-1 actor: %v", err)
	}
	if actor != agentID {
		t.Errorf("step-1 run actor_agent_id = %q, want %q (inherited from blueprint_run, not the empty task claim)", actor, agentID)
	}
}

// TestReactor_AdvanceInheritsTriggerID pins the by-rule spend attribution
// (TFAC-478): the next step the reactor enqueues inherits the
// blueprint_run's frozen trigger_id onto conversations.trigger_id, so an
// event-fired step lands in the JOIN-free llm_spend view attributable to
// its firing rule. Without the inheritance, every autonomous step run
// carried NULL there — autonomous cost in the usage by-category split with
// an empty by-rule breakdown.
func TestReactor_AdvanceInheritsTriggerID(t *testing.T) {
	s, database, brID, _, step0ConversationID := reactorFixture(t, "trig-inherit", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()

	// Freeze a firing trigger on the blueprint_run, as the event path does at
	// mint. The event_handlers row satisfies the trigger_id FK; the br flips to
	// the event shape (creator NULL) so its trigger-type CHECK holds.
	breaker, minSuit := 3, 0.5
	trigID := "trig-inherit-handler"
	if _, err := sqlitestore.New(database).EventHandlers.Create(ctx, org, runmode.LocalDefaultTeamID, domain.EventHandler{
		ID: trigID, Kind: domain.EventHandlerKindTrigger, EventType: domain.EventGitHubPRCICheckFailed,
		BlueprintID: "rbp-trig-inherit", BreakerThreshold: &breaker, MinAutonomySuitability: &minSuit,
		Enabled: true,
	}); err != nil {
		t.Fatalf("EventHandlers.Create: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET trigger_type = 'event', trigger_id = ?, creator_user_id = NULL WHERE id = ?`, trigID, brID); err != nil {
		t.Fatalf("freeze trigger on blueprint_run: %v", err)
	}

	stepConversation, _ := s.conversations.GetSystem(ctx, org, step0ConversationID)
	stepConversation.TriggerType = "event"
	stepConversation.CreatorUserID = ""
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	var gotTrig string
	if err := database.QueryRow(`SELECT COALESCE(trigger_id, '') FROM conversations WHERE blueprint_run_id = ? AND blueprint_step_index = 1`, brID).Scan(&gotTrig); err != nil {
		t.Fatalf("read step-1 trigger_id: %v", err)
	}
	if gotTrig != trigID {
		t.Errorf("step-1 run trigger_id = %q, want %q (inherited from blueprint_run for by-rule spend attribution)", gotTrig, trigID)
	}
}

// TestReactor_FinalStepFinishCompletes: the final step completing with 'finish'
// terminates the blueprint completed and closes the task.
func TestReactor_FinalStepFinishCompletes(t *testing.T) {
	s, database, brID, taskID, step0ConversationID := reactorFixture(t, "fin", 1, "completed", "finish")
	org := runmode.LocalDefaultOrgID

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
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
	s, database, brID, _, step0ConversationID := reactorFixture(t, "can", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID
	if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), org, brID); err != nil {
		t.Fatalf("RequestRunCancelSystem: %v", err)
	}

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
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
	s, database, brID, _, step0ConversationID := reactorFixture(t, "park", 2, "open", "")
	org := runmode.LocalDefaultOrgID

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (parked step waits for resume)", q)
	}
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
		t.Errorf("blueprint status = %q, want running (parked)", br.Status)
	}
}

// TestReactor_CancelledStepParksAndTerminatesBlueprint is the ordering the
// whole cancel path now rests on. A cancelled step parks `open` rather than
// writing a terminal of its own, so the reactor has to read cancel_requested
// BEFORE the parked arm — read the park first and every cancelled run would
// strand its blueprint 'running' forever with nobody to finalize it.
func TestReactor_CancelledStepParksAndTerminatesBlueprint(t *testing.T) {
	s, database, brID, _, step0ConversationID := reactorFixture(t, "cancel-park", 2, "open", "")
	org := runmode.LocalDefaultOrgID
	if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), org, brID); err != nil {
		t.Fatalf("RequestRunCancelSystem: %v", err)
	}

	stepConversation, _ := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID
	// The pre-agent blueprint the dispatcher captured has no cancel on it; the
	// reactor's own refresh is what must find the signal.
	s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

	if q := queuedStepConversations(t, database, brID); len(q) != 0 {
		t.Fatalf("queued step runs = %v, want none (cancel must not advance)", q)
	}
	if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusCancelled {
		t.Errorf("blueprint status = %q, want cancelled — a parked step under a cancel must not leave it running", br.Status)
	}
	// The step keeps its park; the blueprint carries the cancellation.
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, step0ConversationID).Scan(&status); err != nil {
		t.Fatalf("read step status: %v", err)
	}
	if status != "open" {
		t.Errorf("step status = %q, want open", status)
	}
}

// TestReactor_IgnoresTerminalFromAStepTheBlueprintMovedPast forges the exact
// corruption the claim gate now refuses to set up: step 0 concluded, the
// blueprint advanced to step 1, and step 0's conversation reaches a terminal
// anyway — the shape an in-flight engagement leaves behind when the gate
// narrows under it. Every transition either post-run path could make reads the
// sequence's position off the CONCLUDING conversation, so acting on a stale one
// corrupts the blueprint, and each subtest pins one of the ways.
//
// The fixture is the same throughout: a three-step plan whose live step is 1.
func TestReactor_IgnoresTerminalFromAStepTheBlueprintMovedPast(t *testing.T) {
	org := runmode.LocalDefaultOrgID

	// stage builds the advanced blueprint and hands back the spawner, the DB,
	// the blueprint id, the task id and the STALE step-0 run ready to conclude
	// with the given outcome.
	stage := func(t *testing.T, suffix, step0Outcome string) (*Spawner, *sql.DB, string, string, domain.Conversation) {
		t.Helper()
		s, database, brID, taskID, step0ConversationID := reactorFixture(t, suffix, 3, "completed", step0Outcome)
		ctx := context.Background()

		// The advance the reactor itself performed on step 0's first
		// conclusion: pointer first, then the row it names. Step 1 is
		// mid-flight (NULL status) and holds the shared worktree.
		if _, err := s.blueprints.SetRunCurrentStepSystem(ctx, org, brID, 1); err != nil {
			t.Fatalf("SetRunCurrentStepSystem: %v", err)
		}
		step1 := 1
		dbtest.SeedConversation(t, database, domain.Conversation{
			ID: "rrun1-" + suffix, TaskID: taskID, PromptID: suffix + "-p1",
			Model: "claude-sonnet-4-6", BlueprintRunID: brID, BlueprintStepIndex: &step1,
		})

		stepConversation, err := s.conversations.GetSystem(ctx, org, step0ConversationID)
		if err != nil || stepConversation == nil {
			t.Fatalf("read stale step run: (%v, %v)", stepConversation, err)
		}
		stepConversation.TriggerType = "manual"
		stepConversation.CreatorUserID = runmode.LocalDefaultUserID
		return s, database, brID, taskID, *stepConversation
	}

	t.Run("continue does not advance or double-enqueue", func(t *testing.T) {
		// The corruption: next := 0+1 rewrites current_step_index backwards to
		// 1 and enqueues a SECOND copy of the step already running.
		s, database, brID, _, stale := stage(t, "stale-adv", "continue")

		s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), stale, runConfig{orgID: org}, time.Now())

		if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
			t.Fatalf("queued step runs = %v, want [1] — the live step must not be enqueued a second time", q)
		}
		br := mustGetRun(t, s, org, brID)
		if br.CurrentStepIndex != 1 {
			t.Errorf("current_step_index = %d, want 1 — a stale terminal must not move the pointer", br.CurrentStepIndex)
		}
		if br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("blueprint status = %q, want running", br.Status)
		}
	})

	t.Run("finish does not terminate the blueprint under a live step", func(t *testing.T) {
		// The one confirmed in the wild: terminateBlueprint finalizes the
		// blueprint, closes the task and removes the worktree — out from under
		// the agent still working in it.
		s, database, brID, taskID, stale := stage(t, "stale-fin", "finish")

		s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), stale, runConfig{orgID: org}, time.Now())

		if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("blueprint status = %q, want running — step 1 is still executing", br.Status)
		}
		if got := readTaskStatus(t, database, taskID); got == "done" {
			t.Error("task closed on a stale step's finish while its blueprint's live step was still running")
		}
	})

	t.Run("the resume finalizer refuses it on the same grounds", func(t *testing.T) {
		// The other post-run orchestration path: a follow-up's conclusion
		// finalizes through ResumeBlueprintAfterResume rather than the reactor,
		// and it terminates blueprints too — so it carries the same re-check.
		s, database, brID, taskID, stale := stage(t, "stale-resume", "finish")

		s.ResumeBlueprintAfterResume(org, stale.ID, runmode.LocalDefaultUserID)

		if br := mustGetRun(t, s, org, brID); br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("blueprint status = %q, want running — step 1 is still executing", br.Status)
		}
		if got := readTaskStatus(t, database, taskID); got == "done" {
			t.Error("task closed by a stale step's resume while its blueprint's live step was still running")
		}
	})

	t.Run("a cancel is left for the live step to carry", func(t *testing.T) {
		// A cancel does end everything — but the disposition belongs to
		// whichever path disposes of the step that is actually running.
		// Finalizing here would tear the worktree out from under it just the
		// same, and the live step's own conclusion finalizes moments later.
		s, _, brID, _, stale := stage(t, "stale-can", "continue")
		if _, err := s.blueprints.RequestRunCancelSystem(context.Background(), org, brID); err != nil {
			t.Fatalf("RequestRunCancelSystem: %v", err)
		}

		s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), stale, runConfig{orgID: org}, time.Now())

		br := mustGetRun(t, s, org, brID)
		if br.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("blueprint status = %q, want running — the live step disposes of the cancel", br.Status)
		}
		if !br.CancelRequested {
			t.Error("cancel_requested was cleared; the signal must survive for the live step to read")
		}
	})
}

// TestReactor_LeavesASuccessorsConversationAlone is the same lesson on the
// other axis: a re-read showing neither this engagement's terminal nor a park
// means someone else picked the conversation up in between. Queued, mid-setup
// and running all say that, and none of them says "this step is wedged" — but
// the default arm failed the blueprint on all three, which is how a follow-up
// in the hand-off window killed a healthy sequence. Refusing the wake removes
// that entrant; this keeps the next one (a crash re-claim, a boot reconcile)
// from finding the same edge.
func TestReactor_LeavesASuccessorsConversationAlone(t *testing.T) {
	org := runmode.LocalDefaultOrgID

	for _, status := range []string{domain.StatusQueued, domain.StatusRunning, domain.ClaimPhaseCloning} {
		t.Run(status, func(t *testing.T) {
			s, database, brID, taskID, step0ConversationID := reactorFixture(t, "successor-"+status, 2, "completed", "continue")

			stepConversation := loadConversation(t, s, step0ConversationID)
			stepConversation.Status = status
			stepConversation.TriggerType = "manual"
			stepConversation.CreatorUserID = runmode.LocalDefaultUserID
			s.reactToStepTerminal(context.Background(), org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())

			if q := queuedStepConversations(t, database, brID); len(q) != 0 {
				t.Errorf("queued step runs = %v, want none — nothing about a successor's state says to advance", q)
			}
			br := mustGetRun(t, s, org, brID)
			if br.Status != domain.BlueprintRunStatusRunning {
				t.Errorf("blueprint status = %q, want running — a successor owns the conversation, so this engagement writes no transition", br.Status)
			}
			if br.CurrentStepIndex != 0 {
				t.Errorf("current_step_index = %d, want 0", br.CurrentStepIndex)
			}
			if got := readTaskStatus(t, database, taskID); got == "done" {
				t.Error("task closed off a successor's state")
			}
		})
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
// blueprint advances: unset inherits, pinned wins, and nothing in between.
//
// Every case where a pin differs from what was inherited is the same case —
// the pin is what runs — including the ones an ordering rule used to discard:
// a costlier model than the run inherited, a model no Anthropic tier ranks
// (Fable), an older spelling. A pin is a person's explicit choice, and the id
// that survives here is the STORED one, since it is what reaches the provider
// and what the ledger prices.
//
// The set these cases run under is the unrestricted one — what a team that has
// narrowed nothing resolves to. TestStepModelOrInherit_PinOutsideTheSetFails
// below is the other half: the one thing a pin IS held to.
func TestStepModelOrInherit(t *testing.T) {
	cases := []struct {
		name      string
		step      string
		inherited string
		want      string
	}{
		{"empty inherits", "", domain.ModelOpus, domain.ModelOpus},
		{"empty inherits sonnet", "", domain.ModelSonnet, domain.ModelSonnet},
		{"a cheaper pin wins", domain.ModelHaiku, domain.ModelOpus, domain.ModelHaiku},
		{"a mid pin wins", domain.ModelSonnet, domain.ModelOpus, domain.ModelSonnet},
		{"a costlier pin wins too", domain.ModelOpus, domain.ModelSonnet, domain.ModelOpus},
		{"a much costlier pin wins", domain.ModelOpus, domain.ModelHaiku, domain.ModelOpus},
		{"the same model is a no-op", domain.ModelOpus, domain.ModelOpus, domain.ModelOpus},
		// A catalog model with no place on the Haiku-Sonnet-Opus ladder. It is
		// pinnable, so it runs.
		{"a model outside the tier ladder wins", "claude-fable-5", domain.ModelOpus, "claude-fable-5"},
		// A prior-generation spelling nothing ranks either.
		{"an older concrete spelling wins", "claude-opus-4-8", domain.ModelOpus, "claude-opus-4-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := stepModelOrInherit(c.step, c.inherited, domain.ModelSet{})
			if err != nil {
				t.Fatalf("stepModelOrInherit(%q, %q): %v", c.step, c.inherited, err)
			}
			if got != c.want {
				t.Errorf("stepModelOrInherit(%q, %q) = %q; want %q", c.step, c.inherited, got, c.want)
			}
		})
	}
}

// A pin outside the team's enable-set fails the step by name rather than
// falling back to what the step would have inherited. Silently ignoring it
// would run the step on a model nobody chose — the substitution the whole
// selection design forbids — and would hide from the author that their pin
// stopped taking effect.
//
// The inherited model is deliberately enabled here, so the only thing that can
// produce the refusal is the pin itself.
func TestStepModelOrInherit_PinOutsideTheSetFails(t *testing.T) {
	enabled := domain.TeamModelSet([]string{domain.ModelSonnet}, orgUnrestricted())

	got, err := stepModelOrInherit(domain.ModelOpus, domain.ModelSonnet, enabled)
	if !errors.Is(err, domain.ErrModelNotEnabled) {
		t.Fatalf("stepModelOrInherit = (%q, %v); want ErrModelNotEnabled", got, err)
	}
	if got != "" {
		t.Errorf("a refused step resolved to %q; want no model at all", got)
	}
	if !strings.Contains(err.Error(), domain.ModelOpus) {
		t.Errorf("error %q does not name the pinned model", err)
	}
	if !strings.Contains(err.Error(), domain.ModelSonnet) {
		t.Errorf("error %q does not name the set that excludes it", err)
	}

	// An unset step under the same set still inherits: the set narrows what may
	// be PINNED, and the inherited model was already held to it upstream.
	inherited, err := stepModelOrInherit("", domain.ModelSonnet, enabled)
	if err != nil || inherited != domain.ModelSonnet {
		t.Errorf("unset step = (%q, %v); want the inherited model", inherited, err)
	}

	// A pin the set DOES name runs, whatever it costs relative to what was
	// inherited — the enable-set is membership, never a ceiling.
	both := domain.TeamModelSet([]string{domain.ModelHaiku, domain.ModelOpus}, orgUnrestricted())
	pinned, err := stepModelOrInherit(domain.ModelOpus, domain.ModelHaiku, both)
	if err != nil || pinned != domain.ModelOpus {
		t.Errorf("an enabled costlier pin = (%q, %v); want %q", pinned, err, domain.ModelOpus)
	}
}
