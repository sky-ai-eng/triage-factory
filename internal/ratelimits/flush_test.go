package ratelimits

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// fakeReadingStore records what each UpsertSystem call was handed, failing
// while fail is set.
type fakeReadingStore struct {
	db.RateLimitReadingStore
	fail    bool
	written [][]domain.RateLimitReading
	swept   chan struct{}
}

func (f *fakeReadingStore) DeleteExpiredSystem(context.Context, time.Time) (int64, error) {
	f.swept <- struct{}{}
	return 0, nil
}

func (f *fakeReadingStore) UpsertSystem(_ context.Context, readings []domain.RateLimitReading) error {
	if f.fail {
		return errors.New("database unavailable")
	}
	f.written = append(f.written, readings)
	return nil
}

// TestFlush_RetriesAfterAFailedWrite pins the flusher's one promise: a
// transient database fault delays a reading, it never drops one. A failed
// write leaves the mark where it was, so the next tick hands over the same
// readings plus anything recorded since; a successful one advances it, so
// nothing is written twice.
func TestFlush_RetriesAfterAFailedWrite(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	ctx := context.Background()
	store := &fakeReadingStore{}
	reset := time.Now().Add(time.Hour)

	upstream.RecordBudget(upstream.GitHub, "org-1", "pat", "core", upstream.Budget{Remaining: 4000, Limit: 5000, Reset: reset})

	store.fail = true
	mark := flush(ctx, store, 0)
	if mark != 0 {
		t.Fatalf("mark after a failed write = %d, want 0 (unchanged)", mark)
	}

	upstream.RecordBudget(upstream.GitHub, "org-1", "pat", "graphql", upstream.Budget{Remaining: 10, Limit: 5000, Reset: reset})
	store.fail = false
	mark = flush(ctx, store, mark)
	if len(store.written) != 1 || len(store.written[0]) != 2 {
		t.Fatalf("write after the failure = %+v, want both readings (the failed one and the new one)", store.written)
	}

	if next := flush(ctx, store, mark); next != mark || len(store.written) != 1 {
		t.Errorf("flush with nothing recorded since wrote %d batches (mark %d → %d), want no write", len(store.written)-1, mark, next)
	}

	upstream.RecordBudget(upstream.GitHub, "org-1", "pat", "core", upstream.Budget{Remaining: 3999, Limit: 5000, Reset: reset})
	flush(ctx, store, mark)
	if len(store.written) != 2 || len(store.written[1]) != 1 || store.written[1][0].Bucket != "core" {
		t.Errorf("write after one new reading = %+v, want core alone", store.written[len(store.written)-1])
	}
}

// TestRegisterNamer_PanicsOnWiringBugs pins the registry's refusals: a nil
// namer, or a second namer for an upstream that has one, is a boot failure.
func TestRegisterNamer_PanicsOnWiringBugs(t *testing.T) {
	for name, register := range map[string]func(){
		"nil namer":          func() { RegisterNamer(upstream.Linear, nil) },
		"duplicate upstream": func() { RegisterNamer(upstream.GitHub, githubNames) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterNamer did not panic")
				}
			}()
			register()
		})
	}
}

// TestRunFlusher_WritesOnceMoreWhenStopped pins the clean stop: a reading
// recorded since the last tick is written when ctx is cancelled, on a context
// of the flusher's own, rather than lost with the process.
func TestRunFlusher_WritesOnceMoreWhenStopped(t *testing.T) {
	upstream.ResetBudgetsForTest(t)
	store := &fakeReadingStore{}
	upstream.RecordBudget(upstream.GitHub, "org-1", "pat", "core", upstream.Budget{Remaining: 4000, Limit: 5000, Reset: time.Now().Add(time.Hour)})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFlusher(ctx, store, time.Hour)
	}()
	cancel()
	<-done

	if len(store.written) != 1 || len(store.written[0]) != 1 || store.written[0][0].Bucket != "core" {
		t.Errorf("writes after a stop before the first tick = %+v, want the core reading", store.written)
	}
}

// TestRunExpirySweep_SweepsOnStart pins the first pass: a newly promoted brain
// sweeps at once rather than an interval later.
func TestRunExpirySweep_SweepsOnStart(t *testing.T) {
	store := &fakeReadingStore{swept: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunExpirySweep(ctx, store, time.Hour)
	select {
	case <-store.swept:
	case <-time.After(5 * time.Second):
		t.Fatal("no sweep within 5s of starting with an hour's interval")
	}
}

// TestNames_LeavesAFailingUpstreamUnnamed pins naming as best-effort: an
// upstream whose Namer fails contributes nothing, every other upstream is
// still named, and nothing is returned for the caller to fail on.
func TestNames_LeavesAFailingUpstreamUnnamed(t *testing.T) {
	for name, namer := range map[upstream.Name]Namer{
		upstream.Linear: func(context.Context, db.TxStores, string) (map[string]string, error) {
			return nil, errors.New("workspace store unavailable")
		},
		upstream.Jira: func(context.Context, db.TxStores, string) (map[string]string, error) {
			return map[string]string{"cloud_oauth": "Acme"}, nil
		},
	} {
		RegisterNamer(name, namer)
		t.Cleanup(func() { delete(namers, name) })
	}
	var txs int
	inTx := func(fn func(db.TxStores) error) error {
		txs++
		return fn(db.TxStores{})
	}

	got := Names(context.Background(), "org-1", []upstream.Name{upstream.Linear, upstream.Jira}, inTx)
	if _, ok := got[upstream.Linear]; ok {
		t.Errorf("linear names = %v, want none: its namer failed", got[upstream.Linear])
	}
	if got[upstream.Jira]["cloud_oauth"] != "Acme" {
		t.Errorf("jira names = %v, want cloud_oauth named Acme despite linear's failure", got[upstream.Jira])
	}
	if txs != 2 {
		t.Errorf("transactions = %d, want one per namer (2)", txs)
	}
}
