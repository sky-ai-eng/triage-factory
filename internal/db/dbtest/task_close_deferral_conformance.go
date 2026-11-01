package dbtest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskCloseDeferralStores are the stores the held-close suite drives: the
// hold's writes and reads on TaskStore, the terminating close that holds in
// its transaction, the staged notes a cleared hold withdraws, the requeue
// and undo that return a task to the queue, and the person's message and the
// stop request that keep a conversation from expiring.
type TaskCloseDeferralStores struct {
	Tasks            db.TaskStore
	Entities         db.EntityStore
	StagedInjections db.StagedInjectionStore
	Swipes           db.SwipeStore
	Conversations    db.ConversationStore
	// SwipeUserID is who a swipe written through Swipes is attributed to,
	// which an undo's guard keys on, and who writes a person's message.
	SwipeUserID string
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
	// DraftPullRequest gives the conversation a draft pull request
	// deliverable about the pull request entityID names, one that awaits a
	// person's verdict while that entity is open; "" links it to none.
	DraftPullRequest func(t *testing.T, conversationID, entityID string)
	// ResolveDeliverables marks the conversation's draft pull requests open,
	// as a person marking one ready does.
	ResolveDeliverables func(t *testing.T, conversationID string)
	// AgeHold moves back the time the task's held close began by d, as d
	// passing does.
	AgeHold func(t *testing.T, taskID string, d time.Duration)
}

// TaskCloseDeferralFactory hands the suite a fresh database per subtest: the
// cross-org list read must see only what the subtest staged.
type TaskCloseDeferralFactory func(t *testing.T) (stores TaskCloseDeferralStores, orgID string, seed TaskCloseDeferralSeeder)

// RunTaskCloseDeferralConformance is the shared suite for a task's held
// close: TaskStore.CloseOrHoldSystem, ApplyHeldCloseSystem,
// CloseDeferralForTaskSystem, ListActiveCloseDeferralsSystem,
// ClearCloseDeferralSystem, the clear a bump, a requeue or an undo makes, the
// hold arm of EntityStore.CloseTerminalSystem, what holds a close: a working
// conversation, and a deliverable awaiting a person, and the expiry of the
// work an event started under a hold kept too long: when the hold began,
// ListExpiredCloseDeferralsSystem and ExpireHeldCloseSystem.
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
		// run is the blueprint's status, "" for running.
		cases := []struct {
			name    string
			run     string
			status  string
			stage   func(t *testing.T, seed TaskCloseDeferralSeeder, runID, convID string)
			working bool
		}{
			{name: "mid_flight", status: "", working: true},
			{name: "active_claim", status: domain.StatusOpen, working: true,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.Claim(t, convID) }},
			{name: "held_by_a_hand_back", status: "", working: true,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.HoldUntilLater(t, convID) }},
			{name: "between_steps", status: SeedConcluded, working: true},
			{name: "parked_on_a_person", status: domain.StatusOpen},
			{name: "concluded", run: "completed", status: SeedConcluded},
			{name: "failed", run: "failed", status: domain.StatusFailed},
			{name: "blueprint_called_off", status: "",
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, runID, _ string) { seed.CallOff(t, runID) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stores, orgID, seed := mk(t)
				taskID := seed.Task(t)
				run := tc.run
				if run == "" {
					run = "running"
				}
				runID, convID := seed.BlueprintAndConversation(t, taskID, run, tc.status)
				if tc.stage != nil {
					tc.stage(t, seed, runID, convID)
				}
				// Nothing else holds the task, so the close holds exactly
				// when the conversation is working.
				out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID))
				if err != nil {
					t.Fatalf("CloseOrHoldSystem: %v", err)
				}
				if out.Held != tc.working {
					t.Errorf("held = %v, want %v (working)", out.Held, tc.working)
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
			runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
			if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID)); err != nil {
				t.Fatalf("CloseOrHoldSystem(%s): %v", taskID, err)
			}
			seed.CallOff(t, runID)
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

	t.Run("Returning_a_task_to_the_queue_cancels_its_hold", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		// A requeue of a task still working, and an undo of a person's
		// dismiss of one: either way the person's gesture outranks the event.
		for _, door := range []string{"requeue", "undo"} {
			taskID := seed.Task(t)
			seed.BlueprintAndConversation(t, taskID, "running", "")
			if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID)); err != nil || !out.Held {
				t.Fatalf("%s: CloseOrHoldSystem = %+v (err %v), want held", door, out, err)
			}
			var ok bool
			var err error
			if door == "requeue" {
				ok, err = stores.Swipes.RequeueTask(ctx, orgID, taskID)
			} else {
				if _, dismissed, derr := stores.Swipes.RecordSwipe(ctx, orgID, taskID, "dismiss", 0, nil); derr != nil || !dismissed {
					t.Fatalf("undo: dismiss = %v (err %v)", dismissed, derr)
				}
				ok, err = stores.Swipes.UndoLastSwipe(ctx, orgID, taskID, stores.SwipeUserID)
			}
			if err != nil || !ok {
				t.Fatalf("%s = %v (err %v), want the task back in the queue", door, ok, err)
			}
			if d := held(t, stores.Tasks, orgID, taskID); d != nil {
				t.Errorf("held close after a %s = %+v, want none", door, *d)
			}
		}
	})

	t.Run("The_sweep_list_leaves_out_holds_something_still_holds", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		hold := func(taskID string) {
			t.Helper()
			if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, taskID)); err != nil || !out.Held {
				t.Fatalf("CloseOrHoldSystem(%s) = %+v (err %v), want held", taskID, out, err)
			}
		}
		working := seed.Task(t)
		seed.BlueprintAndConversation(t, working, "running", "")
		hold(working)
		awaiting := seed.Task(t)
		awaitingRun, awaitingConv := seed.BlueprintAndConversation(t, awaiting, "running", "")
		hold(awaiting)
		seed.CallOff(t, awaitingRun)
		seed.DraftPullRequest(t, awaitingConv, seed.EntityForTask(t, seed.Task(t)))
		free := seed.Task(t)
		freeRun, _ := seed.BlueprintAndConversation(t, free, "running", "")
		hold(free)
		seed.CallOff(t, freeRun)

		all, err := stores.Tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		var ids []string
		for _, d := range all {
			ids = append(ids, d.TaskID)
		}
		if !slices.Equal(ids, []string{free}) {
			t.Errorf("listed tasks = %v, want only %s (working %s and awaiting %s left out)", ids, free, working, awaiting)
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

	t.Run("A_deliverable_awaiting_a_person_holds_the_close", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		// A finished run's draft pull request, open on its own entity.
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		_, convID := seed.BlueprintAndConversation(t, taskID, "completed", SeedConcluded)
		seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
		out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID)
		if err != nil {
			t.Fatalf("CloseOrHoldSystem: %v", err)
		}
		if !outcomeEqual(out, db.TaskCloseOutcome{Held: true, NewlyHeld: true, AwaitingPerson: true}) {
			t.Errorf("close of a task awaiting a person = %+v, want held on the person", out)
		}
		if d := held(t, stores.Tasks, orgID, taskID); d == nil || d.EventID != eventID {
			t.Errorf("held close = %+v, want event %s", d, eventID)
		}
		apply := func() db.TaskCloseOutcome {
			t.Helper()
			out, err := stores.Tasks.ApplyHeldCloseSystem(ctx, orgID, taskID, eventID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed)
			if err != nil {
				t.Fatalf("ApplyHeldCloseSystem: %v", err)
			}
			return out
		}
		if out := apply(); !outcomeEqual(out, db.TaskCloseOutcome{Held: true, AwaitingPerson: true}) {
			t.Errorf("apply while the draft awaits the person = %+v, want held", out)
		}
		seed.ResolveDeliverables(t, convID)
		if out := apply(); !out.Closed {
			t.Errorf("apply once the person resolved it = %+v, want closed", out)
		}

		// A draft whose own pull request is closed awaits nobody, and
		// neither does one linked to no pull request.
		closedEntity := seed.EntityForTask(t, seed.Task(t))
		seed.CloseEntityRaw(t, closedEntity)
		for _, draftEntity := range []string{closedEntity, ""} {
			moot := seed.Task(t)
			_, mootConv := seed.BlueprintAndConversation(t, moot, "completed", SeedConcluded)
			seed.DraftPullRequest(t, mootConv, draftEntity)
			if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, moot, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, seed.Event(t, moot)); err != nil || !out.Closed {
				t.Errorf("close of a task whose draft is on entity %q, closed or none = %+v (err %v), want closed", draftEntity, out, err)
			}
		}

		// The terminating close decides its tasks after it closes the
		// entity: a deliverable on that entity awaits nobody, one on another
		// open entity holds its task.
		onIt := seed.Task(t)
		entityID := seed.EntityForTask(t, onIt)
		elsewhere := seed.TaskOnEntity(t, entityID, review)
		_, onItConv := seed.BlueprintAndConversation(t, onIt, "completed", SeedConcluded)
		_, elsewhereConv := seed.BlueprintAndConversation(t, elsewhere, "completed", SeedConcluded)
		seed.DraftPullRequest(t, onItConv, entityID)
		seed.DraftPullRequest(t, elsewhereConv, seed.EntityForTask(t, seed.Task(t)))
		_, seq := seed.EntityRow(t, entityID)
		res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review},
			"entity_closed", domain.EventGitHubPRMerged, seed.Event(t, onIt))
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !slices.Equal(res.ClosedTaskIDs, []string{onIt}) {
			t.Errorf("ClosedTaskIDs = %v, want [%s]", res.ClosedTaskIDs, onIt)
		}
		if len(res.HeldTasks) != 1 || res.HeldTasks[0].TaskID != elsewhere || !res.HeldTasks[0].AwaitingPerson {
			t.Errorf("HeldTasks = %+v, want %s held on the person", res.HeldTasks, elsewhere)
		}
	})

	t.Run("Clearing_a_hold_withdraws_its_close_notes", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		// "unheld" is a bump of a task that holds no close: it clears
		// nothing, so it withdraws nothing.
		for _, how := range []string{"bump", "clear", "unheld"} {
			taskID := seed.Task(t)
			eventID := seed.Event(t, taskID)
			_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
			if how != "unheld" {
				if _, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil {
					t.Fatalf("CloseOrHoldSystem: %v", err)
				}
			}
			for _, producer := range []string{domain.StagedInjectionProducerTaskClosing, domain.StagedInjectionProducerPRCoherence} {
				if _, err := stores.StagedInjections.AppendSystem(ctx, orgID, domain.StagedInjection{
					ConversationID: convID, Subtype: domain.MessageSubtypeInjectionSystemNote,
					Producer: producer, Body: producer, Provenance: domain.NoteProvenance{EventID: eventID},
				}); err != nil {
					t.Fatalf("AppendSystem(%s): %v", producer, err)
				}
			}
			if how == "clear" {
				if cleared, err := stores.Tasks.ClearCloseDeferralSystem(ctx, orgID, taskID, eventID); err != nil || !cleared {
					t.Fatalf("ClearCloseDeferralSystem = %v (err %v), want cleared", cleared, err)
				}
			} else if _, err := stores.Tasks.BumpSystem(ctx, orgID, taskID, seed.Event(t, taskID)); err != nil {
				t.Fatalf("BumpSystem: %v", err)
			}
			left, err := stores.StagedInjections.FlushPendingSystem(ctx, orgID, convID)
			if err != nil {
				t.Fatalf("FlushPendingSystem: %v", err)
			}
			want := 1
			if how == "unheld" {
				want = 2
			}
			if len(left) != want {
				t.Errorf("%s: still staged = %+v, want %d", how, left, want)
			} else if want == 1 && left[0].Producer != domain.StagedInjectionProducerPRCoherence {
				t.Errorf("%s: still staged = %+v, want only the pull request note", how, left)
			}
		}
	})

	// The expiry's cases share these. A hold is old once AgeHold has moved it
	// back past an hour, which every expiry below asks about.
	const holdFor = time.Hour
	const aged = 2 * time.Hour
	holdNow := func(t *testing.T, stores TaskCloseDeferralStores, orgID, taskID, eventID string) {
		t.Helper()
		if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed, eventID); err != nil || !out.Held {
			t.Fatalf("CloseOrHoldSystem(%s) = %+v (err %v), want held", taskID, out, err)
		}
	}
	expiredIDs := func(t *testing.T, tasks db.TaskStore) []string {
		t.Helper()
		all, err := tasks.ListExpiredCloseDeferralsSystem(ctx, holdFor)
		if err != nil {
			t.Fatalf("ListExpiredCloseDeferralsSystem: %v", err)
		}
		var ids []string
		for _, d := range all {
			ids = append(ids, d.TaskID)
		}
		return ids
	}
	expire := func(t *testing.T, tasks db.TaskStore, orgID, taskID, eventID string) []string {
		t.Helper()
		expired, err := tasks.ExpireHeldCloseSystem(ctx, orgID, taskID, eventID, holdFor)
		if err != nil {
			t.Fatalf("ExpireHeldCloseSystem: %v", err)
		}
		return expired
	}
	listedIdle := func(t *testing.T, tasks db.TaskStore, taskID string) bool {
		t.Helper()
		all, err := tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		return slices.ContainsFunc(all, func(d domain.TaskCloseDeferral) bool { return d.TaskID == taskID })
	}
	personWrites := func(t *testing.T, stores TaskCloseDeferralStores, orgID, conversationID string) {
		t.Helper()
		if _, err := stores.Conversations.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: conversationID, UserID: stores.SwipeUserID, Role: "user", Content: "one more thing",
		}); err != nil {
			t.Fatalf("InsertMessage: %v", err)
		}
	}

	t.Run("A_hold_keeps_the_time_it_began", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		seed.BlueprintAndConversation(t, taskID, "running", "")
		holdNow(t, stores, orgID, taskID, seed.Event(t, taskID))
		if ids := expiredIDs(t, stores.Tasks); len(ids) != 0 {
			t.Fatalf("a hold that just began is listed as expired: %v", ids)
		}
		seed.AgeHold(t, taskID, aged)

		// A later closing event takes the hold's place and keeps its time.
		holdNow(t, stores, orgID, taskID, seed.Event(t, taskID))
		if ids := expiredIDs(t, stores.Tasks); !slices.Equal(ids, []string{taskID}) {
			t.Errorf("expired after a replacing event = %v, want [%s]", ids, taskID)
		}

		// A hold cancelled and held again starts a new time.
		if _, err := stores.Tasks.BumpSystem(ctx, orgID, taskID, seed.Event(t, taskID)); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		holdNow(t, stores, orgID, taskID, seed.Event(t, taskID))
		if ids := expiredIDs(t, stores.Tasks); len(ids) != 0 {
			t.Errorf("expired after a cancel and a new hold = %v, want none", ids)
		}
	})

	t.Run("The_expired_list_reads_only_work_an_event_started", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		agedHold := func(taskID string) {
			t.Helper()
			holdNow(t, stores, orgID, taskID, seed.Event(t, taskID))
			seed.AgeHold(t, taskID, aged)
		}
		event := seed.Task(t)
		seed.BlueprintAndConversation(t, event, "running", "")
		agedHold(event)

		young := seed.Task(t)
		seed.BlueprintAndConversation(t, young, "running", "")
		holdNow(t, stores, orgID, young, seed.Event(t, young))

		delegated := seed.Task(t)
		seed.BareConversation(t, delegated, "")
		agedHold(delegated)

		wrote := seed.Task(t)
		_, wroteConv := seed.BlueprintAndConversation(t, wrote, "running", "")
		personWrites(t, stores, orgID, wroteConv)
		agedHold(wrote)

		stopping := seed.Task(t)
		_, stoppingConv := seed.BlueprintAndConversation(t, stopping, "running", "")
		seed.Claim(t, stoppingConv)
		agedHold(stopping)
		if ok, err := stores.Conversations.RequestStopSystem(ctx, orgID, stoppingConv, "", "", ""); err != nil || !ok {
			t.Fatalf("RequestStopSystem = %v (err %v), want requested", ok, err)
		}

		idle := seed.Task(t)
		idleRun, _ := seed.BlueprintAndConversation(t, idle, "running", "")
		agedHold(idle)
		seed.CallOff(t, idleRun)

		if ids := expiredIDs(t, stores.Tasks); !slices.Equal(ids, []string{event}) {
			t.Errorf("expired = %v, want only %s (young %s, delegated %s, wrote %s, stopping %s, idle %s left out)",
				ids, event, young, delegated, wrote, stopping, idle)
		}
	})

	t.Run("ExpireHeldClose_calls_off_the_work_an_event_started_and_keeps_the_close", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		eventRun, eventConv := seed.BlueprintAndConversation(t, taskID, "running", "")
		// A person's own delegation on the same task is not the event's work.
		seed.BareConversation(t, taskID, "")
		holdNow(t, stores, orgID, taskID, eventID)
		seed.AgeHold(t, taskID, aged)

		if got := expire(t, stores.Tasks, orgID, taskID, eventID); !slices.Equal(got, []string{eventConv}) {
			t.Errorf("expired = %v, want [%s]", got, eventConv)
		}
		if !seed.CancelRequested(t, eventRun) {
			t.Error("the expired run was not called off")
		}
		if s := seed.TaskStatus(t, taskID); s != "queued" {
			t.Errorf("task status = %q, want queued", s)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close audit rows = %d, want 0", n)
		}
		if d := held(t, stores.Tasks, orgID, taskID); d == nil || d.EventID != eventID {
			t.Errorf("held close = %+v, want event %s", d, eventID)
		}
		if listedIdle(t, stores.Tasks, taskID) {
			t.Error("the sweep would apply the close while the person's work goes on")
		}
	})

	t.Run("After_the_expiry_the_close_applies_once_nothing_holds_it", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		// The expired run called off and nothing else on the task: the
		// sweep's ordinary apply closes it. With a draft the run left for a
		// person, the close waits on the person.
		for _, draft := range []bool{false, true} {
			taskID := seed.Task(t)
			eventID := seed.Event(t, taskID)
			_, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
			if draft {
				seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
			}
			holdNow(t, stores, orgID, taskID, eventID)
			seed.AgeHold(t, taskID, aged)
			if got := expire(t, stores.Tasks, orgID, taskID, eventID); !slices.Equal(got, []string{convID}) {
				t.Fatalf("draft %v: expired = %v, want [%s]", draft, got, convID)
			}

			if listed := listedIdle(t, stores.Tasks, taskID); listed == draft {
				t.Errorf("draft %v: listed for the apply = %v, want %v", draft, listed, !draft)
			}
			out, err := stores.Tasks.ApplyHeldCloseSystem(ctx, orgID, taskID, eventID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed)
			if err != nil {
				t.Fatalf("ApplyHeldCloseSystem: %v", err)
			}
			if out.Closed == draft {
				t.Errorf("draft %v: apply after the expiry = %+v", draft, out)
			}
		}
	})

	t.Run("ExpireHeldClose_leaves_a_hold_it_may_not_expire", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		cases := []struct {
			name  string
			stage func(t *testing.T, taskID string) (eventID, runID string)
		}{
			{name: "young", stage: func(t *testing.T, taskID string) (string, string) {
				runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
				eventID := seed.Event(t, taskID)
				holdNow(t, stores, orgID, taskID, eventID)
				return eventID, runID
			}},
			{name: "replaced", stage: func(t *testing.T, taskID string) (string, string) {
				runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
				read := seed.Event(t, taskID)
				holdNow(t, stores, orgID, taskID, read)
				holdNow(t, stores, orgID, taskID, seed.Event(t, taskID))
				seed.AgeHold(t, taskID, aged)
				return read, runID
			}},
			{name: "only_a_person_works", stage: func(t *testing.T, taskID string) (string, string) {
				runID, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
				personWrites(t, stores, orgID, convID)
				eventID := seed.Event(t, taskID)
				holdNow(t, stores, orgID, taskID, eventID)
				seed.AgeHold(t, taskID, aged)
				return eventID, runID
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				taskID := seed.Task(t)
				eventID, runID := tc.stage(t, taskID)
				if got := expire(t, stores.Tasks, orgID, taskID, eventID); len(got) != 0 {
					t.Errorf("expired = %v, want none", got)
				}
				if seed.CancelRequested(t, runID) {
					t.Error("the run was called off")
				}
				if s := seed.TaskStatus(t, taskID); s != "queued" {
					t.Errorf("task status = %q, want queued", s)
				}
				if held(t, stores.Tasks, orgID, taskID) == nil {
					t.Error("the hold was cleared")
				}
			})
		}
	})
}

func outcomeEqual(a, b db.TaskCloseOutcome) bool {
	return a.Closed == b.Closed && a.Held == b.Held && a.NewlyHeld == b.NewlyHeld && a.AwaitingPerson == b.AwaitingPerson &&
		slices.Equal(a.ActiveConversationIDs, b.ActiveConversationIDs) &&
		slices.Equal(a.WorkingConversationIDs, b.WorkingConversationIDs)
}
