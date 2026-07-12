package ai

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

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

// TestRunner_StopIdempotent pins that Stop is safe to call more than once
// (guarded by stopOnce); a bare close(r.stop) would panic on the second call.
func TestRunner_StopIdempotent(t *testing.T) {
	r := NewRunner(nil, nil, "org-x", nil, nil, nil, nil, RunnerCallbacks{})
	r.Start()
	r.Stop()
	r.Stop() // must not panic
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
