package systemllm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestComplete_LocalMode_DelegatesToSubprocess pins the hard constraint that
// local mode stays byte-identical, always routing through agentproc.Run
// (the OAuth subscription flow only works through the SDK subprocess) —
// never the direct API path, even though nothing here would stop a caller
// from configuring an API key. Stubs the agentproc seam so no subprocess
// actually spawns.
func TestComplete_LocalMode_DelegatesToSubprocess(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	var gotOpts agentproc.RunOptions
	called := false
	orig := runLocal
	runLocal = func(_ context.Context, opts agentproc.RunOptions, sink agentproc.Sink) (*agentproc.Outcome, error) {
		called = true
		gotOpts = opts
		sink.OnSession("sess-1")
		return &agentproc.Outcome{
			SessionID: "sess-1",
			Result:    &agentproc.Result{Result: `{"score":42}`, CostUSD: 0.01, DurationMs: 500, NumTurns: 1},
		}, nil
	}
	t.Cleanup(func() { runLocal = orig })

	fs := &fakeStore{}
	r := NewRecorder(fs)
	result, err := r.Complete(context.Background(), CompleteOptions{
		OrgID:        "org-1",
		Job:          JobRepoProfiler,
		Message:      "combined local prompt",
		SystemPrompt: "should be ignored in local mode",
		UserMessage:  "should be ignored in local mode",
		Model:        domain.ModelHaiku,
		TraceID:      "classify-stage1",
		Secrets:      nil,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !called {
		t.Fatal("expected the local (agentproc.Run) seam to be invoked")
	}
	if gotOpts.Model != domain.ModelHaiku {
		t.Errorf("Model = %q, want the org's background-jobs model %q — the concrete id the CLI accepts", gotOpts.Model, domain.ModelHaiku)
	}
	if gotOpts.Message != "combined local prompt" {
		t.Errorf("Message = %q, want the combined prompt", gotOpts.Message)
	}
	if gotOpts.TraceID != "classify-stage1" || gotOpts.OrgID != "org-1" {
		t.Errorf("TraceID/OrgID not threaded through: %+v", gotOpts)
	}
	if result.Text != `{"score":42}` {
		t.Errorf("Text = %q, want the outcome's Result.Result", result.Text)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	if fs.rows[0].TraceID != "sess-1" {
		t.Errorf("TraceID = %q, want the subprocess session id", fs.rows[0].TraceID)
	}
}

// TestComplete_LocalMode_ErrorIncludesStderr pins the diagnostic contract a
// caller lost when scoreBatch/runVote/profileBatch stopped formatting
// stderr into their own error messages: Complete folds it in for them.
func TestComplete_LocalMode_ErrorIncludesStderr(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{Stderr: "boom: node crashed"}, agentproc.ErrClaimMemoryLimit
	}
	t.Cleanup(func() { runLocal = orig })

	r := NewRecorder(nil)
	_, err := r.Complete(context.Background(), CompleteOptions{Job: JobScorer, Model: domain.ModelHaiku})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "boom: node crashed") {
		t.Errorf("error = %q, want it to include the subprocess stderr", got)
	}
}

// TestComplete_LocalMode_ErrorResultIsAnError pins the mapping the agent
// runtime does not do for us: a captured terminal result flagged is_error is
// a failed completion, not text. agentproc.Run returns nil for it, so without
// this the refusal reaches a caller as its answer and fails later wearing the
// wrong diagnosis — a JSON parse error naming the refusal as malformed
// output. The ledger row still lands, flagged: the call spent tokens.
func TestComplete_LocalMode_ErrorResultIsAnError(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(_ context.Context, _ agentproc.RunOptions, sink agentproc.Sink) (*agentproc.Outcome, error) {
		sink.OnSession("sess-refused")
		return &agentproc.Outcome{
			SessionID: "sess-refused",
			// The shape a client-side refusal actually arrives in: the
			// subtype says success, is_error is the only marker, and Run
			// reports no error because it captured a terminal result.
			Result: &agentproc.Result{
				Subtype: "success",
				IsError: true,
				Result:  "Prompt is too long: 205000 tokens > 200000 maximum",
				CostUSD: 0.003,
			},
		}, nil
	}
	t.Cleanup(func() { runLocal = orig })

	fs := &fakeStore{}
	res, err := NewRecorder(fs).Complete(context.Background(), CompleteOptions{
		OrgID: "org-1", Job: JobMemory, Message: "prompt", Model: domain.ModelHaiku,
	})
	if err == nil {
		t.Fatal("expected an error for an is_error result, got the refusal text as a result")
	}
	if !errors.Is(err, ErrResultIsError) {
		t.Errorf("error = %v, want it to wrap ErrResultIsError so a caller can name this failure", err)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil — the text is the refusal, not an answer", res)
	}
	if !strings.Contains(err.Error(), "Prompt is too long") {
		t.Errorf("error = %q, want it to carry the runtime's own explanation", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 — the call spent tokens either way", len(fs.rows))
	}
	if !fs.rows[0].IsError {
		t.Error("ledger row should be flagged is_error")
	}
	if fs.rows[0].TotalCostUSD != 0.003 {
		t.Errorf("TotalCostUSD = %v, want the refused call's cost recorded", fs.rows[0].TotalCostUSD)
	}
}

// TestComplete_LocalMode_ErrorResultTextIsBounded pins the cut on the wrapped
// text. Nothing constrains what the runtime says — an over-long-prompt
// refusal can quote back what it refused — and this error lands in a caller's
// log line.
func TestComplete_LocalMode_ErrorResultTextIsBounded(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{Result: &agentproc.Result{
			IsError: true,
			Result:  strings.Repeat("x", resultTextLimit*3),
		}}, nil
	}
	t.Cleanup(func() { runLocal = orig })

	_, err := NewRecorder(nil).Complete(context.Background(), CompleteOptions{
		Job: JobScorer, Model: domain.ModelHaiku,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := strings.Count(err.Error(), "x"); n != resultTextLimit {
		t.Errorf("wrapped %d chars of result text, want it cut at %d", n, resultTextLimit)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Error("a cut message must say it was cut, or a reader takes the clipped text for the whole of it")
	}
}

// TestComplete_LocalMode_RunIDIsTheLedgerRowID pins the caller-minted ledger
// id through the local path: a caller that has to name the row from a row of
// its own mints the id first, so Complete must insert with it rather than let
// the store generate one nobody holds.
func TestComplete_LocalMode_RunIDIsTheLedgerRowID(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{SessionID: "sess-1", Result: &agentproc.Result{Result: "ok"}}, nil
	}
	t.Cleanup(func() { runLocal = orig })

	const minted = "11111111-2222-3333-4444-555555555555"
	fs := &fakeStore{}
	if _, err := NewRecorder(fs).Complete(context.Background(), CompleteOptions{
		OrgID: "org-1", Job: JobMemory, Message: "prompt", Model: domain.ModelHaiku, RunID: minted,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	if fs.rows[0].ID != minted {
		t.Errorf("row ID = %q, want the caller-minted %q", fs.rows[0].ID, minted)
	}
	// The id is not the idempotency key; the session id still is.
	if fs.rows[0].TraceID != "sess-1" {
		t.Errorf("TraceID = %q, want the session id — a minted row id must not displace it", fs.rows[0].TraceID)
	}
}

// TestRecordDirectCall_RunIDIsTheLedgerRowID is the direct (multi-mode)
// sibling of the test above. Both paths funnel through one insert precisely
// so they cannot disagree about this, and the test exists to keep that true.
func TestRecordDirectCall_RunIDIsTheLedgerRowID(t *testing.T) {
	const minted = "66666666-7777-8888-9999-000000000000"
	opts := completeOpts("org-1", nil)
	opts.Job = JobMemory
	opts.RunID = minted

	fs := &fakeStore{}
	NewRecorder(fs).recordDirectCall(context.Background(), opts, time.Now().UTC(), 42,
		&inference.Completion{ID: "msg_x"}, nil)

	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	if fs.rows[0].ID != minted {
		t.Errorf("row ID = %q, want the caller-minted %q", fs.rows[0].ID, minted)
	}
	if fs.rows[0].TraceID != "msg_x" {
		t.Errorf("TraceID = %q, want the response id", fs.rows[0].TraceID)
	}
}

// TestComplete_NoRunIDLeavesTheIDToTheStore pins the default every existing
// caller is on: an unset RunID reaches the store empty, which is how both
// dialects are asked to generate one.
func TestComplete_NoRunIDLeavesTheIDToTheStore(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{Result: &agentproc.Result{Result: "ok"}}, nil
	}
	t.Cleanup(func() { runLocal = orig })

	fs := &fakeStore{}
	if _, err := NewRecorder(fs).Complete(context.Background(), CompleteOptions{
		OrgID: "org-1", Job: JobScorer, Message: "prompt", Model: domain.ModelHaiku,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(fs.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(fs.rows))
	}
	if fs.rows[0].ID != "" {
		t.Errorf("row ID = %q, want empty so the store generates one", fs.rows[0].ID)
	}
}
