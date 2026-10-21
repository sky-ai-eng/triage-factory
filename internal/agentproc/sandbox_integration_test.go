package agentproc

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentVisibleHelpers_LocalPassthrough exercises the un-sandboxed path
// (tests run in local mode, so shouldSandbox() is false): both helpers must
// return the host value unchanged. The sandbox branch (cell.WorkDir and
// cell.TFBinaryPath) can't be unit-asserted here without forcing multi mode
// and a Linux host.
func TestAgentVisibleHelpers_LocalPassthrough(t *testing.T) {
	if got := AgentVisibleRoot("/data/worktrees/abc"); got != "/data/worktrees/abc" {
		t.Errorf("AgentVisibleRoot local passthrough: got %q want %q", got, "/data/worktrees/abc")
	}
	if got := AgentVisibleBinary("/home/user/bin/triagefactory"); got != "/home/user/bin/triagefactory" {
		t.Errorf("AgentVisibleBinary local passthrough: got %q want %q", got, "/home/user/bin/triagefactory")
	}
}

// TestWillSandbox_TracksGate confirms WillSandbox mirrors the internal gate
// (false in the default local-mode test environment).
func TestWillSandbox_TracksGate(t *testing.T) {
	if WillSandbox() != shouldSandbox() {
		t.Errorf("WillSandbox() = %v, want it to mirror shouldSandbox() = %v", WillSandbox(), shouldSandbox())
	}
}

// TestRefuseMultiModeSDKLoop pins the fail-closed gate Run and RunInteractive
// both call before spawning anything: multi mode refuses with
// errSDKLoopInMultiMode (the SDK loop has no isolation to offer there — its
// only wrapper on Linux is the bubblewrap courtesy sandbox, not a tenant
// boundary), and local mode — the SDK loop's only legitimate caller — is a
// no-op.
func TestRefuseMultiModeSDKLoop(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	if err := refuseMultiModeSDKLoop(); !errors.Is(err, errSDKLoopInMultiMode) {
		t.Errorf("refuseMultiModeSDKLoop() in multi mode = %v, want errSDKLoopInMultiMode", err)
	}

	runmode.SetForTest(t, runmode.ModeLocal)
	if err := refuseMultiModeSDKLoop(); err != nil {
		t.Errorf("refuseMultiModeSDKLoop() in local mode = %v, want nil", err)
	}
}
