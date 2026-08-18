package systemllm

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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
		Job:          JobClassifier,
		Message:      "combined local prompt",
		SystemPrompt: "should be ignored in local mode",
		UserMessage:  "should be ignored in local mode",
		Model:        "haiku",
		DirectModel:  "claude-haiku-4-5-20251001",
		TraceID:      "classify-stage1",
		Secrets:      nil,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !called {
		t.Fatal("expected the local (agentproc.Run) seam to be invoked")
	}
	if gotOpts.Model != "haiku" {
		t.Errorf("Model = %q, want the CLI alias %q", gotOpts.Model, "haiku")
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
// caller lost when scoreBatch/runHaiku/profileBatch stopped formatting
// stderr into their own error messages: Complete folds it in for them.
func TestComplete_LocalMode_ErrorIncludesStderr(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	orig := runLocal
	runLocal = func(context.Context, agentproc.RunOptions, agentproc.Sink) (*agentproc.Outcome, error) {
		return &agentproc.Outcome{Stderr: "boom: node crashed"}, agentproc.ErrClaimMemoryLimit
	}
	t.Cleanup(func() { runLocal = orig })

	r := NewRecorder(nil)
	_, err := r.Complete(context.Background(), CompleteOptions{Job: JobScorer, Model: "haiku"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "boom: node crashed") {
		t.Errorf("error = %q, want it to include the subprocess stderr", got)
	}
}
