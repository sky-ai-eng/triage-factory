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
// its transaction, the staged notes a cleared hold withdraws, the requeue that
// returns a task to the queue, and the claim phase the working predicate
// reads. The reopen suite runs on the same stores.
type TaskCloseDeferralStores struct {
	Tasks            db.TaskStore
	Entities         db.EntityStore
	StagedInjections db.StagedInjectionStore
	Conversations    db.ConversationStore
	// UserID is a person who can claim a task.
	UserID string
}

// TaskCloseDeferralSeeder stages the graph a held close spans — task, closing
// event, blueprint run and its conversation, the entity — through the
// terminating-close seeder, plus the conversation states the working
// predicate tells apart and the deliverable that holds a close.
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
}

// TaskCloseDeferralFactory hands the suite a fresh database per subtest: the
// cross-org list read must see only what the subtest staged.
type TaskCloseDeferralFactory func(t *testing.T) (stores TaskCloseDeferralStores, orgID string, seed TaskCloseDeferralSeeder)

// RunTaskCloseDeferralConformance is the shared suite for a task's held
// close: TaskStore.CloseOrHoldSystem, ApplyHeldCloseSystem,
// CloseDeferralForTaskSystem, ListActiveCloseDeferralsSystem,
// ClearCloseDeferralSystem, the clear a bump or a requeue makes, and the
// hold arm of EntityStore.CloseTerminalSystem. One thing holds a close: a
// deliverable awaiting a person, on a pull request still open. A
// conversation working on the task never does, and the suite pins both
// halves.
func RunTaskCloseDeferralConformance(t *testing.T, mk TaskCloseDeferralFactory) {
	t.Helper()
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed
	const review = domain.EventGitHubPRReviewChangesRequested
	const passed = domain.EventGitHubPRCICheckPassed

	held := func(t *testing.T, tasks db.TaskStore, orgID, taskID string) *domain.TaskCloseDeferral {
		t.Helper()
		d, err := tasks.CloseDeferralForTaskSystem(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("CloseDeferralForTaskSystem: %v", err)
		}
		return d
	}
	closeOrHold := func(t *testing.T, tasks db.TaskStore, orgID, taskID, eventID string) db.TaskCloseOutcome {
		t.Helper()
		out, err := tasks.CloseOrHoldSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, passed, eventID)
		if err != nil {
			t.Fatalf("CloseOrHoldSystem(%s): %v", taskID, err)
		}
		return out
	}
	apply := func(t *testing.T, tasks db.TaskStore, orgID, taskID, eventID string) db.TaskCloseOutcome {
		t.Helper()
		out, err := tasks.ApplyHeldCloseSystem(ctx, orgID, taskID, eventID, db.SiblingEventCloseReason, passed)
		if err != nil {
			t.Fatalf("ApplyHeldCloseSystem(%s): %v", taskID, err)
		}
		return out
	}
	// awaiting stages a task holding a close on eventID: a finished run's
	// draft pull request, open on an entity of its own, awaits a person.
	awaiting := func(t *testing.T, stores TaskCloseDeferralStores, orgID string, seed TaskCloseDeferralSeeder) (taskID, eventID, convID string) {
		t.Helper()
		taskID = seed.Task(t)
		eventID = seed.Event(t, taskID)
		_, convID = seed.BlueprintAndConversation(t, taskID, "completed", SeedConcluded)
		seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
		if out := closeOrHold(t, stores.Tasks, orgID, taskID, eventID); !out.Held {
			t.Fatalf("CloseOrHoldSystem(%s) = %+v, want held on the draft", taskID, out)
		}
		return taskID, eventID, convID
	}
	listed := func(t *testing.T, tasks db.TaskStore) []string {
		t.Helper()
		all, err := tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		var ids []string
		for _, d := range all {
			ids = append(ids, d.TaskID)
		}
		return ids
	}

	t.Run("A_working_conversation_never_holds_a_close", func(t *testing.T) {
		// run is the blueprint's status, "" for running.
		cases := []struct {
			name   string
			run    string
			status string
			stage  func(t *testing.T, seed TaskCloseDeferralSeeder, runID, convID string)
			// phase is the active claim's phase, set after stage.
			phase string
			// working says the close reports the conversation for its caller
			// to tell.
			working bool
		}{
			{name: "mid_flight", status: "", working: true},
			{name: "active_claim", status: domain.StatusOpen, working: true,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.Claim(t, convID) }},
			// A warm claim holds its conversation, but its turn ended and wrote
			// its verdict, so nothing works under it.
			{name: "warm_claim", run: string(domain.BlueprintRunStatusCompleted), status: SeedConcluded, phase: domain.ClaimPhaseWarm,
				stage: func(t *testing.T, seed TaskCloseDeferralSeeder, _, convID string) { seed.Claim(t, convID) }},
			// A follow-up resumed the conversation onto its warm claim, whose
			// engagement is about to drive it.
			{name: "warm_claim_resumed", run: string(domain.BlueprintRunStatusCompleted), status: "", phase: domain.ClaimPhaseWarm, working: true,
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
				if tc.phase != "" {
					if _, err := stores.Conversations.SetActiveClaimPhaseSystem(ctx, orgID, convID, tc.phase); err != nil {
						t.Fatalf("SetActiveClaimPhaseSystem(%s): %v", tc.phase, err)
					}
				}
				before := seed.WorkState(t, runID, convID)

				out := closeOrHold(t, stores.Tasks, orgID, taskID, seed.Event(t, taskID))
				want := db.TaskCloseOutcome{Closed: true}
				if tc.working {
					want.WorkingConversationIDs = []string{convID}
				}
				if !outcomeEqual(out, want) {
					t.Errorf("close = %+v, want %+v", out, want)
				}
				if s := seed.TaskStatus(t, taskID); s != "done" {
					t.Errorf("task status = %q, want done", s)
				}
				if d := held(t, stores.Tasks, orgID, taskID); d != nil {
					t.Errorf("held close = %+v, want none", *d)
				}
				if after := seed.WorkState(t, runID, convID); after != before {
					t.Errorf("the close changed the work on the task:\n before %+v\n after  %+v", before, after)
				}
			})
		}
	})

	t.Run("CloseOrHold_holds_on_a_deliverable_awaiting_a_person_and_closes_otherwise", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		holding := seed.Task(t)
		first := seed.Event(t, holding)
		second := seed.Event(t, holding)
		// A finished run left a draft for a person, and a later run on the
		// task is working when the closing event arrives.
		_, draftConv := seed.BlueprintAndConversation(t, holding, "completed", SeedConcluded)
		seed.DraftPullRequest(t, draftConv, seed.EntityForTask(t, seed.Task(t)))
		workingRun, workingConv := seed.BlueprintAndConversation(t, holding, "running", "")
		before := seed.WorkState(t, workingRun, workingConv)

		out := closeOrHold(t, stores.Tasks, orgID, holding, first)
		want := db.TaskCloseOutcome{Held: true, NewlyHeld: true, WorkingConversationIDs: []string{workingConv}}
		if !outcomeEqual(out, want) {
			t.Errorf("first close = %+v, want %+v", out, want)
		}
		if s := seed.TaskStatus(t, holding); s != "queued" {
			t.Errorf("held task status = %q, want queued", s)
		}
		if n := seed.CloseAuditCount(t, holding); n != 0 {
			t.Errorf("held task close audit rows = %d, want 0", n)
		}
		if d := held(t, stores.Tasks, orgID, holding); d == nil || *d != (domain.TaskCloseDeferral{OrgID: orgID, TaskID: holding, EventID: first}) {
			t.Errorf("held close = %+v, want event %s", d, first)
		}

		// A second closing event while the close is held takes its place.
		out = closeOrHold(t, stores.Tasks, orgID, holding, second)
		if !out.Held || out.NewlyHeld {
			t.Errorf("second close = %+v, want held, not newly", out)
		}
		if d := held(t, stores.Tasks, orgID, holding); d == nil || d.EventID != second {
			t.Errorf("held close = %+v, want event %s", d, second)
		}
		// A held close is the system's bookkeeping: it changes nothing a
		// person acts on, so a person's pending write on the task stays good.
		if v := TaskVersion(t, stores.Tasks, orgID, holding); v != 1 {
			t.Errorf("held task version = %d, want 1 (unchanged by the holds)", v)
		}
		if after := seed.WorkState(t, workingRun, workingConv); after != before {
			t.Errorf("the hold changed the work on the task:\n before %+v\n after  %+v", before, after)
		}

		// A hold names its closing event, so a close with none closes.
		unnamed := seed.Task(t)
		_, unnamedConv := seed.BlueprintAndConversation(t, unnamed, "completed", SeedConcluded)
		seed.DraftPullRequest(t, unnamedConv, seed.EntityForTask(t, seed.Task(t)))
		if out, err := stores.Tasks.CloseOrHoldSystem(ctx, orgID, unnamed, "reconciled", "", ""); err != nil || !out.Closed {
			t.Errorf("close naming no event = %+v (err %v), want closed", out, err)
		}

		plain := seed.Task(t)
		plainEvent := seed.Event(t, plain)
		out = closeOrHold(t, stores.Tasks, orgID, plain, plainEvent)
		if !outcomeEqual(out, db.TaskCloseOutcome{Closed: true}) {
			t.Errorf("close with nothing awaiting a person = %+v, want closed", out)
		}
		if s := seed.TaskStatus(t, plain); s != "done" {
			t.Errorf("task status = %q, want done", s)
		}
		if v := TaskVersion(t, stores.Tasks, orgID, plain); v != 2 {
			t.Errorf("closed task version = %d, want 2", v)
		}
		if n := seed.CloseAuditCount(t, plain); n != 1 {
			t.Errorf("close audit rows = %d, want 1", n)
		}

		out = closeOrHold(t, stores.Tasks, orgID, plain, plainEvent)
		if !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("close of a terminal task = %+v, want neither", out)
		}
	})

	t.Run("CloseOnEvent_never_holds", func(t *testing.T) {
		// A reassignment's close: the draft that would hold any other
		// event's close does not hold this one.
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		_, convID := seed.BlueprintAndConversation(t, taskID, "completed", SeedConcluded)
		seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))

		out, err := stores.Tasks.CloseOnEventSystem(ctx, orgID, taskID, db.ReassignedCloseReason, domain.EventJiraIssueAssigned, eventID)
		if err != nil {
			t.Fatalf("CloseOnEventSystem: %v", err)
		}
		if !outcomeEqual(out, db.TaskCloseOutcome{Closed: true}) {
			t.Errorf("close = %+v, want closed", out)
		}
		if d := held(t, stores.Tasks, orgID, taskID); d != nil {
			t.Errorf("held close = %+v, want none", *d)
		}
	})

	t.Run("A_deliverable_only_holds_while_someone_can_act_on_it", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		// A draft whose own pull request is closed awaits nobody, and
		// neither does one linked to no pull request.
		closedEntity := seed.EntityForTask(t, seed.Task(t))
		seed.CloseEntityRaw(t, closedEntity)
		for _, draftEntity := range []string{closedEntity, ""} {
			moot := seed.Task(t)
			_, mootConv := seed.BlueprintAndConversation(t, moot, "completed", SeedConcluded)
			seed.DraftPullRequest(t, mootConv, draftEntity)
			if out := closeOrHold(t, stores.Tasks, orgID, moot, seed.Event(t, moot)); !out.Closed {
				t.Errorf("close of a task whose draft is on entity %q, closed or none = %+v, want closed", draftEntity, out)
			}
		}
		// One a person already resolved awaits nobody either.
		resolved := seed.Task(t)
		_, resolvedConv := seed.BlueprintAndConversation(t, resolved, "completed", SeedConcluded)
		seed.DraftPullRequest(t, resolvedConv, seed.EntityForTask(t, seed.Task(t)))
		seed.ResolveDeliverables(t, resolvedConv)
		if out := closeOrHold(t, stores.Tasks, orgID, resolved, seed.Event(t, resolved)); !out.Closed {
			t.Errorf("close of a task whose draft a person resolved = %+v, want closed", out)
		}
	})

	t.Run("CloseTerminal_holds_tasks_awaiting_a_person_and_closes_the_rest", func(t *testing.T) {
		// The terminating close decides its tasks after it closes the
		// entity: a deliverable on that entity awaits nobody, one on another
		// open entity holds its task, and a working conversation holds
		// nothing.
		stores, orgID, seed := mk(t)
		onIt := seed.Task(t)
		entityID := seed.EntityForTask(t, onIt)
		elsewhere := seed.TaskOnEntity(t, entityID, review)
		worked := seed.TaskOnEntity(t, entityID, domain.EventGitHubPRConflicts)
		_, onItConv := seed.BlueprintAndConversation(t, onIt, "completed", SeedConcluded)
		_, elsewhereConv := seed.BlueprintAndConversation(t, elsewhere, "completed", SeedConcluded)
		workedRun, workedConv := seed.BlueprintAndConversation(t, worked, "running", "")
		seed.DraftPullRequest(t, onItConv, entityID)
		seed.DraftPullRequest(t, elsewhereConv, seed.EntityForTask(t, seed.Task(t)))
		before := seed.WorkState(t, workedRun, workedConv)
		eventID := seed.Event(t, onIt)
		_, seq := seed.EntityRow(t, entityID)

		res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review, domain.EventGitHubPRConflicts},
			"entity_closed", domain.EventGitHubPRMerged, eventID)
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed {
			t.Fatal("entity did not close")
		}
		closed := slices.Clone(res.ClosedTaskIDs)
		slices.Sort(closed)
		wantClosed := []string{onIt, worked}
		slices.Sort(wantClosed)
		if !slices.Equal(closed, wantClosed) {
			t.Errorf("ClosedTaskIDs = %v, want %v", res.ClosedTaskIDs, wantClosed)
		}
		if !slices.Equal(res.WorkingConversationIDs[worked], []string{workedConv}) {
			t.Errorf("working conversations for %s = %v, want [%s]", worked, res.WorkingConversationIDs[worked], workedConv)
		}
		if len(res.HeldTasks) != 1 || res.HeldTasks[0].TaskID != elsewhere || !res.HeldTasks[0].NewlyHeld {
			t.Errorf("HeldTasks = %+v, want %s newly held", res.HeldTasks, elsewhere)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "closed" {
			t.Errorf("entity state = %q, want closed", state)
		}
		if s := seed.TaskStatus(t, elsewhere); s != "queued" {
			t.Errorf("held task status = %q, want queued", s)
		}
		if n := seed.CloseAuditCount(t, elsewhere); n != 0 {
			t.Errorf("held task close audit rows = %d, want 0", n)
		}
		if d := held(t, stores.Tasks, orgID, elsewhere); d == nil || d.EventID != eventID {
			t.Errorf("held close = %+v, want event %s", d, eventID)
		}
		if after := seed.WorkState(t, workedRun, workedConv); after != before {
			t.Errorf("the close changed the work on the task:\n before %+v\n after  %+v", before, after)
		}
		// Open on its closed entity by design: not the stranded task the
		// terminal-state checker counts.
		stranded, err := stores.Tasks.ListOpenOnClosedEntitiesSystem(ctx, orgID)
		if err != nil {
			t.Fatalf("ListOpenOnClosedEntitiesSystem: %v", err)
		}
		if slices.Contains(stranded, elsewhere) {
			t.Errorf("the held task is counted as stranded on its closed entity: %v", stranded)
		}
	})

	t.Run("Bump_clears_the_hold", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID, _, _ := awaiting(t, stores, orgID, seed)

		if _, err := stores.Tasks.BumpSystem(ctx, orgID, taskID, seed.Event(t, taskID)); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		if d := held(t, stores.Tasks, orgID, taskID); d != nil {
			t.Errorf("held close after a bump = %+v, want none", *d)
		}
	})

	t.Run("Reads_see_holds_on_active_tasks_only", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		holding, _, holdingConv := awaiting(t, stores, orgID, seed)
		closed, _, closedConv := awaiting(t, stores, orgID, seed)
		plain := seed.Task(t)
		// Resolved, so the sweep's list would return either hold.
		seed.ResolveDeliverables(t, holdingConv)
		seed.ResolveDeliverables(t, closedConv)
		// A close by any other path leaves the column as it is; the reads
		// are what keep it from being read again.
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, closed, "run_completed", ""); err != nil {
			t.Fatalf("CloseSystem: %v", err)
		}

		all, err := stores.Tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		if ids := listed(t, stores.Tasks); !slices.Equal(ids, []string{holding}) {
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
		// A requeue of a task holding a close, and a requeue of a person's
		// dismiss of one: either way the person's gesture outranks the event.
		for _, door := range []string{"requeue", "requeue after a dismiss"} {
			taskID, _, _ := awaiting(t, stores, orgID, seed)
			if door != "requeue" {
				if _, err := stores.Tasks.CloseByUser(ctx, orgID, taskID, "dismissed", TaskVersion(t, stores.Tasks, orgID, taskID)); err != nil {
					t.Fatalf("%s: CloseByUser = %v", door, err)
				}
			}
			ok, err := stores.Tasks.RequeueTask(ctx, orgID, taskID, TaskVersion(t, stores.Tasks, orgID, taskID))
			if err != nil || !ok {
				t.Fatalf("%s = %v (err %v), want the task back in the queue", door, ok, err)
			}
			if d := held(t, stores.Tasks, orgID, taskID); d != nil {
				t.Errorf("held close after a %s = %+v, want none", door, *d)
			}
		}
	})

	t.Run("A_persons_close_never_holds", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		for _, status := range []string{"dismissed", "done"} {
			taskID := seed.Task(t)
			_, convID := seed.BlueprintAndConversation(t, taskID, "completed", SeedConcluded)
			seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
			if _, err := stores.Tasks.CloseByUser(ctx, orgID, taskID, status, TaskVersion(t, stores.Tasks, orgID, taskID)); err != nil {
				t.Fatalf("CloseByUser(%s): %v", status, err)
			}
			if s := seed.TaskStatus(t, taskID); s != status {
				t.Errorf("task status = %q, want %s", s, status)
			}
		}
	})

	t.Run("The_sweep_list_leaves_out_holds_a_deliverable_still_holds", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		still, _, _ := awaiting(t, stores, orgID, seed)
		free, _, freeConv := awaiting(t, stores, orgID, seed)
		seed.ResolveDeliverables(t, freeConv)
		// Work on the task does not keep a hold from the sweep: its draft is
		// resolved and a later run is mid-flight.
		worked, _, workedConv := awaiting(t, stores, orgID, seed)
		seed.ResolveDeliverables(t, workedConv)
		seed.BlueprintAndConversation(t, worked, "running", "")

		got := listed(t, stores.Tasks)
		slices.Sort(got)
		want := []string{free, worked}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("listed tasks = %v, want %v (awaiting %s left out)", got, want, still)
		}
	})

	t.Run("Clear_compares_on_the_event_it_read", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID, read, _ := awaiting(t, stores, orgID, seed)
		// The hold moves on to a newer event between the sweep's read and
		// its clear.
		newer := seed.Event(t, taskID)
		closeOrHold(t, stores.Tasks, orgID, taskID, newer)

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

		stale, first, staleConv := awaiting(t, stores, orgID, seed)
		if out := apply(t, stores.Tasks, orgID, stale, first); !outcomeEqual(out, db.TaskCloseOutcome{Held: true}) {
			t.Errorf("apply while the draft awaits the person = %+v, want held", out)
		}
		// The hold the sweep read is replaced, then cancelled, and by the
		// time the cancelled one is applied the person has resolved the
		// draft.
		second := seed.Event(t, stale)
		closeOrHold(t, stores.Tasks, orgID, stale, second)
		if out := apply(t, stores.Tasks, orgID, stale, first); !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("apply of a replaced hold = %+v, want nothing", out)
		}
		if _, err := stores.Tasks.BumpSystem(ctx, orgID, stale, seed.Event(t, stale)); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		seed.ResolveDeliverables(t, staleConv)
		if out := apply(t, stores.Tasks, orgID, stale, second); !outcomeEqual(out, db.TaskCloseOutcome{}) {
			t.Errorf("apply of a cancelled hold = %+v, want nothing", out)
		}
		if s := seed.TaskStatus(t, stale); s == "done" {
			t.Error("a cancelled hold closed the task")
		}
		if d := held(t, stores.Tasks, orgID, stale); d != nil {
			t.Errorf("the cancelled hold came back: %+v", *d)
		}

		// The current hold, once the person resolved the draft, applies
		// under a conversation still working on the task, which it leaves
		// working and reports.
		current, eventID, currentConv := awaiting(t, stores, orgID, seed)
		workingRun, workingConv := seed.BlueprintAndConversation(t, current, "running", "")
		before := seed.WorkState(t, workingRun, workingConv)
		seed.ResolveDeliverables(t, currentConv)
		if out := apply(t, stores.Tasks, orgID, current, eventID); !outcomeEqual(out, db.TaskCloseOutcome{Closed: true, WorkingConversationIDs: []string{workingConv}}) {
			t.Errorf("apply of the current hold = %+v, want closed, telling [%s]", out, workingConv)
		}
		if s := seed.TaskStatus(t, current); s != "done" {
			t.Errorf("task status = %q, want done", s)
		}
		if n := seed.CloseAuditCount(t, current); n != 1 {
			t.Errorf("close audit rows = %d, want 1", n)
		}
		if after := seed.WorkState(t, workingRun, workingConv); after != before {
			t.Errorf("the applied close changed the work on the task:\n before %+v\n after  %+v", before, after)
		}
		got, err := stores.Tasks.Get(ctx, orgID, current)
		if err != nil || got == nil {
			t.Fatalf("Get = %+v (err %v)", got, err)
		}
		if got.CloseReason != db.SiblingEventCloseReason || got.CloseEventType != passed {
			t.Errorf("close = (%q, %q), want (%s, %s)", got.CloseReason, got.CloseEventType, db.SiblingEventCloseReason, passed)
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
				seed.DraftPullRequest(t, convID, seed.EntityForTask(t, seed.Task(t)))
				if out := closeOrHold(t, stores.Tasks, orgID, taskID, eventID); !out.Held {
					t.Fatalf("%s: CloseOrHoldSystem = %+v, want held", how, out)
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
}

func outcomeEqual(a, b db.TaskCloseOutcome) bool {
	return a.Closed == b.Closed && a.Held == b.Held && a.NewlyHeld == b.NewlyHeld &&
		slices.Equal(a.WorkingConversationIDs, b.WorkingConversationIDs)
}
