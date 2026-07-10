package app

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestStartCapBrokerIfSandboxing_FailClosed pins the retirement of the
// privsep rollback flag: on a host that sandboxes, a broker that can't start
// is FATAL. startCapBrokerIfSandboxing surfaces the error rather than
// silently continuing — there is no in-process launch to fall back to, so a
// swallowed error would mean running with no sandbox launcher at all.
func TestStartCapBrokerIfSandboxing_FailClosed(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	if !agentproc.WillSandbox() {
		t.Skip("host does not sandbox (non-Linux); the fail-closed path is Linux-only")
	}
	orig := startCapBrokerFn
	t.Cleanup(func() { startCapBrokerFn = orig })
	wantErr := errors.New("broker refused to start")
	startCapBrokerFn = func(context.Context) (capBrokerHandle, error) {
		return nil, wantErr
	}

	a := &App{}
	err := a.startCapBrokerIfSandboxing(context.Background())
	if err == nil {
		t.Fatal("expected a fatal error when the broker can't start on a sandboxing host — no silent fallback to a less-isolated launch")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
	if a.capBroker != nil {
		t.Error("capBroker set despite a start failure")
	}
}

// TestStartCapBrokerIfSandboxing_NoOpWhenNotSandboxing pins that a
// non-sandboxing host (local mode) never starts a broker — the broker exists
// only to protect the sandbox, so local mode is unaffected by the split.
func TestStartCapBrokerIfSandboxing_NoOpWhenNotSandboxing(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	orig := startCapBrokerFn
	t.Cleanup(func() { startCapBrokerFn = orig })
	called := false
	startCapBrokerFn = func(context.Context) (capBrokerHandle, error) {
		called = true
		return nil, errors.New("should not be called")
	}

	a := &App{}
	if err := a.startCapBrokerIfSandboxing(context.Background()); err != nil {
		t.Fatalf("local mode should be a no-op, got error: %v", err)
	}
	if called {
		t.Error("startCapBroker called in local mode — the broker must only start when the host sandboxes")
	}
	if a.capBroker != nil {
		t.Error("capBroker set in local mode")
	}
}
