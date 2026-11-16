package delegate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestBlueprintWorktreeCleanup_RemovesTheSessionFileWhateverTheSnapshotDid: a
// blueprint's end removes the task's run tree and the SDK session file kept
// outside it, whatever the key's snapshot came to. The session's durable copy
// is its own blob, not the snapshot.
func TestBlueprintWorktreeCleanup_RemovesTheSessionFileWhateverTheSnapshotDid(t *testing.T) {
	for i, tc := range []struct {
		name  string
		state string // the key's lifecycle row; "" records none
		blob  bool
	}{
		{"the latest persist is stored", domain.WorkspaceSnapshotWritten, true},
		{"the latest persist failed over an earlier step's blob", domain.WorkspaceSnapshotFailed, true},
		{"the row says written but the blob is gone", domain.WorkspaceSnapshotWritten, false},
		{"no persist was recorded", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunNamespace(t)
			setupGitTestEnv(t)
			s, _, _, taskID := setupAdvanceFixture(t, fmt.Sprintf("bp-cleanup-%d", i))
			wireBlobStore(t, s)
			wsKey := workspaceKey(taskID)

			root, err := worktree.MakeRunRoot(wsKey)
			if err != nil {
				t.Fatalf("MakeRunRoot: %v", err)
			}
			session, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(root), "sess-bp-cleanup")
			if err != nil {
				t.Fatalf("ClaudeSessionPath: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(session), 0o700); err != nil {
				t.Fatalf("mkdir session dir: %v", err)
			}
			if err := os.WriteFile(session, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
				t.Fatalf("write session: %v", err)
			}
			if tc.state != "" {
				seedSnapshotState(t, s, wsKey, evictWriterClaim, tc.state)
			}
			if tc.blob {
				putTestSnapshot(t, s, wsKey)
			}

			s.runBlueprintWorktreeCleanup(runmode.LocalDefaultOrgID, "bpr-cleanup", wsKey)

			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("run root after the blueprint's end: stat err = %v, want it removed", err)
			}
			if _, err := os.Stat(session); !os.IsNotExist(err) {
				t.Errorf("session file after the blueprint's end: stat err = %v, want it removed", err)
			}
		})
	}
}
