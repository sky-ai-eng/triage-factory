package dbtest

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskCloseDeferralStores are the stores the held-close suite drives: the
// hold's writes and reads on TaskStore, the terminating close that holds in
// its transaction, and the working-conversation read the sweep makes.
type TaskCloseDeferralStores struct {
	Tasks         db.TaskStore
	Entities      db.EntityStore
	Conversations db.ConversationStore
}

// TaskCloseDeferralSeeder stages the graph a held close spans — task, closing
// event, blueprint run and its conversation, the entity — through the
// terminating-close seeder, plus the conversation states the working
// predicate tells apart.
type TaskCloseDeferralSeeder struct {
	EntityTerminalCloseSeeder

	// Claim gives the conversation an active claim.
	Claim func(t *testing.T, conversationID string)
	// HoldUntilLater sets the conversation's next_attempt_at an hour ahead,
	// as a hand-back that has to wait does.
	HoldUntilLater func(t *testing.T, conversationID string)
	// CallOff sets the blueprint run's cancel_requested.
	CallOff func(t *testing.T, blueprintRunID string)
}

// TaskCloseDeferralFactory hands the suite a fresh database per subtest: the
// cross-org list read must see only what the subtest staged.
type TaskCloseDeferralFactory func(t *testing.T) (stores TaskCloseDeferralStores, orgID string, seed TaskCloseDeferralSeeder)

// RunTaskCloseDeferralConformance is the shared suite for a task's held
// close: TaskStore.CloseOrHoldSystem, ApplyHeldCloseSystem,
// CloseDeferralForTaskSystem, ListActiveCloseDeferralsSystem,
// ClearCloseDeferralSystem and Bump's clear,
// the hold arm of EntityStore.CloseTerminalSystem, and the working predicate
// both decide on, read through
// ConversationStore.HasWorkingConversationForTaskSystem.
func RunTaskCloseDeferralConformance(t *testing.T, mk TaskCloseDeferralFactory) {
	t.Helper()
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed
	const review = domain.EventGitHubPRReviewChangesRequested

	held := func(t *testing.T, tasks db.TaskStore, orgID, taskID string) *domain.TaskCloseDeferral {
		t.Helper()
		d, err := tasks.CloseDeferralForTaskSystem(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("CloseDeferralForTaskSystem: %v", err)
		}
		return d
	}

	t.Run("Working_predicate", func(t *testing.T) {
		cases := []struct {
			name    string
			status  string
			stage   func(t *testing.T, seed TaskCloseDeferralSeeder, runID, convID string)
			working bool
		}{
			{name: "mid_flight", status: "", working: true},
			{name: "active_claim", status: domain.StatusOpen, working: true,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.Claim(t, convID) }},
			{name: "held_by_a_hand_back", status: "", working: true,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.HoldUntilLater(t, convID) }},
			{name: "parked_on_a_person", status: domain.StatusOpen},
			{name: "concluded", status: SeedConcluded},
			{name: "failed", status: domain.StatusFailed},
			{name: "blueprint_called_off", status: "",
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, runID, _ string) { seed.CallOff(t, runID) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stores, orgID, seed := mk(t)
				taskID := seed.Task(t)
				runID, convID := seed.BlueprintAndConversation(t, taskID, "running", tc.status)
				if tc.stage != nil {
					tc.stage(t, seed, runID, convID)
				}
				got, err := stores.Conversations.HasWorkingConversationForTaskSystem(ctx, orgID, taskID)
				if err != nil {
					t.Fatalf("HasWorkingConversationForTaskSystem: %v", err)
				}
				if got != tc.working {
					t.Errorf("working = %v, want %v", got, tc.working)
				}
			})
		}
	})

	t.Run("CloseOrHold_holds_on_a_working_conversation_and_closes_otherwise", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		working := seed.Task(t)
		idle := seed.Task(t)
		first := seed.Event(t, working)
		second := seed.Event(t, working)
		idleEvent := seed.Event(t, idle)
		workingRun, workingConv := seed.BlueprintAndConversation(t, working, "running", "")
		idleRun, idleConv := seed.BlueprintAndConversation(t, idle, "running", domain.StatusOpen)

		out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, working, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, first)
		if err != nil {
			t.Fatalf("CloseOrHoldSystem: %v", err)
		}
		want := db.TaskCloseOutcome{Held: true, NewlyHeld: true, WorkingConversationIDs: []string{workingConv}}
		if !outcomeEqual(out, want) {
			t.Errorf("first close = %+v, want %+v", out, want)
		}
		if s := seed.TaskStatus(t, working); s != "queued" {
			t.Errorf("held task status = %q, want queued", s)
		}
		if n := seed.CloseAuditCount(t, working); n != 0 {
			t.Errorf("held task close audit rows = %d, want 0", n)
		}
		if seed.CancelRequested(t, workingRun) {
			t.Error("the held task's run was called off")
		}
		if d := held(t, stores.Tasks, orgID, working); d == nil || *d != (domain.TaskCloseDeferral{OrgID: orgID, TaskID: working, EventID: first}) {
			t.Errorf("held close = %+v, want event %s", d, first)
		}

		// A second closing event while the close is held takes its place.
		out, err = stores.Tasks.CloseOrHoldSystem(ctx, orgID, working, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, second)
		if err != nil {
			t.Fatalf("second CloseOrHoldSystem: %v", err)
		}
		if !out.Held || out.NewlyHeld {
			t.Errorf("second close = %+v, want held, not newly", out)
		}
		if d := held(t, stores.Tasks, orgID, working); d == nil || d.EventID != second {
			t.Errorf("held close = %+v, want event %s", d, second)
		}

		out, err = stores.Tasks.CloseOrHoldSystem(ctx, orgID, idle, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, idleEvent)
		if err != nil {
			t.Fatalf("CloseOrHoldSystem (idle): %v", err)
		}
		want = db.TaskCloseOutcome{Closed: true, ActiveConversationIDs: []string{idleConv}}
		if !outcomeEqual(out, want) {
			t.Errorf("idle close = %+v, want %+v", out, want)
		}
		if s := seed.TaskStatus(t, idle); s != "done" {
			t.Errorf("idle task status = %q, want done", s)
		}
		if n := seed.CloseAuditCount(t, idle); n != 1 {
			t.Errorf("idle task close audit rows = %d, want 1", n)
		}
		if !seed.CancelRequested(t, idleRun) {
			t.Error("the closed task's run was not called off")
		}

		out, err = stores.Tasks.CloseOrHoldSystem(ctx, orgID, idle, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, idleEvent)
		if err != nil {
			t.Fatalf("CloseOrHoldSystem (terminal): %v", err)
		}
		if !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("close of a terminal task = %+v, want neither", out)
		}
	})

	t.Run("CloseTerminal_holds_working_tasks_and_closes_the_rest", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		working := seed.Task(t)
		entityID := seed.EntityForTask(t, working)
		other := seed.TaskOnEntity(t, entityID, review)
		eventID := seed.Event(t, working)
		runID, convID := seed.BlueprintAndConversation(t, working, "running", "")
		_, seq := seed.EntityRow(t, entityID)

		res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review},
			"entity_closed", domain.EventGitHubPRMerged, eventID)
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed {
			t.Fatal("entity did not close")
		}
		if !slices.Equal(res.ClosedTaskIDs, []string{other}) {
			t.Errorf("ClosedTaskIDs = %v, want [%s]", res.ClosedTaskIDs, other)
		}
		if len(res.HeldTasks) != 1 || res.HeldTasks[0].TaskID != working || !res.HeldTasks[0].NewlyHeld ||
			!slices.Equal(res.HeldTasks[0].WorkingConversationIDs, []string{convID}) {
			t.Errorf("HeldTasks = %+v, want %s newly held on [%s]", res.HeldTasks, working, convID)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "closed" {
			t.Errorf("entity state = %q, want closed", state)
		}
		if s := seed.TaskStatus(t, working); s != "queued" {
			t.Errorf("held task status = %q, want queued", s)
		}
		if s := seed.TaskStatus(t, other); s != "done" {
			t.Errorf("other task status = %q, want done", s)
		}
		if n := seed.CloseAuditCount(t, working); n != 0 {
			t.Errorf("held task close audit rows = %d, want 0", n)
		}
		if seed.CancelRequested(t, runID) {
			t.Error("the held task's run was called off")
		}
		if d := held(t, stores.Tasks, orgID, working); d == nil || d.EventID != eventID {
			t.Errorf("held close = %+v, want event %s", d, eventID)
		}
		// Open on its closed entity by design: not the stranded task the
		// terminal-state checker counts.
		stranded, err := stores.Tasks.ListOpenOnClosedEntitiesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListOpenOnClosedEntitiesSystem: %v", err)
		}
		if slices.Contains(stranded, working) {
			t.Errorf("the held task is counted as stranded on its closed entity: %v", stranded)
		}
	})

	t.Run("Bump_clears_the_hold", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil {
			t.Fatalf("CloseOrHoldSystem: %v", err)
		}

		if _, err := stores.Tasks.BumpSystem(ctx, orgID, taskID, seed.Event(t, taskID)); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		if d := held(t, stores.Tasks, orgID, taskID); d != nil {
			t.Errorf("held close after a bump = %+v, want none", *d)
		}
	})

	t.Run("Reads_see_holds_on_active_tasks_only", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		holding := seed.Task(t)
		closed := seed.Task(t)
		plain := seed.Task(t)
		for _, taskID := range []string{holding, closed} {
			seed.BlueprintAndConversation(t, taskID, "running", "")
			if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID)); err != nil {
				t.Fatalf("CloseOrHoldSystem(%s): %v", taskID, err)
			}
		}
		// A close by any other path leaves the column as it is; the reads
		// are what keep it from being read again.
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, closed, "run_completed", ""); err != nil {
			t.Fatalf("CloseSystem: %v", err)
		}

		all, err := stores.Tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		var ids []string
		for _, d := range all {
			ids = append(ids, d.TaskID)
		}
		if !slices.Equal(ids, []string{holding}) {
			t.Errorf("listed tasks = %v, want only %s (closed %s and plain %s left out)", ids, holding, closed, plain)
		}
		if len(all) == 1 && all[0].OrgID != orgID {
			t.Errorf("listed org = %q, want %q", all[0].OrgID, orgID)
		}
		for _, taskID := range []string{closed, plain} {
			if d := held(t, stores.Tasks, orgID, taskID); d != nil {
				t.Errorf("CloseDeferralForTaskSystem(%s) = %+v, want none", taskID, *d)
			}
		}
	})

	t.Run("Clear_compares_on_the_event_it_read", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		read := seed.Event(t, taskID)
		newer := seed.Event(t, taskID)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		for _, eventID := range []string{read, newer} {
			// The hold moves on to a newer event between the sweep's read and
			// its clear.
			if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil {
				t.Fatalf("CloseOrHoldSystem: %v", err)
			}
		}

		cleared, err := stores.Tasks.ClearCloseDeferralSystem(ctx, orgID, taskID, read)
		if err != nil {
			t.Fatalf("ClearCloseDeferralSystem (stale): %v", err)
		}
		if cleared {
			t.Error("clear on the overwritten event reported cleared")
		}
		if d := held(t, stores.Tasks, orgID, taskID); d == nil || d.EventID != newer {
			t.Fatalf("the overwritten hold did not survive: %+v", d)
		}

		cleared, err = stores.Tasks.ClearCloseDeferralSystem(ctx, orgID, taskID, newer)
		if err != nil {
			t.Fatalf("ClearCloseDeferralSystem: %v", err)
		}
		if !cleared {
			t.Error("clear on the held event reported not cleared")
		}
		if d := held(t, stores.Tasks, orgID, taskID); d != nil {
			t.Errorf("hold still read after its clear: %+v", *d)
		}
	})

	t.Run("ApplyHeldClose_applies_only_the_hold_it_names", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		hold := func(taskID, eventID string) {
			t.Helper()
			if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil {
				t.Fatalf("CloseOrHoldSystem: %v", err)
			}
		}
		apply := func(taskID, eventID string) db.TaskCloseOutcome {
			t.Helper()
			out, err := stores.Tasks.ApplyHeldCloseSystem(ctx, orgID, taskID, eventID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed)
			if err != nil {
				t.Fatalf("ApplyHeldCloseSystem: %v", err)
			}
			return out
		}

		stale := seed.Task(t)
		first, second := seed.Event(t, stale), seed.Event(t, stale)
		staleRun, staleConv := seed.BlueprintAndConversation(t, stale, "running", "")
		hold(stale, first)
		if out := apply(stale, first); !outcomeEqual(out, db.TaskCloseOutcome{Held: true, WorkingConversationIDs: []string{staleConv}}) {
			t.Errorf("apply under a working conversation = %+v, want held", out)
		}
		// The hold the sweep read is replaced, then cancelled, and by the
		// time the cancelled one is applied nothing is working.
		hold(stale, second)
		if out := apply(stale, first); !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("apply of a replaced hold = %+v, want nothing", out)
		}
		if _, err := stores.Tasks.BumpSystem(ctx, orgID, stale, seed.Event(t, stale)); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		seed.CallOff(t, staleRun)
		if out := apply(stale, second); !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("apply of a cancelled hold = %+v, want nothing", out)
		}
		if s := seed.TaskStatus(t, stale); s == "done" {
			t.Error("a cancelled hold closed the task")
		}
		if d := held(t, stores.Tasks, orgID, stale); d != nil {
			t.Errorf("the cancelled hold came back: %+v", *d)
		}

		current := seed.Task(t)
		eventID := seed.Event(t, current)
		currentRun, _ := seed.BlueprintAndConversation(t, current, "running", "")
		hold(current, eventID)
		seed.CallOff(t, currentRun)
		if out := apply(current, eventID); !out.Closed {
			t.Errorf("apply of the current hold with nothing working = %+v, want closed", out)
		}
		if s := seed.TaskStatus(t, current); s != "done" {
			t.Errorf("task status = %q, want done", s)
		}
		if n := seed.CloseAuditCount(t, current); n != 1 {
			t.Errorf("close audit rows = %d, want 1", n)
		}
	})
}

func outcomeEqual(a, b db.TaskCloseOutcome) bool {
	return a.Closed == b.Closed && a.Held == b.Held && a.NewlyHeld == b.NewlyHeld &&
		slices.Equal(a.ActiveConversationIDs, b.ActiveConversationIDs) &&
		slices.Equal(a.WorkingConversationIDs, b.WorkingConversationIDs)
}
