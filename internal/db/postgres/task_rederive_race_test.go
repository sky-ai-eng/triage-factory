package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The two orders of the leased-row race between a score write and a
// completion, on real concurrent connections. The queue row is the
// serialization point: every score writer raises requested_revision on the
// row in the transaction that writes the score, so the write takes the row
// lock the completor holds. It therefore either committed before the lock —
// the completor sees the raised value and defers — or waits behind it — the
// completor completes, and the writer's admission finds a done row and
// inserts a fresh ready one for the new revision.

type rederiveRaceWorld struct {
	h      *pgtest.Harness
	stores db.Stores
	orgID  string
	tup    dbtest.PendingFiringsTuple
	agent  string
}

func newReDeriveRaceWorld(t *testing.T) rederiveRaceWorld {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	orgID, userID, agentID := seedPgPendingFiringsOrg(t, h)
	seed := newPgPendingFiringsSeeder(h, stores, orgID, userID, agentID)
	return rederiveRaceWorld{h: h, stores: stores, orgID: orgID, tup: seed.Tuple(t), agent: agentID}
}

func (w rederiveRaceWorld) score(t *testing.T, autonomy float64) {
	t.Helper()
	if err := w.stores.Scores.UpdateTaskScores(context.Background(), w.orgID, []domain.TaskScoreUpdate{{
		ID: w.tup.TaskID, PriorityScore: 0.5, AutonomySuitability: autonomy, Summary: "s", PriorityReasoning: "r",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}
}

func (w rederiveRaceWorld) claim(t *testing.T) db.ClaimedReDerive {
	t.Helper()
	batch, err := w.stores.TaskReDerive.Claim(context.Background(), workitem.Owner{ID: "race", Epoch: 1}, 1)
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("Claim: %+v %v", batch, err)
	}
	return batch.Items[0]
}

func (w rederiveRaceWorld) enqueue(claim db.AgentClaimStamp) func(db.PendingFiringsStore) error {
	return func(s db.PendingFiringsStore) error {
		_, _, err := s.Enqueue(context.Background(), w.orgID, w.tup.EntityID, w.tup.TaskID, w.tup.TriggerID, w.tup.EventID, claim)
		return err
	}
}

// TestTaskReDerive_Postgres_ScoreCommitsBeforeTheCompletionLock is order
// (a): the newer score lands between the claim and the completion's lock, so
// the completion sees the raised revision and returns ErrScoreMoved with
// nothing written.
func TestTaskReDerive_Postgres_ScoreCommitsBeforeTheCompletionLock(t *testing.T) {
	w := newReDeriveRaceWorld(t)
	ctx := context.Background()
	w.score(t, 0.9)
	item := w.claim(t)
	w.score(t, 0.2)

	err := w.stores.TaskReDerive.Complete(ctx, item.Receipt, w.enqueue(db.AgentClaimStamp{}))
	if !errors.Is(err, db.ErrScoreMoved) {
		t.Fatalf("Complete = %v, want ErrScoreMoved", err)
	}
	if rows, _ := w.stores.PendingFirings.ListForEntity(ctx, w.orgID, w.tup.EntityID); len(rows) != 0 {
		t.Errorf("refused completion admitted firings: %+v", rows)
	}
	got := readPgReDeriveRows(t, w.h.AdminDB, w.tup.TaskID)
	if len(got) != 1 || got[0].Status != workitem.StatusLeased || got[0].RequestedRevision != 2 {
		t.Errorf("queue rows = %+v, want the one row still leased at revision 2", got)
	}
}

// TestTaskReDerive_Postgres_ScoreWaitsBehindTheCompletionLock is order (b):
// the score write arrives while the completion holds its row lock, waits,
// and on release finds a done row — so it admits a fresh ready one at the
// new revision rather than raising the one the completion just settled.
func TestTaskReDerive_Postgres_ScoreWaitsBehindTheCompletionLock(t *testing.T) {
	w := newReDeriveRaceWorld(t)
	ctx := context.Background()
	w.score(t, 0.9)
	item := w.claim(t)

	inClosure := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		completed <- w.stores.TaskReDerive.Complete(ctx, item.Receipt, func(s db.PendingFiringsStore) error {
			if err := w.enqueue(db.AgentClaimStamp{})(s); err != nil {
				return err
			}
			close(inClosure)
			<-release
			return nil
		})
	}()
	<-inClosure

	scored := make(chan error, 1)
	go func() {
		scored <- w.stores.Scores.UpdateTaskScores(ctx, w.orgID, []domain.TaskScoreUpdate{{
			ID: w.tup.TaskID, PriorityScore: 0.5, AutonomySuitability: 0.2, Summary: "s", PriorityReasoning: "r",
		}})
	}()
	// The score write is blocked on the row the completion holds: it must
	// not return while the closure is open.
	select {
	case err := <-scored:
		t.Fatalf("UpdateTaskScores returned %v while the completion held the row lock; the admission did not wait", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-completed; err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case err := <-scored:
		if err != nil {
			t.Fatalf("UpdateTaskScores after the completion released the row: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UpdateTaskScores did not return after the completion committed")
	}

	rows := readPgReDeriveRows(t, w.h.AdminDB, w.tup.TaskID)
	if len(rows) != 2 {
		t.Fatalf("queue rows = %+v, want the done row and a fresh ready one", rows)
	}
	if rows[0].Status != workitem.StatusDone || rows[0].RequestedRevision != 1 {
		t.Errorf("first row = %+v, want done at revision 1", rows[0])
	}
	if rows[1].Status != workitem.StatusReady || rows[1].RequestedRevision != 2 || rows[1].Attempt != 0 {
		t.Errorf("second row = %+v, want ready at revision 2", rows[1])
	}
	if rev := readPgScoreRevision(t, w.h.AdminDB, w.tup.TaskID); rev != 2 {
		t.Errorf("score_revision = %d, want 2", rev)
	}
	if firings, _ := w.stores.PendingFirings.ListForEntity(ctx, w.orgID, w.tup.EntityID); len(firings) != 1 {
		t.Errorf("firings = %+v, want the one the completion admitted", firings)
	}
}

// TestTaskReDerive_Postgres_LockOrderAgainstAScoreWrite pins the lock order
// the two transactions share: a completion whose closure stamps the task —
// so it holds the queue row and then the tasks row — runs concurrently with a
// score write for the same task, which takes the queue row before it touches
// tasks. Both take the queue row first, so the score write waits behind the
// completion instead of the two waiting on each other; a score write that
// locked tasks first would deadlock here.
func TestTaskReDerive_Postgres_LockOrderAgainstAScoreWrite(t *testing.T) {
	w := newReDeriveRaceWorld(t)
	ctx := context.Background()
	w.score(t, 0.9)
	item := w.claim(t)

	stamped := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		completed <- w.stores.TaskReDerive.Complete(ctx, item.Receipt, func(s db.PendingFiringsStore) error {
			// The claim stamp is the tasks write; from here the transaction
			// holds both rows until it commits.
			if err := w.enqueue(db.AgentClaimStamp{AgentID: w.agent})(s); err != nil {
				return err
			}
			close(stamped)
			<-release
			return nil
		})
	}()
	<-stamped

	scored := make(chan error, 1)
	go func() {
		scored <- w.stores.Scores.UpdateTaskScores(ctx, w.orgID, []domain.TaskScoreUpdate{{
			ID: w.tup.TaskID, PriorityScore: 0.5, AutonomySuitability: 0.2, Summary: "s", PriorityReasoning: "r",
		}})
	}()
	// Let the score write reach the queue row and block there; a deadlock
	// would surface as an error on one side rather than a wait.
	select {
	case err := <-scored:
		t.Fatalf("UpdateTaskScores returned %v while the completion held both rows", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	deadline := time.After(10 * time.Second)
	for done := 0; done < 2; {
		select {
		case err := <-completed:
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			done++
		case err := <-scored:
			if err != nil {
				t.Fatalf("UpdateTaskScores: %v", err)
			}
			done++
		case <-deadline:
			t.Fatal("the completion and the score write did not both finish; the lock order deadlocked")
		}
	}

	var claimedBy string
	if err := w.h.AdminDB.QueryRow(`SELECT COALESCE(claimed_by_agent_id::text, '') FROM tasks WHERE id = $1`, w.tup.TaskID).Scan(&claimedBy); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	if claimedBy != w.agent {
		t.Errorf("task claimed_by_agent_id = %q, want the completion's stamp %q", claimedBy, w.agent)
	}
	if rev := readPgScoreRevision(t, w.h.AdminDB, w.tup.TaskID); rev != 2 {
		t.Errorf("score_revision = %d, want 2", rev)
	}
	rows := readPgReDeriveRows(t, w.h.AdminDB, w.tup.TaskID)
	if len(rows) != 2 || rows[0].Status != workitem.StatusDone || rows[1].Status != workitem.StatusReady || rows[1].RequestedRevision != 2 {
		t.Errorf("queue rows = %+v, want done at 1 then ready at 2", rows)
	}
}
