package routing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The worker's recovery and race behavior, on the SQLite handle the local
// brain runs on. Every scenario scores through the store, so the queue row
// under test is the one production's score write admits.

// hookedReDerive wraps the queue store with a hook that runs before the
// completion opens its transaction: the seam between the evaluation's reads
// and the completion's lock, where a newer score or a lost lease lands in
// the race these tests stage.
type hookedReDerive struct {
	db.TaskReDeriveStore
	beforeComplete func()
}

func (h hookedReDerive) Complete(ctx context.Context, r workitem.Receipt, effects func(db.PendingFiringsStore) error) error {
	if h.beforeComplete != nil {
		h.beforeComplete()
	}
	return h.TaskReDeriveStore.Complete(ctx, r, effects)
}

// hookedReDeriveRouter is reDeriveRouter with the queue store wrapped.
func hookedReDeriveRouter(t *testing.T, database *sql.DB, handlers db.EventHandlerStore, beforeComplete func()) *Router {
	t.Helper()
	seedLocalBot(t, database)
	st := sqlitestore.New(database)
	if handlers == nil {
		handlers = testEventHandlerStore(database)
	}
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), handlers, st.Agents, st.TeamAgents, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	r.SetTaskReDerive(hookedReDerive{TaskReDeriveStore: st.TaskReDerive, beforeComplete: beforeComplete})
	r.SetExecutorID("rederive-worker-test", 1)
	return r
}

// expireReDeriveLease rewinds a leased row's lease into the past, standing
// in for a holder that died without a terminal write.
func expireReDeriveLease(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	res, err := database.Exec(`UPDATE task_rederive_queue SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE task_id = ? AND status = 'leased'`, taskID)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expire lease touched %d rows, want the one leased row", n)
	}
}

// ripenReDerive clears a ready row's retry time, so a requeued row is
// claimable again without waiting out the backoff.
func ripenReDerive(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE task_rederive_queue SET next_attempt_at = NULL WHERE task_id = ? AND status = 'ready'`, taskID); err != nil {
		t.Fatalf("ripen: %v", err)
	}
}

// TestReDeriveWorker_ReclaimsADeadHoldersRowAndEvaluatesOnce: a holder
// claimed the row and died before completing. Nothing it did landed —
// its admissions were inside its uncommitted completion — so the reclaim
// evaluates the task once and admits its firing once, and a later pass has
// nothing left to do.
func TestReDeriveWorker_ReclaimsADeadHoldersRowAndEvaluatesOnce(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)
	st := sqlitestore.New(database)

	// The dead holder: claimed, never came back.
	batch, err := st.TaskReDerive.Claim(t.Context(), workitem.Owner{ID: "dead-pod", Epoch: 1}, 1)
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("Claim: %+v %v", batch, err)
	}
	expireReDeriveLease(t, database, taskID)

	r := reDeriveRouter(t, database, nil)
	drainReDeriveOnce(t, r)

	firings := firingsForTask(t, database, taskID)
	if len(firings) != 1 {
		t.Fatalf("reclaimed evaluation admitted %d firing(s), want exactly 1: %+v", len(firings), firings)
	}
	row := reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusDone || row.attempt != 2 || row.generation != 2 {
		t.Errorf("row after the reclaim = %+v, want done at attempt 2, generation 2", row)
	}

	drainReDeriveOnce(t, r)
	if firings := firingsForTask(t, database, taskID); len(firings) != 1 {
		t.Errorf("a later pass changed the firings to %d; the obligation was already settled", len(firings))
	}
}

// TestReDeriveWorker_ScoreLandingAfterTheClaimDefersAndReEvaluates: the
// evaluation read a score above the threshold and planned a firing; before
// its completion locked the row a newer score landed below the threshold.
// The completion sees the raised revision and commits nothing; the deferral
// refunds the attempt; the next claim freezes the newer revision and decides
// on the newer score, which fires nothing. A score that crossed the threshold
// downward is never acted on through the older read.
func TestReDeriveWorker_ScoreLandingAfterTheClaimDefersAndReEvaluates(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)

	landed := false
	r := hookedReDeriveRouter(t, database, nil, func() {
		if landed {
			return
		}
		landed = true
		scoreTask(t, database, taskID, 0.4)
	})

	drainReDeriveOnce(t, r)
	if !landed {
		t.Fatal("the newer score never landed; the hook did not run before the completion")
	}
	requireNoFirings(t, database, taskID)
	row := reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusReady || row.attempt != 0 || row.revision != 2 || row.lastOutcome != "deferred" {
		t.Fatalf("row after the moved score = %+v, want ready at attempt 0 (refunded), revision 2, deferred", row)
	}

	drainReDeriveOnce(t, r)
	requireNoFirings(t, database, taskID)
	row = reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusDone || row.attempt != 1 {
		t.Errorf("row after the re-evaluation = %+v, want done after one charged attempt", row)
	}
	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.ClaimedByAgentID != "" {
		t.Errorf("task claimed by %q; the evaluation against the older score must not have landed", task.ClaimedByAgentID)
	}
}

// TestReDeriveWorker_ReadFailureRequeuesWithBackoffAndParksOnTheFifth: a
// store read that keeps failing mid-evaluation is not a decision. Each
// attempt returns the row to the queue with a retry time; the fifth parks
// it for a person, with the failure that spent the budget on the row.
func TestReDeriveWorker_ReadFailureRequeuesWithBackoffAndParksOnTheFifth(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)

	handlers := failingHandlerStore{
		EventHandlerStore: testEventHandlerStore(database),
		err:               errors.New("simulated store failure"),
	}
	r := hookedReDeriveRouter(t, database, handlers, nil)

	drainReDeriveOnce(t, r)
	row := reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusReady || row.attempt != 1 || row.lastOutcome != string(workitem.OutcomeTransient) || !row.nextAttempt.Valid {
		t.Fatalf("row after the first failure = %+v, want ready at attempt 1 with a backoff", row)
	}
	// Not yet ripe: a pass now claims nothing.
	drainReDeriveOnce(t, r)
	if row := reDeriveRowFor(t, database, taskID); row.attempt != 1 {
		t.Fatalf("a pass before the backoff ripened charged attempt %d", row.attempt)
	}

	// Each failed attempt's retry sits inside the kind's backoff band for
	// that attempt — 5s, 10s, 20s, 40s before jitter — so the delays grow
	// rather than repeat.
	requireBackoff(t, row, 1)
	for attempt := 2; attempt <= 5; attempt++ {
		ripenReDerive(t, database, taskID)
		drainReDeriveOnce(t, r)
		row := reDeriveRowFor(t, database, taskID)
		if row.attempt != attempt {
			t.Fatalf("after pass %d the row charged attempt %d", attempt, row.attempt)
		}
		if attempt < 5 {
			if row.status != workitem.StatusReady {
				t.Fatalf("after pass %d the row is %q, want ready with budget left", attempt, row.status)
			}
			requireBackoff(t, row, attempt)
		}
	}
	row = reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusParked || row.lastOutcome != string(workitem.OutcomeTransient) {
		t.Errorf("row after the fifth failure = %+v, want parked under the transient outcome that spent the budget", row)
	}
	requireNoFirings(t, database, taskID)

	// A working read after a redrive evaluates it and admits the firing.
	h := sqlitestore.New(database).TaskReDerive.(db.WorkKindHandle)
	if err := workitem.Redrive(t.Context(), h.Conn(), h.Kind(), runmode.LocalDefaultOrgID, row.id, "operator"); err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	drainReDeriveOnce(t, reDeriveRouter(t, database, nil))
	if firings := firingsForTask(t, database, taskID); len(firings) != 1 {
		t.Errorf("redriven evaluation admitted %d firing(s), want 1", len(firings))
	}
	requireReDeriveDone(t, database, taskID)
}

// TestReDeriveWorker_CompletionThatLosesItsLeaseAdmitsNothing: the lease
// lapses between the evaluation and the completion's lock. The completion
// matches nothing and rolls back, so the planned firing never lands, and the
// next claim reclaims the row and evaluates it again.
func TestReDeriveWorker_CompletionThatLosesItsLeaseAdmitsNothing(t *testing.T) {
	database := newTestDB(t)
	taskID, _ := setupReDeriveScenario(t, database, 0.6)
	scoreTask(t, database, taskID, 0.9)

	lost := false
	r := hookedReDeriveRouter(t, database, nil, func() {
		if lost {
			return
		}
		lost = true
		expireReDeriveLease(t, database, taskID)
	})

	drainReDeriveOnce(t, r)
	if !lost {
		t.Fatal("the lease was never expired; the hook did not run before the completion")
	}
	requireNoFirings(t, database, taskID)
	row := reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusLeased || row.attempt != 1 {
		t.Fatalf("row after the lost lease = %+v, want still leased (expired) at attempt 1", row)
	}
	task, _ := testTaskStore(database).Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if task.ClaimedByAgentID != "" {
		t.Errorf("task claimed by %q after a completion that lost its lease", task.ClaimedByAgentID)
	}

	// The successor reclaims and its evaluation lands once.
	drainReDeriveOnce(t, r)
	if firings := firingsForTask(t, database, taskID); len(firings) != 1 {
		t.Errorf("successor admitted %d firing(s), want 1", len(firings))
	}
	row = reDeriveRowFor(t, database, taskID)
	if row.status != workitem.StatusDone || row.generation != 2 {
		t.Errorf("row after the successor = %+v, want done at generation 2", row)
	}
}

// failingHandlerStore fails the trigger lookup the evaluation makes per
// task, leaving every other store on the path working — the shape of a
// transient DB blip mid-evaluation.
type failingHandlerStore struct {
	db.EventHandlerStore
	err error
}

func (s failingHandlerStore) GetEnabledForEventSystem(context.Context, string, string) ([]domain.EventHandler, error) {
	return nil, s.err
}

// requireBackoff asserts a requeued row's retry time lies in the kind's
// jitter band for the attempt that failed: Base * 2^(attempt-1), within
// ±25%, measured from now. The bands do not overlap, so a delay that failed
// to grow lands outside its attempt's band.
func requireBackoff(t *testing.T, row reDeriveRow, attempt int) {
	t.Helper()
	if !row.nextAttempt.Valid {
		t.Fatalf("attempt %d: next_attempt_at is NULL, want a backoff", attempt)
	}
	next, err := time.Parse("2006-01-02 15:04:05.000", row.nextAttempt.String)
	if err != nil {
		t.Fatalf("attempt %d: parse next_attempt_at %q: %v", attempt, row.nextAttempt.String, err)
	}
	delay := next.Sub(time.Now().UTC())
	spec := taskReDeriveKind.Policy.Backoff
	lo := workitem.Backoff(spec, attempt, func() float64 { return 0 })
	hi := workitem.Backoff(spec, attempt, func() float64 { return 1 })
	// The write happened moments ago, so the delay measured now is a little
	// under what was written; a second of slack covers a slow runner.
	if delay < lo-time.Second || delay > hi {
		t.Errorf("attempt %d: retry in %s, want within the backoff band [%s, %s]", attempt, delay, lo, hi)
	}
}
