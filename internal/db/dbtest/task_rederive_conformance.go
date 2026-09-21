package dbtest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskReDeriveStoreFactory is what a per-backend test file hands to
// RunTaskReDeriveStoreConformance. Called once per subtest; returns a fresh
// fixture.
type TaskReDeriveStoreFactory func(t *testing.T) TaskReDeriveFixture

// TaskReDeriveFixture is one subtest's world for the re-evaluation queue:
// the store under test, the score store that admits into it, the firings
// store the completion's effects land in, and the schema-aware probes the
// harness cannot write itself.
type TaskReDeriveFixture struct {
	Store   db.TaskReDeriveStore
	Scores  db.ScoreStore
	Firings db.PendingFiringsStore
	OrgID   string
	// Tuple seeds a fresh entity/task/trigger/event chain a firing can be
	// admitted against.
	Tuple func(t *testing.T) PendingFiringsTuple
	// QueueRows reads the task's task_rederive_queue rows, oldest first.
	QueueRows func(t *testing.T, taskID string) []ReDeriveQueueRow
	// ExpireLease rewinds a leased row's lease_expires_at into the past,
	// standing in for a holder that died without a terminal write.
	ExpireLease func(t *testing.T, id int64)
}

// rederiveOwner is the owner every claim in this suite stamps a row with.
var rederiveOwner = workitem.Owner{ID: "conformance-rederive-worker", Epoch: 1}

// RunTaskReDeriveStoreConformance covers the re-evaluation queue contract
// every backend impl must hold: the claim freezes the revision and names the
// task, the completion runs its effects inside its own transaction and only
// against the revision it froze, the deferral refunds only when the revision
// really moved, a stale receipt changes nothing, and the handle describes
// its rows.
func RunTaskReDeriveStoreConformance(t *testing.T, mk TaskReDeriveStoreFactory) {
	t.Helper()
	ctx := context.Background()

	score := func(t *testing.T, f TaskReDeriveFixture, taskID string, autonomy float64) {
		t.Helper()
		if err := f.Scores.UpdateTaskScores(ctx, f.OrgID, []domain.TaskScoreUpdate{{
			ID: taskID, PriorityScore: 0.5, AutonomySuitability: autonomy, Summary: "s", PriorityReasoning: "r",
		}}); err != nil {
			t.Fatalf("UpdateTaskScores: %v", err)
		}
	}
	claimOne := func(t *testing.T, f TaskReDeriveFixture) db.ClaimedReDerive {
		t.Helper()
		batch, err := f.Store.Claim(ctx, rederiveOwner, 1)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(batch.Items) != 1 {
			t.Fatalf("Claim returned %d items, want 1 (cancelled=%d parked=%d)", len(batch.Items), batch.Cancelled, batch.Parked)
		}
		return batch.Items[0]
	}
	row := func(t *testing.T, f TaskReDeriveFixture, taskID string) ReDeriveQueueRow {
		t.Helper()
		rows := f.QueueRows(t, taskID)
		if len(rows) != 1 {
			t.Fatalf("task %s has %d queue rows, want 1: %+v", taskID, len(rows), rows)
		}
		return rows[0]
	}
	firings := func(t *testing.T, f TaskReDeriveFixture, entityID string) []domain.PendingFiring {
		t.Helper()
		rows, err := f.Firings.ListForEntity(ctx, f.OrgID, entityID)
		if err != nil {
			t.Fatalf("ListForEntity: %v", err)
		}
		return rows
	}
	enqueue := func(f TaskReDeriveFixture, tup PendingFiringsTuple) func(db.PendingFiringsStore) error {
		return func(s db.PendingFiringsStore) error {
			_, _, err := s.Enqueue(ctx, f.OrgID, tup.EntityID, tup.TaskID, tup.TriggerID, tup.EventID, db.AgentClaimStamp{})
			return err
		}
	}

	t.Run("Claim_freezes_the_requested_revision_and_names_the_task", func(t *testing.T) {
		f := mk(t)
		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		score(t, f, tup.TaskID, 0.8)

		item := claimOne(t, f)
		if item.TaskID != tup.TaskID {
			t.Errorf("claimed task = %q, want %q", item.TaskID, tup.TaskID)
		}
		if item.RequestedRevision != 2 {
			t.Errorf("claimed RequestedRevision = %d, want 2 after two saves", item.RequestedRevision)
		}
		frozen, ok := item.Receipt.Frozen["requested_revision"].(int64)
		if !ok || frozen != 2 {
			t.Errorf("receipt froze requested_revision as %T %v, want int64 2", item.Receipt.Frozen["requested_revision"], item.Receipt.Frozen["requested_revision"])
		}
		if got := row(t, f, tup.TaskID); got.Status != workitem.StatusLeased || got.Attempt != 1 {
			t.Errorf("row after claim = %+v, want leased at attempt 1", got)
		}
	})

	t.Run("Complete_runs_the_effects_in_the_completion_transaction", func(t *testing.T) {
		f := mk(t)
		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		item := claimOne(t, f)
		if err := f.Store.Complete(ctx, item.Receipt, enqueue(f, tup)); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if got := row(t, f, tup.TaskID); got.Status != workitem.StatusDone {
			t.Errorf("row after Complete = %+v, want done", got)
		}
		landed := firings(t, f, tup.EntityID)
		if len(landed) != 1 || landed[0].Status != workitem.StatusReady || landed[0].UniqueKey != workkinds.PendingFiringKey(tup.TaskID, tup.TriggerID) {
			t.Fatalf("firings after Complete = %+v, want one ready row keyed on the (task, trigger)", landed)
		}

		// A closure that fails rolls its admission back with it: no firing
		// row, and the queue row still leased for this holder to dispose of.
		tup2 := f.Tuple(t)
		score(t, f, tup2.TaskID, 0.9)
		item2 := claimOne(t, f)
		boom := errors.New("effects refused")
		err := f.Store.Complete(ctx, item2.Receipt, func(s db.PendingFiringsStore) error {
			if err := enqueue(f, tup2)(s); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Complete with a failing closure = %v, want the closure's error", err)
		}
		if landed := firings(t, f, tup2.EntityID); len(landed) != 0 {
			t.Errorf("a failed completion left firing rows behind: %+v", landed)
		}
		if got := row(t, f, tup2.TaskID); got.Status != workitem.StatusLeased {
			t.Errorf("row after a failed Complete = %+v, want still leased", got)
		}
	})

	t.Run("Complete_after_a_newer_score_returns_ErrScoreMoved_and_writes_nothing", func(t *testing.T) {
		f := mk(t)
		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		item := claimOne(t, f)
		score(t, f, tup.TaskID, 0.2)

		err := f.Store.Complete(ctx, item.Receipt, enqueue(f, tup))
		if !errors.Is(err, db.ErrScoreMoved) {
			t.Fatalf("Complete against a raised row = %v, want ErrScoreMoved", err)
		}
		if landed := firings(t, f, tup.EntityID); len(landed) != 0 {
			t.Errorf("a refused completion left firing rows behind: %+v", landed)
		}
		got := row(t, f, tup.TaskID)
		if got.Status != workitem.StatusLeased || got.RequestedRevision != 2 || got.Attempt != 1 {
			t.Errorf("row after ErrScoreMoved = %+v, want still leased at revision 2, attempt 1", got)
		}
	})

	t.Run("DeferScoreMoved_refunds_only_when_the_revision_moved", func(t *testing.T) {
		f := mk(t)
		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		item := claimOne(t, f)
		score(t, f, tup.TaskID, 0.2)

		if err := f.Store.DeferScoreMoved(ctx, item.Receipt); err != nil {
			t.Fatalf("DeferScoreMoved after a newer score: %v", err)
		}
		got := row(t, f, tup.TaskID)
		if got.Status != workitem.StatusReady || got.Attempt != 0 || got.RequestedRevision != 2 {
			t.Fatalf("row after deferral = %+v, want ready at attempt 0, revision 2", got)
		}
		// The stale receipt cannot refund twice.
		if err := f.Store.DeferScoreMoved(ctx, item.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("second DeferScoreMoved with the spent receipt = %v, want ErrLeaseLost", err)
		}

		// The next claim freezes the newer revision, and a deferral with
		// nothing moved is refused, the row still leased and the attempt
		// still charged.
		again := claimOne(t, f)
		if again.RequestedRevision != 2 || again.Receipt.LeaseGeneration != item.Receipt.LeaseGeneration+1 {
			t.Fatalf("re-claim = %+v, want revision 2 at the next generation", again)
		}
		if err := f.Store.DeferScoreMoved(ctx, again.Receipt); !errors.Is(err, workitem.ErrDeferRefused) {
			t.Errorf("DeferScoreMoved with the revision unmoved = %v, want ErrDeferRefused", err)
		}
		if got := row(t, f, tup.TaskID); got.Status != workitem.StatusLeased || got.Attempt != 1 {
			t.Errorf("row after a refused deferral = %+v, want still leased at attempt 1", got)
		}
	})

	t.Run("Stale_receipts_change_nothing", func(t *testing.T) {
		f := mk(t)
		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		item := claimOne(t, f)
		f.ExpireLease(t, item.Receipt.ItemID)
		before := f.QueueRows(t, tup.TaskID)

		if _, err := f.Store.RenewLease(ctx, item.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("RenewLease on an expired lease = %v, want ErrLeaseLost", err)
		}
		if err := f.Store.Complete(ctx, item.Receipt, enqueue(f, tup)); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("Complete on an expired lease = %v, want ErrLeaseLost", err)
		}
		if err := f.Store.DeferScoreMoved(ctx, item.Receipt); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("DeferScoreMoved on an expired lease = %v, want ErrLeaseLost", err)
		}
		if _, err := f.Store.Requeue(ctx, item.Receipt, workitem.OutcomeTransient, errors.New("late")); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("Requeue on an expired lease = %v, want ErrLeaseLost", err)
		}
		if landed := firings(t, f, tup.EntityID); len(landed) != 0 {
			t.Errorf("a stale completion admitted firings: %+v", landed)
		}
		if after := f.QueueRows(t, tup.TaskID); !reflect.DeepEqual(before, after) {
			t.Errorf("stale receipts changed the row:\n before %+v\n after  %+v", before, after)
		}

		// The next claim reclaims it and the old receipt is still dead.
		successor := claimOne(t, f)
		if !successor.Receipt.Reclaimed || successor.Receipt.LeaseGeneration != item.Receipt.LeaseGeneration+1 {
			t.Fatalf("successor = %+v, want a reclaim at the next generation", successor.Receipt)
		}
		if err := f.Store.Complete(ctx, item.Receipt, enqueue(f, tup)); !errors.Is(err, workitem.ErrLeaseLost) {
			t.Errorf("Complete with the old receipt after a takeover = %v, want ErrLeaseLost", err)
		}
		if err := f.Store.Complete(ctx, successor.Receipt, enqueue(f, tup)); err != nil {
			t.Errorf("successor's Complete: %v", err)
		}
	})

	t.Run("Handle_describes_rows_by_their_task", func(t *testing.T) {
		f := mk(t)
		h, ok := f.Store.(db.WorkKindHandle)
		if !ok {
			t.Fatalf("%T does not implement db.WorkKindHandle", f.Store)
		}
		if h.Name() != workkinds.TaskReDeriveName || h.Label() != workkinds.TaskReDeriveLabel {
			t.Errorf("handle identity = %q / %q", h.Name(), h.Label())
		}
		if h.Access() != db.WorkAccessOrgAdmin {
			t.Errorf("access = %v, want org admin", h.Access())
		}
		if c := h.Controls(); !c.Redrive || !c.Cancel || c.Supersede {
			t.Errorf("controls = %+v, want redrive and cancel only", c)
		}
		if h.Objective().OldestReadyAge != workkinds.TaskReDeriveOldestReadyObjective {
			t.Errorf("objective = %v", h.Objective())
		}
		if h.Kind().Table != workkinds.TaskReDeriveName {
			t.Errorf("kind table = %q", h.Kind().Table)
		}

		tup := f.Tuple(t)
		score(t, f, tup.TaskID, 0.9)
		got := row(t, f, tup.TaskID)
		subjects, err := h.Describe(ctx, f.OrgID, []int64{got.ID, got.ID + 1000})
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if len(subjects) != 1 {
			t.Fatalf("Describe returned %d subjects, want the one real row: %+v", len(subjects), subjects)
		}
		subj := subjects[got.ID]
		if subj.Label == "" || subj.Label == tup.TaskID || subj.Detail != "Test PR" {
			t.Errorf("subject = %+v, want the entity's source id and title", subj)
		}
		want := map[string]string{"task_id": tup.TaskID, "requested_revision": "1", "event_type": domain.EventGitHubPRCICheckFailed}
		if !reflect.DeepEqual(subj.Fields, want) {
			t.Errorf("subject fields = %v, want %v", subj.Fields, want)
		}
		if empty, err := h.Describe(ctx, f.OrgID, nil); err != nil || len(empty) != 0 {
			t.Errorf("Describe(nil) = %v, %v", empty, err)
		}
	})
}
