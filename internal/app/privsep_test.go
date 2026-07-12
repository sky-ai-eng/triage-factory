package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestStartCapBrokerIfSandboxing_RefusesOrchestratorUIDInSidecarBand pins
// the authoritative backstop against an operator repointing
// TF_ORCHESTRATOR_UID into the reserved sidecar uid range: sandbox's own
// init() assertion can only see the DEFAULT orchestrator uid, not an
// operator override, so this runtime check — reading the process's own
// observed uid via the currentUID seam — is what actually catches it. A
// collision there would let the orchestrator and a run's sidecar share a
// uid, defeating the per-run-uid isolation the whole sidecar design relies
// on, so this must refuse to start the broker at all rather than merely
// warn.
func TestStartCapBrokerIfSandboxing_RefusesOrchestratorUIDInSidecarBand(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	if !agentproc.WillSandbox() {
		t.Skip("host does not sandbox (non-Linux); the fail-closed path is Linux-only")
	}
	origUID := currentUID
	t.Cleanup(func() { currentUID = origUID })
	currentUID = func() int { return sandbox.SidecarUIDBase + 5 }

	origBroker := startCapBrokerFn
	t.Cleanup(func() { startCapBrokerFn = origBroker })
	called := false
	startCapBrokerFn = func(context.Context) (capBrokerHandle, func(context.Context) error, error) {
		called = true
		return nil, nil, nil
	}

	a := &App{}
	err := a.startCapBrokerIfSandboxing(context.Background())
	if err == nil {
		t.Fatal("expected a fatal error when the orchestrator's own uid falls inside the reserved sidecar band")
	}
	if !strings.Contains(err.Error(), "sidecar uid band") {
		t.Errorf("err = %v, want it to name the sidecar uid band collision", err)
	}
	if called {
		t.Error("startCapBroker was invoked despite the uid-collision guard — the broker must never start in this state")
	}
}

// TestStartCapBrokerIfSandboxing_AllowsOrchestratorUIDOutsideSidecarBand
// pins that the new uid-collision guard doesn't false-positive on an
// ordinary, non-colliding orchestrator uid (the documented default,
// 10001, among others) — the happy path this whole check must not break.
func TestStartCapBrokerIfSandboxing_AllowsOrchestratorUIDOutsideSidecarBand(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	if !agentproc.WillSandbox() {
		t.Skip("host does not sandbox (non-Linux); the fail-closed path is Linux-only")
	}
	origUID := currentUID
	t.Cleanup(func() { currentUID = origUID })
	currentUID = func() int { return 10001 }

	origBroker := startCapBrokerFn
	t.Cleanup(func() { startCapBrokerFn = origBroker })
	called := false
	startCapBrokerFn = func(context.Context) (capBrokerHandle, func(context.Context) error, error) {
		called = true
		return nil, nil, nil
	}

	a := &App{}
	if err := a.startCapBrokerIfSandboxing(context.Background()); err != nil {
		t.Fatalf("startCapBrokerIfSandboxing: %v", err)
	}
	if !called {
		t.Error("startCapBroker was never invoked for a non-colliding orchestrator uid")
	}
}

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
	startCapBrokerFn = func(context.Context) (capBrokerHandle, func(context.Context) error, error) {
		return nil, nil, wantErr
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
	startCapBrokerFn = func(context.Context) (capBrokerHandle, func(context.Context) error, error) {
		called = true
		return nil, nil, errors.New("should not be called")
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
