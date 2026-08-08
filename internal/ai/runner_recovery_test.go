package ai

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRun_RescoresCrashResidue is the recovery case the scorer had no
// answer for: a cycle that stamped 'in_progress' and then died left its
// tasks invisible to UnscoredTasks forever, so their autonomy_suitability
// stayed NULL and every min_autonomy_suitability-deferred trigger on them
// silently never fired. The next cycle must pick them up.
func TestRun_RescoresCrashResidue(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)

	residue := seedScoringTasks(t, database, 2)
	fresh := seedScoringTasks(t, database, 1)

	// The crash: a prior cycle claimed these rows and never came back to
	// score them or reset them.
	if err := stores.Scores.MarkScoring(ctx, runmode.LocalDefaultOrgID, residue); err != nil {
		t.Fatalf("MarkScoring (simulated crashed cycle): %v", err)
	}

	var completed []string
	r := NewRunner(stores.Scores, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{
		OnScoringCompleted: func(_ context.Context, _ string, ids []string) { completed = ids },
	})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	// Every task — residue and fresh alike — is scored, and the score the
	// deferred triggers gate on is populated.
	for _, id := range append(append([]string{}, residue...), fresh...) {
		status, autonomy := readScoringState(t, database, id)
		if status != "scored" {
			t.Errorf("task %s: scoring_status = %q, want scored", id, status)
		}
		if autonomy == nil {
			t.Errorf("task %s: autonomy_suitability is NULL; a deferred trigger's re-derive would return early on it", id)
		}
	}

	// OnScoringCompleted carries the IDs into ReDeriveAfterScoring, so the
	// recovered rows have to be in it — that handoff is what actually fires
	// the deferred triggers.
	inCallback := map[string]bool{}
	for _, id := range completed {
		inCallback[id] = true
	}
	for _, id := range residue {
		if !inCallback[id] {
			t.Errorf("recovered task %s missing from OnScoringCompleted; its deferred triggers would still never fire", id)
		}
	}
}

// TestRun_DoesNotResetItsOwnClaims pins the ordering invariant that makes
// a timestamp-free reset safe: recovery runs strictly before the cycle's
// own MarkScoring, so no cycle can un-claim the rows it is about to score.
func TestRun_DoesNotResetItsOwnClaims(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)
	seedScoringTasks(t, database, 3)

	rec := &callOrderScoreStore{ScoreStore: stores.Scores}
	r := NewRunner(rec, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{})
	// Observe the DB mid-cycle: by the time the batch is scored the rows
	// are claimed, and nothing may have reset them since.
	var claimedDuringScoring int
	r.scoreFn = func(ctx context.Context, tasks []TaskInput, orgID string, secrets agentproc.SecretsReader) ([]TaskScore, error) {
		claimedDuringScoring = countByScoringStatus(t, database, "in_progress")
		return stubScoreFn(0.8)(ctx, tasks, orgID, secrets)
	}

	r.run(ctx)

	if claimedDuringScoring != 3 {
		t.Errorf("in_progress rows while the batch was in flight = %d, want 3", claimedDuringScoring)
	}
	calls := rec.recorded()
	if len(calls) == 0 || calls[0] != "ResetStaleScoring" {
		t.Fatalf("cycle call order = %v, want ResetStaleScoring first", calls)
	}
	for i, c := range calls {
		if c == "MarkScoring" {
			for _, later := range calls[i:] {
				if later == "ResetStaleScoring" {
					t.Errorf("cycle call order = %v; ResetStaleScoring ran after MarkScoring and would strip this cycle's own claims", calls)
				}
			}
			break
		}
	}
}

// TestRun_ResetFailureStillScores pins the best-effort posture: recovery
// is an optimization on top of the cycle, so a store error on the reset
// leaves the residue for a later cycle rather than costing this one the
// tasks it can still score.
func TestRun_ResetFailureStillScores(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)
	ids := seedScoringTasks(t, database, 1)

	rec := &callOrderScoreStore{ScoreStore: stores.Scores, resetErr: fmt.Errorf("simulated store failure")}
	r := NewRunner(rec, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	if status, _ := readScoringState(t, database, ids[0]); status != "scored" {
		t.Errorf("scoring_status = %q, want scored — a failed stale reset must not abort the cycle", status)
	}
}

// TestRun_DrainsOwedReDerivesAtCycleStart is the far-side sibling of the
// residue case above: the scores committed, and the process died before the
// post-scoring re-derive ran. The task is 'scored', so no future cycle
// re-picks it and its deferred triggers would never be evaluated — unless
// the cycle drains the debt the scores write recorded.
func TestRun_DrainsOwedReDerivesAtCycleStart(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)

	crashed := seedScoringTasks(t, database, 1)[0]

	// The crash: scores committed (which owes a re-derive), callback never
	// ran. Written through the store so the owed mark comes from the same
	// statement production uses, not a hand-set column.
	if err := stores.Scores.UpdateTaskScores(ctx, runmode.LocalDefaultOrgID, []domain.TaskScoreUpdate{{
		ID: crashed, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores (simulated crashed cycle): %v", err)
	}

	// The re-derive pass, as the router implements it: evaluate, then
	// discharge what was evaluated.
	var drained [][]string
	r := NewRunner(stores.Scores, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{
		OnReDeriveOwed: func(ctx context.Context, orgID string, ids []string) {
			drained = append(drained, ids)
			if err := stores.Scores.ClearReDeriveOwed(ctx, orgID, ids); err != nil {
				t.Errorf("ClearReDeriveOwed: %v", err)
			}
		},
	})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	if len(drained) != 1 || len(drained[0]) != 1 || drained[0][0] != crashed {
		t.Fatalf("cycle drained %v, want one pass over [%s] — the crashed task's deferred triggers are only reachable through it", drained, crashed)
	}

	// The debt is discharged, so the next cycle is a no-op rather than a
	// re-derive of the same task forever.
	r.run(ctx)
	if len(drained) != 1 {
		t.Errorf("second cycle drained again (%v); a discharged debt must not re-fire", drained)
	}
}

// TestRun_OwedDrainPrecedesThisCyclesScores pins the ordering that keeps the
// drain's clear from racing the cycle's own scores write: the scorer is
// single-flight per org, so draining before MarkScoring means no fresh mark
// can be raised on a task while the drain is deciding to clear its old one.
// Reversed, the drain could discharge a debt created by scores it never saw.
func TestRun_OwedDrainPrecedesThisCyclesScores(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)
	seedScoringTasks(t, database, 2)

	rec := &callOrderScoreStore{ScoreStore: stores.Scores}
	r := NewRunner(rec, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{
		OnReDeriveOwed: func(context.Context, string, []string) {},
	})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	calls := rec.recorded()
	drainAt, markAt := -1, -1
	for i, c := range calls {
		if c == "TasksOwedReDerive" && drainAt < 0 {
			drainAt = i
		}
		if c == "MarkScoring" && markAt < 0 {
			markAt = i
		}
	}
	if drainAt < 0 {
		t.Fatalf("cycle call order = %v; the owed-re-derive drain never ran", calls)
	}
	if markAt >= 0 && drainAt > markAt {
		t.Errorf("cycle call order = %v; the drain ran after MarkScoring and could clear a mark this cycle raised", calls)
	}
}

// TestRun_StaleScoringAndOwedReDeriveCoexist covers a task carrying both
// halves of the epic's scoring-crash damage at once: claimed by a cycle that
// died mid-scoring, and still owing a re-derive from the cycle before that.
// The two sweeps are independent — one keys on scoring_status, the other on
// the owed mark — and one cycle has to discharge both.
func TestRun_StaleScoringAndOwedReDeriveCoexist(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)
	taskID := seedScoringTasks(t, database, 1)[0]

	// Cycle 1 scored it (owing a re-derive) and its callback never ran.
	if err := stores.Scores.UpdateTaskScores(ctx, runmode.LocalDefaultOrgID, []domain.TaskScoreUpdate{{
		ID: taskID, PriorityScore: 0.5, AutonomySuitability: 0.9, Summary: "s", PriorityReasoning: "r",
	}}); err != nil {
		t.Fatalf("UpdateTaskScores: %v", err)
	}
	// A later event put it back in the queue; cycle 2 claimed it and died.
	if _, err := database.Exec(`UPDATE tasks SET scoring_status = 'pending' WHERE id = ?`, taskID); err != nil {
		t.Fatalf("re-queue for scoring: %v", err)
	}
	if err := stores.Scores.MarkScoring(ctx, runmode.LocalDefaultOrgID, []string{taskID}); err != nil {
		t.Fatalf("MarkScoring (simulated crashed cycle): %v", err)
	}

	var drained []string
	var scoredInCycle []string
	r := NewRunner(stores.Scores, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{
		OnReDeriveOwed: func(ctx context.Context, orgID string, ids []string) {
			drained = append(drained, ids...)
			if err := stores.Scores.ClearReDeriveOwed(ctx, orgID, ids); err != nil {
				t.Errorf("ClearReDeriveOwed: %v", err)
			}
		},
		OnScoringCompleted: func(_ context.Context, _ string, ids []string) { scoredInCycle = ids },
	})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	if len(drained) != 1 || drained[0] != taskID {
		t.Errorf("drained %v, want [%s] — the stale-scoring reset must not swallow the outstanding re-derive", drained, taskID)
	}
	if len(scoredInCycle) != 1 || scoredInCycle[0] != taskID {
		t.Errorf("freshly scored = %v, want [%s] — the owed mark must not hide the task from the reset that re-queues it", scoredInCycle, taskID)
	}
	// The fresh scores raise a fresh mark for the completion callback to
	// discharge; the drain cleared only the debt it evaluated.
	owed, err := stores.Scores.TasksOwedReDerive(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("TasksOwedReDerive: %v", err)
	}
	if len(owed) != 1 || owed[0] != taskID {
		t.Errorf("owed after the cycle = %v, want [%s] — this cycle's own scores owe their own re-derive", owed, taskID)
	}
}

// TestRun_OwedDrainFailureStillScores mirrors the stale-reset posture: the
// drain is recovery layered on top of a cycle, so a store error leaves the
// debt for the next cycle rather than costing this one the tasks it can
// still score.
func TestRun_OwedDrainFailureStillScores(t *testing.T) {
	ctx := context.Background()
	database := newScoringTestDB(t)
	stores := sqlitestore.New(database)
	ids := seedScoringTasks(t, database, 1)

	rec := &callOrderScoreStore{ScoreStore: stores.Scores, owedErr: fmt.Errorf("simulated store failure")}
	drained := false
	r := NewRunner(rec, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil, RunnerCallbacks{
		OnReDeriveOwed: func(context.Context, string, []string) { drained = true },
	})
	r.scoreFn = stubScoreFn(0.8)

	r.run(ctx)

	if drained {
		t.Error("the re-derive pass ran on an unreadable owed set; a failed list must produce no work, not an empty pass")
	}
	if status, _ := readScoringState(t, database, ids[0]); status != "scored" {
		t.Errorf("scoring_status = %q, want scored — a failed owed-set read must not abort the cycle", status)
	}
}

// stubScoreFn returns a batch scorer that scores every input with the
// given autonomy suitability. No subprocess, no model call.
func stubScoreFn(autonomy float64) batchScoreFn {
	return func(_ context.Context, tasks []TaskInput, _ string, _ agentproc.SecretsReader) ([]TaskScore, error) {
		out := make([]TaskScore, len(tasks))
		for i, in := range tasks {
			out[i] = TaskScore{
				ID:                  in.ID,
				PriorityScore:       0.5,
				AutonomySuitability: autonomy,
				PriorityReasoning:   "stub",
				Summary:             "stub",
			}
		}
		return out, nil
	}
}

// callOrderScoreStore wraps a real ScoreStore and records the order of
// the cycle's status writes. resetErr, when set, fails ResetStaleScoring
// without touching the wrapped store.
type callOrderScoreStore struct {
	db.ScoreStore
	resetErr error
	owedErr  error

	mu    sync.Mutex
	calls []string
}

func (s *callOrderScoreStore) record(name string) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	s.mu.Unlock()
}

func (s *callOrderScoreStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.calls...)
}

func (s *callOrderScoreStore) ResetStaleScoring(ctx context.Context, orgID string) (int, error) {
	s.record("ResetStaleScoring")
	if s.resetErr != nil {
		return 0, s.resetErr
	}
	return s.ScoreStore.ResetStaleScoring(ctx, orgID)
}

func (s *callOrderScoreStore) MarkScoring(ctx context.Context, orgID string, taskIDs []string) error {
	s.record("MarkScoring")
	return s.ScoreStore.MarkScoring(ctx, orgID, taskIDs)
}

func (s *callOrderScoreStore) ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error {
	s.record("ResetScoringToPending")
	return s.ScoreStore.ResetScoringToPending(ctx, orgID, taskIDs)
}

func (s *callOrderScoreStore) TasksOwedReDerive(ctx context.Context, orgID string) ([]string, error) {
	s.record("TasksOwedReDerive")
	if s.owedErr != nil {
		return nil, s.owedErr
	}
	return s.ScoreStore.TasksOwedReDerive(ctx, orgID)
}

// newScoringTestDB opens an in-memory SQLite with the full schema.
func newScoringTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// seedScoringTasks inserts n queued+pending tasks (with the entity and
// event rows their FKs need) and returns their IDs.
func seedScoringTasks(t *testing.T, database *sql.DB, n int) []string {
	t.Helper()
	now := time.Now().UTC()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		entityID := uuid.New().String()
		eventID := uuid.New().String()
		taskID := uuid.New().String()
		sourceID := fmt.Sprintf("scoring-pr-%d-%d", i, time.Now().UnixNano())
		const eventType = "github:pr:opened" // seeded in events_catalog

		if _, err := database.Exec(`
			INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
			VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
		`, entityID, sourceID, fmt.Sprintf("Scoring PR %d", i), "https://example/pr/"+sourceID, now); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if _, err := database.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES (?, ?, ?, '', '{}', ?)
		`, eventID, entityID, eventType, now); err != nil {
			t.Fatalf("seed event: %v", err)
		}
		if _, err := database.Exec(`
			INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
			                   status, scoring_status, created_at)
			VALUES (?, ?, ?, '', ?, 'queued', 'pending', ?)
		`, taskID, entityID, eventType, eventID, now); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		ids = append(ids, taskID)
	}
	return ids
}

func readScoringState(t *testing.T, database *sql.DB, taskID string) (status string, autonomy *float64) {
	t.Helper()
	if err := database.QueryRow(
		`SELECT scoring_status, autonomy_suitability FROM tasks WHERE id = ?`, taskID,
	).Scan(&status, &autonomy); err != nil {
		t.Fatalf("read scoring state for %s: %v", taskID, err)
	}
	return status, autonomy
}

func countByScoringStatus(t *testing.T, database *sql.DB, status string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE scoring_status = ?`, status,
	).Scan(&n); err != nil {
		t.Fatalf("count %s tasks: %v", status, err)
	}
	return n
}
