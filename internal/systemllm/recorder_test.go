package systemllm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type fakeStore struct {
	rows []domain.SystemLLMRun
	err  error
}

func (f *fakeStore) Record(_ context.Context, row domain.SystemLLMRun) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, row)
	return nil
}

// TestRecorder_BuildsRowFromOutcomeAndUsage pins the happy path: cost /
// duration / turns / is_error come from the Result, tokens from the sink,
// and the per-call context + marshaled metadata from the Call.
func TestRecorder_BuildsRowFromOutcomeAndUsage(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)
	started := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	r.Record(context.Background(), Call{
		OrgID:     "org-1",
		Job:       JobScorer,
		Model:     "haiku",
		StartedAt: started,
		Metadata:  map[string]any{"batch_size": 10},
	},
		&agentproc.Outcome{Result: &agentproc.Result{
			CostUSD: 0.02, DurationMs: 1200, NumTurns: 1, IsError: false,
		}},
		&agentproc.UsageSink{InputTokens: 5, OutputTokens: 6, CacheReadTokens: 7, CacheCreationTokens: 8},
	)

	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	got := fs.rows[0]
	if got.OrgID != "org-1" || got.Job != JobScorer || got.Model != "haiku" {
		t.Errorf("context fields wrong: %+v", got)
	}
	if got.TotalCostUSD != 0.02 || got.DurationMs != 1200 || got.NumTurns != 1 || got.IsError {
		t.Errorf("result fields wrong: %+v", got)
	}
	if got.InputTokens != 5 || got.OutputTokens != 6 || got.CacheReadTokens != 7 || got.CacheCreationTokens != 8 {
		t.Errorf("token fields wrong: %+v", got)
	}
	if got.MetadataJSON != `{"batch_size":10}` {
		t.Errorf("metadata = %q, want {\"batch_size\":10}", got.MetadataJSON)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
	}
	if got.CompletedAt.IsZero() {
		t.Error("completed_at should be stamped")
	}
}

// TestRecorder_NilOutcomeSkips pins the err-with-nil-outcome case: the
// call site passes a nil outcome and Record must not insert (the run never
// produced anything to account for).
func TestRecorder_NilOutcomeSkips(t *testing.T) {
	fs := &fakeStore{}
	NewRecorder(fs).Record(context.Background(), Call{Job: JobScorer}, nil, &agentproc.UsageSink{})
	if len(fs.rows) != 0 {
		t.Errorf("recorded %d rows, want 0 on nil outcome", len(fs.rows))
	}
}

// TestRecorder_NilResultRecordsAsError pins the "outcome but no terminal
// result event" case: tokens are still real, so we record, flagged
// is_error with zero cost.
func TestRecorder_NilResultRecordsAsError(t *testing.T) {
	fs := &fakeStore{}
	NewRecorder(fs).Record(context.Background(), Call{OrgID: "o", Job: JobRepoProfiler, Model: "haiku"},
		&agentproc.Outcome{Result: nil},
		&agentproc.UsageSink{InputTokens: 3},
	)
	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	got := fs.rows[0]
	if !got.IsError {
		t.Error("nil Result should record is_error=true")
	}
	if got.TotalCostUSD != 0 || got.InputTokens != 3 {
		t.Errorf("want zero cost + token passthrough, got %+v", got)
	}
}

// TestRecorder_StoreErrorSwallowed pins the load-bearing contract: a store
// failure must never escape Record (it would otherwise break the job).
func TestRecorder_StoreErrorSwallowed(t *testing.T) {
	r := NewRecorder(&fakeStore{err: errors.New("db down")})
	// Must not panic or propagate — there's no error return to check; the
	// assertion is simply that this call completes.
	r.Record(context.Background(), Call{Job: JobScorer},
		&agentproc.Outcome{Result: &agentproc.Result{}}, &agentproc.UsageSink{})
}

// TestRecorder_NilSafe pins that a nil Recorder and a nil store are both
// safe no-ops, so call sites never have to guard.
func TestRecorder_NilSafe(t *testing.T) {
	var nilRec *Recorder
	nilRec.Record(context.Background(), Call{Job: JobScorer},
		&agentproc.Outcome{Result: &agentproc.Result{}}, &agentproc.UsageSink{})

	NewRecorder(nil).Record(context.Background(), Call{Job: JobScorer},
		&agentproc.Outcome{Result: &agentproc.Result{}}, &agentproc.UsageSink{})
}

// TestRecorder_RecordDirect_BuildsRow pins RecordDirect's row shape — the
// direct-API sibling of TestRecorder_BuildsRowFromOutcomeAndUsage. NumTurns
// is always 1 (single-turn, non-streaming completions), and TraceID comes
// from the caller-supplied API response id rather than an agentproc session.
func TestRecorder_RecordDirect_BuildsRow(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)
	started := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

	r.RecordDirect(context.Background(), Call{
		OrgID:     "org-1",
		Job:       JobRepoProfiler,
		Model:     "claude-haiku-4-5-20251001",
		StartedAt: started,
		Metadata:  map[string]any{"repo_count": 3},
	}, "msg_abc123", DirectUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheCreationTokens: 2}, 0.05, 250, false)

	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	got := fs.rows[0]
	if got.OrgID != "org-1" || got.Job != JobRepoProfiler || got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("context fields wrong: %+v", got)
	}
	if got.TraceID != "msg_abc123" {
		t.Errorf("TraceID = %q, want the API response id", got.TraceID)
	}
	if got.TotalCostUSD != 0.05 || got.DurationMs != 250 || got.NumTurns != 1 || got.IsError {
		t.Errorf("result fields wrong: %+v", got)
	}
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.CacheReadTokens != 3 || got.CacheCreationTokens != 2 {
		t.Errorf("token fields wrong: %+v", got)
	}
	if got.MetadataJSON != `{"repo_count":3}` {
		t.Errorf("metadata = %q, want {\"repo_count\":3}", got.MetadataJSON)
	}
}

// TestRecorder_RecordDirect_NilSafe pins that a nil Recorder and a nil store
// are both safe no-ops for RecordDirect too, mirroring Record's contract.
func TestRecorder_RecordDirect_NilSafe(t *testing.T) {
	var nilRec *Recorder
	nilRec.RecordDirect(context.Background(), Call{Job: JobScorer}, "", DirectUsage{}, 0, 0, false)

	NewRecorder(nil).RecordDirect(context.Background(), Call{Job: JobScorer}, "", DirectUsage{}, 0, 0, false)
}

// ctxCheckingStore mimics a real DB driver: it fails the insert if the ctx it
// receives is already cancelled, and records the row otherwise.
type ctxCheckingStore struct{ rows []domain.SystemLLMRun }

func (s *ctxCheckingStore) Record(ctx context.Context, row domain.SystemLLMRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.rows = append(s.rows, row)
	return nil
}

// TestRecorder_RecordsDespiteCancelledCtx is the regression test for the
// shutdown/timeout drop: when a run completes just as its ctx is cancelled,
// the accounting row must still land. Record detaches the insert from the
// caller's ctx, so a store that rejects a cancelled ctx (every real DB
// driver) still sees a live context and writes the row.
func TestRecorder_RecordsDespiteCancelledCtx(t *testing.T) {
	cs := &ctxCheckingStore{}
	r := NewRecorder(cs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before Record is called, as on shutdown

	r.Record(ctx, Call{OrgID: "o", Job: JobScorer, Model: "haiku"},
		&agentproc.Outcome{Result: &agentproc.Result{CostUSD: 0.01}}, &agentproc.UsageSink{InputTokens: 9})

	if len(cs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 (insert must survive a cancelled caller ctx)", len(cs.rows))
	}
	if cs.rows[0].InputTokens != 9 {
		t.Errorf("row not carried through: %+v", cs.rows[0])
	}
}
