package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestLocalSandboxSocketDestination is the drift guard between the two
// packages that must agree on where the agenthost socket lands inside a local
// namespace. localsandbox can't import cmd/exec/agenthost (that package
// transitively imports internal/agentproc, which imports localsandbox), so it
// carries its own copy of the constant and this — the package that wires both
// — cross-checks them. A drift makes every sandboxed run's exec verbs fail on
// a socket the daemon is serving somewhere else.
func TestLocalSandboxSocketDestination(t *testing.T) {
	if localsandbox.AgentHostSocketDest != agenthost.DefaultSocketPath {
		t.Errorf("localsandbox.AgentHostSocketDest = %q, want agenthost.DefaultSocketPath %q",
			localsandbox.AgentHostSocketDest, agenthost.DefaultSocketPath)
	}
}

// TestStartLocalSandbox_DisabledIsFreeAndNil pins that an opted-out (or
// never-resolved) posture costs nothing: no daemon, no socket, and a nil Spec
// — which is what makes the spawn byte-identical to the pre-sandbox one.
func TestStartLocalSandbox_DisabledIsFreeAndNil(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	runmode.SetLocalSandboxForTest(t, false)
	root := t.TempDir()
	paths.SetForTest(t, root)

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStores(db.Stores{})

	sbx, err := s.startLocalSandbox("conv-1", "rk-1", nil, agenthost.ConversationInfo{ConversationID: "conv-1"})
	if err != nil {
		t.Fatalf("startLocalSandbox: %v", err)
	}
	if sbx.runSpec() != nil {
		t.Errorf("runSpec() = %+v with the sandbox off, want nil", sbx.runSpec())
	}
	if _, err := os.Stat(paths.AgentHostSocketDir()); !os.IsNotExist(err) {
		t.Errorf("an opted-out run created the agenthost socket dir (err=%v)", err)
	}
	if err := sbx.Close(); err != nil {
		t.Errorf("Close on a nil channel: %v", err)
	}
}

// TestStartLocalSandbox_BuildsPlanAndDaemon covers the enabled path: the Spec
// names this run's own tree, this conversation's own gh-channel dir, and a
// socket that is actually being served.
func TestStartLocalSandbox_BuildsPlanAndDaemon(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	runmode.SetLocalSandboxForTest(t, true)
	paths.SetForTest(t, t.TempDir())

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStores(db.Stores{})

	sbx, err := s.startLocalSandbox("conv-7", "rk-7",
		&agentproc.GHChannelParams{Host: "127.0.0.1:1234"},
		agenthost.ConversationInfo{ConversationID: "conv-7", OrgID: runmode.LocalDefaultOrgID})
	if err != nil {
		t.Fatalf("startLocalSandbox: %v", err)
	}
	defer func() { _ = sbx.Close() }()

	spec := sbx.runSpec()
	if spec == nil {
		t.Fatal("runSpec() = nil with the sandbox on")
	}
	if want := worktree.RunRoot("rk-7"); spec.RunRoot != want {
		t.Errorf("RunRoot = %q, want the run tree keyed by the run-tree key %q", spec.RunRoot, want)
	}
	if want := paths.GHChannelDir("conv-7"); spec.GHChannelDir != want {
		t.Errorf("GHChannelDir = %q, want %q", spec.GHChannelDir, want)
	}
	if want := paths.AgentHostSocketPath("conv-7"); spec.AgentHostSocket != want {
		t.Errorf("AgentHostSocket = %q, want %q", spec.AgentHostSocket, want)
	}
	info, err := os.Stat(spec.AgentHostSocket)
	if err != nil {
		t.Fatalf("stat agenthost socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket", spec.AgentHostSocket)
	}

	if err := sbx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(spec.AgentHostSocket); !os.IsNotExist(err) {
		t.Errorf("socket survived Close (err=%v)", err)
	}
}

// TestStartLocalSandbox_NoGHChannelBindsNoChannelDir pins that the gh-channel
// bind tracks the channel, not the conversation: a run that never provisioned
// one must not bind a directory that doesn't exist.
func TestStartLocalSandbox_NoGHChannelBindsNoChannelDir(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	runmode.SetLocalSandboxForTest(t, true)
	paths.SetForTest(t, t.TempDir())

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStores(db.Stores{})

	sbx, err := s.startLocalSandbox("conv-8", "rk-8", nil, agenthost.ConversationInfo{ConversationID: "conv-8"})
	if err != nil {
		t.Fatalf("startLocalSandbox: %v", err)
	}
	defer func() { _ = sbx.Close() }()

	if got := sbx.runSpec().GHChannelDir; got != "" {
		t.Errorf("GHChannelDir = %q for a run with no channel, want empty", got)
	}
}

// TestStartLocalSandbox_RequiresARunTreeKey pins that the plan refuses to be
// built without the key that names the tree it is supposed to bind — a Spec
// with an empty RunRoot would mask the temp dir and bind nothing back, which
// is a run with no workspace at all.
func TestStartLocalSandbox_RequiresARunTreeKey(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	runmode.SetLocalSandboxForTest(t, true)
	paths.SetForTest(t, t.TempDir())

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStores(db.Stores{})

	if _, err := s.startLocalSandbox("conv-9", "", nil, agenthost.ConversationInfo{ConversationID: "conv-9"}); err == nil {
		t.Error("startLocalSandbox accepted an empty run-tree key")
	}
}

// TestStartLocalSandbox_RefusesWithoutStores pins the fail-closed posture at
// the one place it could plausibly be waved through: with no stores there is
// no daemon, and with no daemon every exec verb inside the namespace fails.
// Failing here reports that as a wiring bug instead of as an agent that
// cannot use its own tools.
func TestStartLocalSandbox_RefusesWithoutStores(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	runmode.SetLocalSandboxForTest(t, true)
	paths.SetForTest(t, t.TempDir())

	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	_, err := s.startLocalSandbox("conv-10", "rk-10", nil, agenthost.ConversationInfo{ConversationID: "conv-10"})
	if err == nil {
		t.Fatal("startLocalSandbox succeeded with unwired stores")
	}
	if !strings.Contains(err.Error(), "local sandbox") {
		t.Errorf("error %q does not name the local sandbox", err)
	}
}

// TestAgentHostSocketPath_FitsTheSunPathLimit pins the constraint that
// decided where the local socket lives at all: a unix socket address is
// capped at 108 bytes, and the state root is deep-ish under a home directory.
func TestAgentHostSocketPath_FitsTheSunPathLimit(t *testing.T) {
	paths.SetForTest(t, filepath.Join("/home/a-fairly-long-developer-name", ".triagefactory"))
	got := paths.AgentHostSocketPath("00000000-0000-0000-0000-0000000000aa")
	if len(got) >= 108 {
		t.Errorf("socket path %q is %d bytes; the sun_path limit is 108", got, len(got))
	}
}
