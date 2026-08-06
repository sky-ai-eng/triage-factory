package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

// captureHandler is a minimal slog.Handler that records emitted entries so a
// test can assert exactly what — and how often — a code path logged. Mirrors
// internal/github/client_test.go's helper of the same name/shape.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) recorded() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// TestScoreTasks_BatchesAndCountsSkips exercises the scoreFn seam:
// scoreTasks chunks tasks into batchSize batches, dispatches each through the
// injectable scoreFn, aggregates the successful scores, and counts exactly the
// tasks in failed batches as skipped — by the batch's actual length, not a
// flat batchSize. A stubbed scoreFn drives this without spawning an agent
// subprocess, the unit-test seam the scorer previously lacked.
func TestScoreTasks_BatchesAndCountsSkips(t *testing.T) {
	// nil stores/secrets/recorder/limiter: scoreTasks skips description
	// loading when entities is nil and never touches the others on this path.
	r := NewRunner(nil, nil, "org-x", nil, nil, nil, nil, RunnerCallbacks{})

	var calls int32
	r.scoreFn = func(_ context.Context, tasks []TaskInput, orgID string, _ agentproc.SecretsReader) ([]TaskScore, error) {
		atomic.AddInt32(&calls, 1)
		if orgID != "org-x" {
			t.Errorf("scoreFn orgID = %q; want org-x (the Runner's org must thread through)", orgID)
		}
		for _, in := range tasks {
			if in.ID == "fail-me" {
				return nil, errors.New("simulated batch failure")
			}
		}
		out := make([]TaskScore, len(tasks))
		for i, in := range tasks {
			out[i] = TaskScore{ID: in.ID, PriorityScore: 50}
		}
		return out, nil
	}

	// 25 tasks → batches of 10, 10, 5. The poisoned task lands in the second
	// (full) batch, so its 10 tasks count as skipped.
	tasks := make([]domain.Task, 25)
	for i := range tasks {
		tasks[i] = domain.Task{ID: fmt.Sprintf("t-%d", i)}
	}
	tasks[12].ID = "fail-me"

	scores, skipped, err := r.scoreTasks(context.Background(), tasks)
	if err != nil {
		t.Fatalf("scoreTasks: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("scoreFn called %d times; want 3 (ceil(25/%d))", got, batchSize)
	}
	if skipped != 10 {
		t.Errorf("skipped = %d; want 10 (the full failed batch)", skipped)
	}
	if len(scores) != 15 {
		t.Errorf("aggregated scores = %d; want 15 (25 - 10 skipped)", len(scores))
	}
}

// TestScoreTasks_ProviderBackoff_LogsQuietly pins the systemllm circuit
// breaker's caller-side behavior (scorer.go's IsProviderBackoff check): a
// batch failing with a breaker skip is still counted as skipped for retry
// exactly like any other failure, but it must log at Info, never at the
// Warn level a genuine failure gets — a boot-time overload shouldn't read
// as a wall of alarms.
func TestScoreTasks_ProviderBackoff_LogsQuietly(t *testing.T) {
	logs := &captureHandler{}
	prev := aiLog
	aiLog = slog.New(logs)
	t.Cleanup(func() { aiLog = prev })

	r := NewRunner(nil, nil, "org-x", nil, nil, nil, nil, RunnerCallbacks{})
	r.scoreFn = func(context.Context, []TaskInput, string, agentproc.SecretsReader) ([]TaskScore, error) {
		return nil, &systemllm.ErrProviderBackoff{Provider: "anthropic-direct:default"}
	}

	tasks := []domain.Task{{ID: "t-1"}}
	_, skipped, err := r.scoreTasks(context.Background(), tasks)
	if err != nil {
		t.Fatalf("scoreTasks: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 — a breaker skip must still count the batch as skipped for retry", skipped)
	}

	recs := logs.recorded()
	if len(recs) != 1 {
		t.Fatalf("expected exactly one log record, got %d", len(recs))
	}
	if got := recs[0]; got.Level != slog.LevelInfo {
		t.Errorf("log level = %v, want Info for a provider-backoff skip (got message %q)", got.Level, got.Message)
	}
}

// TestRunner_StopIdempotent pins that Stop is safe to call more than once
// (guarded by stopOnce); a bare close(r.stop) would panic on the second call.
func TestRunner_StopIdempotent(t *testing.T) {
	r := NewRunner(nil, nil, "org-x", nil, nil, nil, nil, RunnerCallbacks{})
	r.Start()
	r.Stop()
	r.Stop() // must not panic
}

// stubScoreStore is a minimal db.ScoreStore for testing Runner.run end-to-end
// without a real database. UnscoredTasks returns the configured task list
// exactly once; the mutation methods are no-ops.
type stubScoreStore struct {
	tasks   []domain.Task
	fetched bool
}

func (s *stubScoreStore) UnscoredTasks(_ context.Context, _ string) ([]domain.Task, error) {
	if s.fetched {
		return nil, nil
	}
	s.fetched = true
	return s.tasks, nil
}
func (s *stubScoreStore) MarkScoring(_ context.Context, _ string, _ []string) error {
	return nil
}
func (s *stubScoreStore) ResetScoringToPending(_ context.Context, _ string, _ []string) error {
	return nil
}
func (s *stubScoreStore) UpdateTaskScores(_ context.Context, _ string, _ []domain.TaskScoreUpdate) error {
	return nil
}

// TestRun_OnScoringCompletedReceivesOnlyFreshlyScored pins that Runner.run
// passes only the IDs of tasks that actually received fresh scores to
// OnScoringCompleted — not the full set picked at cycle start. When some
// batches fail, the skipped tasks are reset to 'pending' and excluded from
// updates; their IDs must not appear in the callback or ReDeriveAfterScoring
// would fire triggers against stale scores from a prior cycle.
func TestRun_OnScoringCompletedReceivesOnlyFreshlyScored(t *testing.T) {
	// 25 tasks → 3 batches (10, 10, 5). The poisoned task lands in the
	// second batch so 10 tasks are skipped; the other 15 score fine.
	allTasks := make([]domain.Task, 25)
	for i := range allTasks {
		allTasks[i] = domain.Task{ID: fmt.Sprintf("t-%d", i)}
	}
	allTasks[12].ID = "fail-me" // poisons the second batch (indices 10-19)

	store := &stubScoreStore{tasks: allTasks}
	r := NewRunner(store, nil, "org-y", nil, nil, nil, nil, RunnerCallbacks{})

	r.scoreFn = func(_ context.Context, tasks []TaskInput, _ string, _ agentproc.SecretsReader) ([]TaskScore, error) {
		for _, in := range tasks {
			if in.ID == "fail-me" {
				return nil, errors.New("simulated batch failure")
			}
		}
		out := make([]TaskScore, len(tasks))
		for i, in := range tasks {
			out[i] = TaskScore{ID: in.ID, PriorityScore: 50}
		}
		return out, nil
	}

	var mu sync.Mutex
	var gotOrgID string
	var gotIDs []string
	r.callbacks.OnScoringCompleted = func(_ context.Context, orgID string, ids []string) {
		mu.Lock()
		defer mu.Unlock()
		gotOrgID = orgID
		gotIDs = append(gotIDs, ids...)
	}

	r.run(context.Background())

	mu.Lock()
	defer mu.Unlock()

	if gotOrgID != "org-y" {
		t.Errorf("OnScoringCompleted orgID = %q; want org-y", gotOrgID)
	}
	if len(gotIDs) != 15 {
		t.Errorf("OnScoringCompleted received %d IDs; want 15 (only the 15 successfully-scored tasks, not the 10 skipped)", len(gotIDs))
	}
	idSet := make(map[string]bool, len(gotIDs))
	for _, id := range gotIDs {
		idSet[id] = true
	}
	if idSet["fail-me"] {
		t.Error(`OnScoringCompleted received "fail-me"; want only IDs of tasks that went through UpdateTaskScores`)
	}
	// All tasks in the failed second batch (indices 10-19) must be absent.
	for i := 10; i < 20; i++ {
		id := fmt.Sprintf("t-%d", i)
		if idSet[id] {
			t.Errorf("OnScoringCompleted received skipped task %s; want only freshly-scored IDs", id)
		}
	}
}

// TestScoreTasks_EmptyReturnsZero pins the early-out: no tasks means no scoreFn
// calls and a clean zero result.
func TestScoreTasks_EmptyReturnsZero(t *testing.T) {
	r := NewRunner(nil, nil, "org-x", nil, nil, nil, nil, RunnerCallbacks{})
	var calls int32
	r.scoreFn = func(context.Context, []TaskInput, string, agentproc.SecretsReader) ([]TaskScore, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	scores, skipped, err := r.scoreTasks(context.Background(), nil)
	if err != nil || skipped != 0 || len(scores) != 0 {
		t.Errorf("scoreTasks(nil) = (%v, %d, %v); want (nil, 0, nil)", scores, skipped, err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("scoreFn called %d times for empty input; want 0", got)
	}
}
