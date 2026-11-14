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

// TestBlueprintWorktreeCleanup_KeepsASessionTheSnapshotDoesNotHold: a
// blueprint's end removes the task's run tree whatever the snapshot says, and
// the SDK session file kept outside it goes too only when the store holds the
// key's latest persist. Any other answer leaves the file, since it may be the
// only copy of the conversation a follow-up resumes.
func TestBlueprintWorktreeCleanup_KeepsASessionTheSnapshotDoesNotHold(t *testing.T) {
	for i, tc := range []struct {
		name     string
		state    string // the key's lifecycle row; "" records none
		blob     bool
		wantKept bool
	}{
		{"the latest persist is stored", domain.WorkspaceSnapshotWritten, true, false},
		{"the latest persist failed over an earlier step's blob", domain.WorkspaceSnapshotFailed, true, true},
		{"the row says written but the blob is gone", domain.WorkspaceSnapshotWritten, false, true},
		{"no persist was recorded", "", false, true},
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
			_, err = os.Stat(session)
			if kept := err == nil; kept != tc.wantKept {
				t.Errorf("session file kept = %v (stat err %v), want %v", kept, err, tc.wantKept)
			}
		})
	}
}
