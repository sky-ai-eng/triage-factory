package dbtest

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskCloseDeferralStores are the stores the close-deferral suite drives:
// the deferral's own writes and read on TaskStore, the terminating close
// that stamps it in its transaction, and the two reads the router's
// run-cause check makes.
type TaskCloseDeferralStores struct {
	Tasks           db.TaskStore
	Entities        db.EntityStore
	Blueprints      db.BlueprintStore
	ExternalActions db.ExternalActionStore
}

// TaskCloseDeferralSeeder stages the graph a deferral spans — task, closing
// event, blueprint run and its conversation, the entity — through the
// terminating-close seeder, plus a bare write of the two deferral columns.
type TaskCloseDeferralSeeder struct {
	EntityTerminalCloseSeeder

	// SetDeferralColumns writes the task's close_deferred_event_id and
	// close_deferred_blueprint_run_id with a bare UPDATE, an empty value as
	// NULL, and returns what the database answered: the pairing constraint
	// is a property of the table, so it is tested beneath every store
	// method.
	SetDeferralColumns func(t *testing.T, taskID, eventID, blueprintRunID string) error
}

// TaskCloseDeferralFactory hands the suite a fresh database per subtest: the
// cross-org list read must see only what the subtest staged.
type TaskCloseDeferralFactory func(t *testing.T) (stores TaskCloseDeferralStores, orgID string, seed TaskCloseDeferralSeeder)

// RunTaskCloseDeferralConformance is the shared suite for a task's deferred
// close: TaskStore.DeferCloseSystem, ListActiveCloseDeferralsSystem and
// ClearCloseDeferralSystem, the deferral arm of EntityStore.
// CloseTerminalSystem, the tasks_close_deferral_paired rule, and the two
// reads the router's run-cause check makes (BlueprintStore.
// RunningRunForTaskSystem, ExternalActionStore.
// ListAgentActionsForBlueprintRunSystem).
func RunTaskCloseDeferralConformance(t *testing.T, mk TaskCloseDeferralFactory) {
	t.Helper()
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed
	const review = domain.EventGitHubPRReviewChangesRequested

	listed := func(t *testing.T, tasks db.TaskStore, taskID string) *domain.TaskCloseDeferral {
		t.Helper()
		all, err := tasks.ListActiveCloseDeferralsSystem(ctx)
		if err != nil {
			t.Fatalf("ListActiveCloseDeferralsSystem: %v", err)
		}
		for i := range all {
			if all[i].TaskID == taskID {
				return &all[i]
			}
		}
		return nil
	}

	t.Run("DeferClose_returns_the_stored_row_and_writes_nothing_else", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")

		got, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, eventID, runID)
		if err != nil {
			t.Fatalf("DeferCloseSystem: %v", err)
		}
		if got == nil {
			t.Fatal("DeferCloseSystem on an active task returned no row")
		}
		want := domain.TaskCloseDeferral{OrgID: orgID, TaskID: taskID, EventID: eventID, BlueprintRunID: runID}
		if *got != want {
			t.Errorf("returned %+v, want %+v", *got, want)
		}
		AssertWriteReturnedStoredRow(t, "DeferCloseSystem", *got, func() (*domain.TaskCloseDeferral, error) {
			return listed(t, stores.Tasks, taskID), nil
		})
		if s := seed.TaskStatus(t, taskID); s != "queued" {
			t.Errorf("task status = %q, want queued: a deferral does not move the task", s)
		}
		if n := seed.CloseAuditCount(t, taskID); n != 0 {
			t.Errorf("close audit rows = %d, want 0: the audit row is written when the deferred close applies", n)
		}
		if seed.CancelRequested(t, runID) {
			t.Error("the run's cancel intent was set: a deferral stops nothing")
		}
	})

	t.Run("DeferClose_overwrites_the_deferral_the_task_holds", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		first := seed.Event(t, taskID)
		second := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")

		if _, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, first, runID); err != nil {
			t.Fatalf("first DeferCloseSystem: %v", err)
		}
		got, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, second, runID)
		if err != nil || got == nil {
			t.Fatalf("second DeferCloseSystem: got=%v err=%v", got, err)
		}
		if d := listed(t, stores.Tasks, taskID); d == nil || d.EventID != second {
			t.Errorf("listed deferral = %+v, want event %s", d, second)
		}
	})

	t.Run("DeferClose_on_a_closed_task_writes_nothing", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, taskID, "user_dismissed", ""); err != nil {
			t.Fatalf("CloseSystem: %v", err)
		}

		got, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, eventID, runID)
		if err != nil {
			t.Fatalf("DeferCloseSystem: %v", err)
		}
		if got != nil {
			t.Errorf("DeferCloseSystem on a closed task returned %+v, want nil", *got)
		}
	})

	t.Run("List_returns_only_deferrals_on_active_tasks", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		held := seed.Task(t)
		closed := seed.Task(t)
		plain := seed.Task(t)
		for _, taskID := range []string{held, closed} {
			eventID := seed.Event(t, taskID)
			runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
			if _, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, eventID, runID); err != nil {
				t.Fatalf("DeferCloseSystem(%s): %v", taskID, err)
			}
		}
		// A close by any other path leaves the columns as they are; the list
		// is what keeps them from being read again.
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
		if !slices.Equal(ids, []string{held}) {
			t.Errorf("listed tasks = %v, want only %s (closed %s and undeferred %s left out)", ids, held, closed, plain)
		}
		if len(all) == 1 && all[0].OrgID != orgID {
			t.Errorf("listed org = %q, want %q", all[0].OrgID, orgID)
		}
	})

	t.Run("Clear_compares_on_the_event_it_read", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		read := seed.Event(t, taskID)
		newer := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
		if _, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, read, runID); err != nil {
			t.Fatalf("DeferCloseSystem: %v", err)
		}
		// The deferral moves on to a newer event between the sweep's read and
		// its clear.
		if _, err := stores.Tasks.DeferCloseSystem(ctx, orgID, taskID, newer, runID); err != nil {
			t.Fatalf("DeferCloseSystem (overwrite): %v", err)
		}

		cleared, err := stores.Tasks.ClearCloseDeferralSystem(ctx, orgID, taskID, read)
		if err != nil {
			t.Fatalf("ClearCloseDeferralSystem (stale): %v", err)
		}
		if cleared {
			t.Error("clear on the overwritten event reported cleared")
		}
		if d := listed(t, stores.Tasks, taskID); d == nil || d.EventID != newer {
			t.Fatalf("the overwritten deferral did not survive: %+v", d)
		}

		cleared, err = stores.Tasks.ClearCloseDeferralSystem(ctx, orgID, taskID, newer)
		if err != nil {
			t.Fatalf("ClearCloseDeferralSystem: %v", err)
		}
		if !cleared {
			t.Error("clear on the held event reported not cleared")
		}
		if d := listed(t, stores.Tasks, taskID); d != nil {
			t.Errorf("deferral still listed after its clear: %+v", *d)
		}
	})

	t.Run("Deferral_columns_are_set_or_cleared_together", func(t *testing.T) {
		_, _, seed := mk(t)
		taskID := seed.Task(t)
		eventID := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")

		if err := seed.SetDeferralColumns(t, taskID, eventID, ""); err == nil {
			t.Error("an event with no blueprint run was accepted")
		}
		if err := seed.SetDeferralColumns(t, taskID, "", runID); err == nil {
			t.Error("a blueprint run with no event was accepted")
		}
		if err := seed.SetDeferralColumns(t, taskID, eventID, runID); err != nil {
			t.Errorf("both set was refused: %v", err)
		}
		if err := seed.SetDeferralColumns(t, taskID, "", ""); err != nil {
			t.Errorf("both cleared was refused: %v", err)
		}
	})

	t.Run("CloseTerminal_defers_the_named_task_and_closes_the_rest", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		deferred := seed.Task(t)
		entityID := seed.EntityForTask(t, deferred)
		other := seed.TaskOnEntity(t, entityID, review)
		eventID := seed.Event(t, deferred)
		runID, _ := seed.BlueprintAndConversation(t, deferred, "running", "")
		_, seq := seed.EntityRow(t, entityID)

		res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &seq, []string{ci, review},
			"entity_closed", domain.EventGitHubPRMerged, eventID, map[string]string{deferred: runID})
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if !res.Closed {
			t.Fatal("entity did not close")
		}
		if !slices.Equal(res.ClosedTaskIDs, []string{other}) {
			t.Errorf("ClosedTaskIDs = %v, want [%s]", res.ClosedTaskIDs, other)
		}
		if !slices.Equal(res.DeferredTaskIDs, []string{deferred}) {
			t.Errorf("DeferredTaskIDs = %v, want [%s]", res.DeferredTaskIDs, deferred)
		}
		if state, _ := seed.EntityRow(t, entityID); state != "closed" {
			t.Errorf("entity state = %q, want closed", state)
		}
		if s := seed.TaskStatus(t, deferred); s != "queued" {
			t.Errorf("deferred task status = %q, want queued", s)
		}
		if s := seed.TaskStatus(t, other); s != "done" {
			t.Errorf("other task status = %q, want done", s)
		}
		if n := seed.CloseAuditCount(t, deferred); n != 0 {
			t.Errorf("deferred task close audit rows = %d, want 0", n)
		}
		if seed.CancelRequested(t, runID) {
			t.Error("the deferred task's run was called off")
		}
		want := domain.TaskCloseDeferral{OrgID: orgID, TaskID: deferred, EventID: eventID, BlueprintRunID: runID}
		if d := listed(t, stores.Tasks, deferred); d == nil || *d != want {
			t.Errorf("listed deferral = %+v, want %+v", d, want)
		}
	})

	t.Run("CloseTerminal_guard_miss_stamps_nothing", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		eventID := seed.Event(t, taskID)
		runID, _ := seed.BlueprintAndConversation(t, taskID, "running", "")
		_, judgedAt := seed.EntityRow(t, entityID)
		seed.BumpPollSeq(t, entityID)

		res, err := stores.Entities.CloseTerminalSystem(ctx, orgID, entityID, &judgedAt, []string{ci},
			"entity_closed", domain.EventGitHubPRMerged, eventID, map[string]string{taskID: runID})
		if err != nil {
			t.Fatalf("CloseTerminalSystem: %v", err)
		}
		if res.Closed || len(res.DeferredTaskIDs) != 0 {
			t.Errorf("guard miss reported %+v, want nothing", res)
		}
		if d := listed(t, stores.Tasks, taskID); d != nil {
			t.Errorf("guard miss stamped a deferral: %+v", *d)
		}
	})

	t.Run("RunningRunForTask_reads_the_running_run_only", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		none := seed.Task(t)
		finished := seed.Task(t)
		running := seed.Task(t)
		seed.BlueprintAndConversation(t, finished, "completed", "")
		runID, _ := seed.BlueprintAndConversation(t, running, "running", "")

		for _, taskID := range []string{none, finished} {
			br, err := stores.Blueprints.RunningRunForTaskSystem(ctx, orgID, taskID)
			if err != nil {
				t.Fatalf("RunningRunForTaskSystem(%s): %v", taskID, err)
			}
			if br != nil {
				t.Errorf("task %s with no running run returned run %s", taskID, br.ID)
			}
		}
		br, err := stores.Blueprints.RunningRunForTaskSystem(ctx, orgID, running)
		if err != nil {
			t.Fatalf("RunningRunForTaskSystem: %v", err)
		}
		if br == nil || br.ID != runID {
			t.Fatalf("running task returned %+v, want run %s", br, runID)
		}
		point, err := stores.Blueprints.GetRunSystem(ctx, orgID, runID)
		if err != nil || point == nil {
			t.Fatalf("GetRunSystem: run=%v err=%v", point, err)
		}
		if !reflect.DeepEqual(*br, *point) {
			t.Errorf("RunningRunForTaskSystem read %+v, the point read %+v", *br, *point)
		}
	})

	t.Run("ListAgentActionsForBlueprintRun_reads_the_runs_agent_rows_of_one_action", func(t *testing.T) {
		stores, orgID, seed := mk(t)
		taskID := seed.Task(t)
		entityID := seed.EntityForTask(t, taskID)
		runID, convID := seed.BlueprintAndConversation(t, taskID, "running", "")
		otherTask := seed.Task(t)
		_, otherConv := seed.BlueprintAndConversation(t, otherTask, "running", "")

		record := func(conversationID, action, performedBy, detail string) string {
			t.Helper()
			key := action + "|" + performedBy + "|" + conversationID + "|" + detail
			if err := stores.ExternalActions.RecordSystem(ctx, orgID, domain.ExternalAction{
				Provider: domain.ProviderGitHub, Scope: "github.com", Action: action, Target: "owner/repo#1",
				EntityID: entityID, ConversationID: conversationID, PerformedBy: performedBy,
				Credential: domain.CredentialGitHubApp, DedupKey: key, DetailJSON: detail,
			}); err != nil {
				t.Fatalf("RecordSystem(%s): %v", action, err)
			}
			return key
		}
		first := record(convID, domain.ActionBranchPushed, domain.PerformedByAgent, `{"sha":"aaa"}`)
		second := record(convID, domain.ActionBranchPushed, domain.PerformedByAgent, `{"sha":"bbb"}`)
		record(convID, domain.ActionBranchPushed, domain.PerformedByPerson, `{"sha":"ccc"}`)
		record(convID, domain.ActionPRMerged, domain.PerformedByAgent, "")
		record(otherConv, domain.ActionBranchPushed, domain.PerformedByAgent, `{"sha":"ddd"}`)

		rows, err := stores.ExternalActions.ListAgentActionsForBlueprintRunSystem(ctx, orgID, runID, domain.ActionBranchPushed)
		if err != nil {
			t.Fatalf("ListAgentActionsForBlueprintRunSystem: %v", err)
		}
		var keys []string
		for _, a := range rows {
			keys = append(keys, a.DedupKey)
		}
		if got := slices.Sorted(slices.Values(keys)); !slices.Equal(got, slices.Sorted(slices.Values([]string{first, second}))) {
			t.Errorf("rows = %v, want the run's two agent pushes %v", keys, []string{first, second})
		}
		for i := 1; i < len(rows); i++ {
			prev, cur := rows[i-1], rows[i]
			if cur.OccurredAt.After(prev.OccurredAt) || (cur.OccurredAt.Equal(prev.OccurredAt) && cur.ID > prev.ID) {
				t.Errorf("rows not newest first by (occurred_at, id): %v then %v", prev, cur)
			}
		}
	})
}
