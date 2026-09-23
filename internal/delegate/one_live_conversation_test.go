package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// countConversationsForTask is the other half of countBlueprintRuns: what the
// one-live-conversation rule is ultimately about is the conversation, and a
// blueprint_run that minted no step would satisfy the run count alone.
func countConversationsForTask(t *testing.T, database *sql.DB, taskID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM conversations WHERE task_id = ?`, taskID).Scan(&n); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	return n
}

// TestDelegate_ConcurrentManualDelegates_MintOneConversation closes the
// double-delegate race. The delegate route is check-then-act — it tears the
// task's prior engagement down and then mints — so two gestures arriving
// together both find the task free, and before the manual mint was fenced they
// each minted a run and the task held two live conversations with nothing able
// to say which it was about.
//
// Every loser must come back as ErrTaskBusy specifically: it is the error the
// route answers 409 on, and a raw store error there would be a 500 telling the
// person their delegation broke rather than that someone beat them to it.
func TestDelegate_ConcurrentManualDelegates_MintOneConversation(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "double-delegate")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	const racers = 4
	ids := make([]string, racers)
	errs := make([]error, racers)
	var ready, done sync.WaitGroup
	ready.Add(racers)
	done.Add(racers)
	start := make(chan struct{})
	for i := range racers {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			ids[i], errs[i] = s.Delegate(task, DelegateOpts{
				OrgID:               runmode.LocalDefaultOrgID,
				ExplicitBlueprintID: bpID,
				TriggerType:         "manual",
				CreatorUserID:       runmode.LocalDefaultUserID,
			})
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	winners := 0
	for i := range racers {
		switch {
		case errs[i] == nil:
			winners++
			if ids[i] == "" {
				t.Errorf("racer %d succeeded with no blueprint_run id", i)
			}
		case errors.Is(errs[i], ErrTaskBusy):
			if ids[i] != "" {
				t.Errorf("racer %d lost the race but returned blueprint_run %q", i, ids[i])
			}
		default:
			t.Errorf("racer %d: want nil or ErrTaskBusy, got %v", i, errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent delegates produced %d live engagements, want exactly 1", winners)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 1 {
		t.Errorf("blueprint_runs on the task = %d, want 1", n)
	}
	if n := countConversationsForTask(t, database, task.ID); n != 1 {
		t.Errorf("conversations on the task = %d, want 1", n)
	}
}

// TestDelegate_ReDelegateAfterTeardown_Succeeds is the flow the fence must not
// break, and the reason cancelling the blueprint during a teardown is
// load-bearing rather than a courtesy: a parked conversation's blueprint stays
// 'running' by design, so if a teardown left it there the task would be locked
// out of every future delegation by a run nothing is driving.
//
// It is the delegate route's sequence: nothing holds the old step, so the
// stop it requests is settled in the same request and the new run mints on
// the first attempt.
func TestDelegate_ReDelegateAfterTeardown_Succeeds(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "re-delegate")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	opts := DelegateOpts{
		OrgID:               runmode.LocalDefaultOrgID,
		ExplicitBlueprintID: bpID,
		TriggerType:         "manual",
		CreatorUserID:       runmode.LocalDefaultUserID,
	}

	first, err := s.Delegate(task, opts)
	if err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	if _, err := s.Delegate(task, opts); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("re-delegate onto a live task = %v, want ErrTaskBusy", err)
	}

	ctx := context.Background()
	if err := s.CheckTaskUnheld(ctx, runmode.LocalDefaultOrgID, task.ID); err != nil {
		t.Fatalf("CheckTaskUnheld on a queued, unclaimed step = %v, want nil", err)
	}
	stores := sqlitestore.New(database)
	convs, err := stores.Blueprints.ConversationsForBlueprintSystem(ctx, runmode.LocalDefaultOrgID, first)
	if err != nil || len(convs) == 0 {
		t.Fatalf("ConversationsForBlueprintSystem(%s) = %d conversations, err=%v", first, len(convs), err)
	}
	if err := s.StopConversationAndCancelBlueprint(runmode.LocalDefaultOrgID, convs[0].ID,
		runmode.LocalDefaultUserID, StopCauseTaskDelegated); err != nil {
		t.Fatalf("StopConversationAndCancelBlueprint: %v", err)
	}
	if err := s.SettleTaskStops(ctx, runmode.LocalDefaultOrgID, task.ID); err != nil {
		t.Fatalf("SettleTaskStops: %v", err)
	}
	if br, err := stores.Blueprints.GetRunSystem(ctx, runmode.LocalDefaultOrgID, first); err != nil || br == nil ||
		br.Status == domain.BlueprintRunStatusRunning {
		t.Fatalf("the teardown left the blueprint running (%+v, err=%v); the task would be locked out forever", br, err)
	}

	if _, err := s.Delegate(task, opts); err != nil {
		t.Fatalf("re-delegate after the teardown: %v", err)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 2 {
		t.Errorf("blueprint_runs on the task = %d, want 2 (one torn down, one live)", n)
	}
}

// The route's one window: the held check passes, then an executor claims the
// step before the stop is requested. Replayed in order rather than raced, so
// the interleaving is the one under test every time. The settlement leaves
// the now-held row to its holder, the mint is refused by the one-active-run
// index, and the holder still finds its stop pending: the loser gets a 409
// and nothing is left half-settled.
func TestDelegate_ReDelegateLosingTheClaimRaceIsRefusedAtTheMint(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "claim-race")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	opts := DelegateOpts{
		OrgID: runmode.LocalDefaultOrgID, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
	}

	first, err := s.Delegate(task, opts)
	if err != nil {
		t.Fatalf("first Delegate: %v", err)
	}
	stores := sqlitestore.New(database)
	convs, err := stores.Blueprints.ConversationsForBlueprintSystem(ctx, runmode.LocalDefaultOrgID, first)
	if err != nil || len(convs) == 0 {
		t.Fatalf("ConversationsForBlueprintSystem = %d conversations, err=%v", len(convs), err)
	}

	if err := s.CheckTaskUnheld(ctx, runmode.LocalDefaultOrgID, task.ID); err != nil {
		t.Fatalf("CheckTaskUnheld before the claim = %v, want nil", err)
	}
	markEngaged(t, database, convs[0].ID)
	if err := s.StopConversationAndCancelBlueprint(runmode.LocalDefaultOrgID, convs[0].ID,
		runmode.LocalDefaultUserID, StopCauseTaskDelegated); err != nil {
		t.Fatalf("StopConversationAndCancelBlueprint: %v", err)
	}
	if err := s.SettleTaskStops(ctx, runmode.LocalDefaultOrgID, task.ID); err != nil {
		t.Fatalf("SettleTaskStops: %v", err)
	}

	if _, err := s.Delegate(task, opts); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("re-delegate after losing the claim race = %v, want ErrTaskBusy", err)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 1 {
		t.Errorf("blueprint_runs on the task = %d, want the one the holder still drives", n)
	}
	got, err := s.conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, convs[0].ID)
	if err != nil || got == nil {
		t.Fatalf("GetSystem: (%v, %v)", got, err)
	}
	if got.StopRequestedAt == nil || got.Status == domain.StatusOpen {
		t.Errorf("held step = (status %q, intent %v), want unparked with the stop pending for its holder", got.Status, got.StopRequestedAt)
	}
}

// A conversation an executor holds is the one case a re-delegate cannot
// settle for itself, and the check says whether that holder has been asked
// to stop yet.
func TestCheckTaskUnheld_NamesAHeldTaskAndAStoppingOne(t *testing.T) {
	database := newCostCapTestDB(t)
	task, bpID := delegatableFixture(t, database, "held")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()

	first, err := s.Delegate(task, DelegateOpts{
		OrgID: runmode.LocalDefaultOrgID, ExplicitBlueprintID: bpID,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	convs, err := sqlitestore.New(database).Blueprints.ConversationsForBlueprintSystem(ctx, runmode.LocalDefaultOrgID, first)
	if err != nil || len(convs) == 0 {
		t.Fatalf("ConversationsForBlueprintSystem = %d conversations, err=%v", len(convs), err)
	}
	markEngaged(t, database, convs[0].ID)

	if err := s.CheckTaskUnheld(ctx, runmode.LocalDefaultOrgID, task.ID); !errors.Is(err, ErrTaskHeld) {
		t.Fatalf("CheckTaskUnheld on a held step = %v, want ErrTaskHeld", err)
	}
	if ok, err := s.conversations.RequestStopSystem(ctx, runmode.LocalDefaultOrgID, convs[0].ID, runmode.LocalDefaultUserID, ""); err != nil || !ok {
		t.Fatalf("RequestStopSystem = (%v, %v)", ok, err)
	}
	if err := s.CheckTaskUnheld(ctx, runmode.LocalDefaultOrgID, task.ID); !errors.Is(err, ErrTaskStopping) {
		t.Fatalf("CheckTaskUnheld on a held step with a stop pending = %v, want ErrTaskStopping", err)
	}
	// The settlement leaves a held row to its holder, so a re-delegate in
	// this state would still be refused at the mint.
	if err := s.SettleTaskStops(ctx, runmode.LocalDefaultOrgID, task.ID); err != nil {
		t.Fatalf("SettleTaskStops: %v", err)
	}
	if got, _ := s.conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, convs[0].ID); got == nil || got.StopRequestedAt == nil {
		t.Error("the task settlement took a row its holder is still driving")
	}
}
