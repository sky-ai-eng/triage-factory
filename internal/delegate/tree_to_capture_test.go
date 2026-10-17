package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// recordingHandOff stands in for the run network an ending hands its tree to.
type recordingHandOff struct {
	roots []string
	err   error
}

func (r *recordingHandOff) HandOffRunTree(_ context.Context, root string) error {
	r.roots = append(r.roots, root)
	return r.err
}

// TestTreeToCapture pins when a jailed ending captures a tree its own jail
// never ran in: only when the key's last snapshot did not land, and then only
// through its own jail identity. A tree an engagement's jail holds is captured
// as it stands.
func TestTreeToCapture(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "tree-to-capture")
	runmode.SetForTest(t, runmode.ModeMulti)
	if !agentproc.WillSandbox() {
		t.Skip("the jailed capture exists only on Linux")
	}
	wsKey := taskIDForConversation(t, database, conversationID)

	for _, tc := range []struct {
		name        string
		state       string // "" leaves the key with no record
		noNetwork   bool
		missingTree bool
		wantCapture bool
		wantAdopt   bool
	}{
		{name: "last snapshot written", state: domain.WorkspaceSnapshotWritten, wantCapture: false},
		{name: "last snapshot failed", state: domain.WorkspaceSnapshotFailed, wantCapture: true, wantAdopt: true},
		{name: "last snapshot still pending", state: domain.WorkspaceSnapshotPending, wantCapture: true, wantAdopt: true},
		{name: "never snapshotted", wantCapture: true, wantAdopt: true},
		{name: "failed, with no network to adopt through", state: domain.WorkspaceSnapshotFailed, noNetwork: true, wantCapture: false},
		{name: "no tree at all", state: domain.WorkspaceSnapshotFailed, missingTree: true, wantCapture: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.workspaceSnapshots.DeleteSnapshotStateSystem(context.Background(), runmode.LocalDefaultOrgID, wsKey); err != nil {
				t.Fatal(err)
			}
			if tc.state != "" {
				seedSnapshotState(t, s, wsKey, "claim-before", tc.state)
			}
			tree := filepath.Join(t.TempDir(), "gone")
			if !tc.missingTree {
				tree = makeRunTree(t, wsKey)
			}
			net := &recordingHandOff{}
			park := liveParkContext{orgID: runmode.LocalDefaultOrgID, conversationID: conversationID, namespace: wsKey, claudeCwd: tree, cell: net}
			if tc.noNetwork {
				park.cell = nil
			}

			capture, adopt := s.treeToCapture(context.Background(), park)
			if capture != tc.wantCapture {
				t.Fatalf("capture = %v, want %v", capture, tc.wantCapture)
			}
			if (adopt != nil) != tc.wantAdopt {
				t.Fatalf("adopt set = %v, want %v", adopt != nil, tc.wantAdopt)
			}
			if adopt == nil {
				return
			}
			if len(net.roots) != 0 {
				t.Fatal("the tree was handed off before the capture asked for it")
			}
			if err := adopt(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(net.roots) != 1 || net.roots[0] != tree {
				t.Errorf("handed off %v, want just %s", net.roots, tree)
			}
		})
	}

	t.Run("held by a jail", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to give the tree to a jail identity")
		}
		seedSnapshotState(t, s, wsKey, "claim-held", domain.WorkspaceSnapshotWritten)
		tree := makeRunTree(t, wsKey)
		if err := os.Chown(tree, sandbox.JailUID(3), -1); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chown(tree, os.Getuid(), -1) })
		capture, adopt := s.treeToCapture(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrgID, namespace: wsKey, claudeCwd: tree, cell: &recordingHandOff{}})
		if !capture || adopt != nil {
			t.Errorf("a tree an engagement's jail holds: capture = %v, adopt set = %v; want it captured as it stands", capture, adopt != nil)
		}
	})
}

// TestPersistWorkspaceSnapshot_AdoptsBeforeCapturing pins where an ending's
// hand-off runs: inside the persist, after its record is opened, so a hand-off
// that fails closes the record failed like any other capture failure, and one
// that succeeds is followed by the capture it was for.
func TestPersistWorkspaceSnapshot_AdoptsBeforeCapturing(t *testing.T) {
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "adopt-before-capture")
	wireBlobStore(t, s)
	ctx := context.Background()
	wsKey := taskIDForConversation(t, database, conversationID)
	tree := makeRunTree(t, wsKey)

	refused := errors.New("hand-off refused")
	err := s.persistWorkspaceSnapshot(ctx, snapshotWrite{
		orgID: runmode.LocalDefaultOrgID, conversationID: conversationID, keyID: wsKey, claimID: "claim-adopt",
		wtPath: tree, reason: snapshotReasonShutdown,
		adoptTree: func(context.Context) error { return refused },
	}, false)
	if !errors.Is(err, refused) {
		t.Fatalf("persist with a refused hand-off = %v, want the refusal", err)
	}
	if st, _ := s.snapshotStateFor(ctx, runmode.LocalDefaultOrgID, wsKey); st == nil || st.State != domain.WorkspaceSnapshotFailed {
		t.Errorf("record after a refused hand-off = %+v, want failed", st)
	}

	adopted := false
	if err := s.persistWorkspaceSnapshot(ctx, snapshotWrite{
		orgID: runmode.LocalDefaultOrgID, conversationID: conversationID, keyID: wsKey, claimID: "claim-adopt",
		wtPath: tree, reason: snapshotReasonShutdown,
		adoptTree: func(context.Context) error { adopted = true; return nil },
	}, false); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if !adopted {
		t.Error("the persist captured without handing the tree off first")
	}
	if st, _ := s.snapshotStateFor(ctx, runmode.LocalDefaultOrgID, wsKey); st == nil || st.State != domain.WorkspaceSnapshotWritten {
		t.Errorf("record after the adopted capture = %+v, want written", st)
	}
}
