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
