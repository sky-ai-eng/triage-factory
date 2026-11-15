package delegate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestComposeLaunchTurn_ResumedSessionOnARebuiltTreeOpensWithTheWorkspaceNote:
// a re-claim that resumes a surviving session into a tree rebuilt under it
// sends the workspace note first, ahead of the continuation note, and records
// it delivered. A resume onto the tree it left sends no such note.
func TestComposeLaunchTurn_ResumedSessionOnARebuiltTreeOpensWithTheWorkspaceNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov domain.WorkspaceProvenance
	}{
		{"restored from the snapshot", domain.WorkspaceProvenanceRehydrated},
		{"built from scratch", domain.WorkspaceProvenanceFresh},
		{"the tree it left", domain.WorkspaceProvenanceWarm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			seedConversation(t, database, "r-sdk-note", "", "/tmp/wt-sdk-note")
			claimID := markEngaged(t, database, "r-sdk-note")
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
			sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-note", claimID, "event", runmode.LocalDefaultUserID)
			ctx := context.Background()
			before := len(allRows(t, s, "r-sdk-note"))

			note := workspaceNote("/tmp/wt-sdk-note", tc.prov)
			resumed := agentproc.RunOptions{SessionID: "sess-survived"}
			if err := s.composeLaunchTurn(ctx, &resumed, sink, runmode.LocalDefaultOrgID, "r-sdk-note",
				runmode.LocalDefaultUserID, "", note, launchUpdate{}, false, openingTestMemories(), openingTestTaskContext); err != nil {
				t.Fatalf("the resuming launch: %v", err)
			}

			rows := allRows(t, s, "r-sdk-note")[before:]
			var noted []domain.Message
			for _, r := range rows {
				if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
					noted = append(noted, r)
				}
			}
			if !tc.prov.Rebuilt() {
				if note != "" || len(noted) != 0 {
					t.Errorf("a resume onto the tree it left wrote note %q and %d note rows, want none", note, len(noted))
				}
				if resumed.Message != domain.SessionContinuationNote {
					t.Errorf("RunOptions.Message = %q, want only the continuation note", resumed.Message)
				}
				return
			}
			if note == "" {
				t.Fatal("no workspace note for a rebuilt tree")
			}
			if want := note + "\n\n" + domain.SessionContinuationNote; resumed.Message != want {
				t.Errorf("RunOptions.Message = %q, want the workspace note then the continuation note %q", resumed.Message, want)
			}
			if len(noted) != 1 || noted[0].Content != note || noted[0].Role != "user" {
				t.Fatalf("workspace note rows = %+v, want one user row carrying the note", noted)
			}
			if got := pendingRows(t, s, "r-sdk-note"); len(got) != 0 {
				t.Errorf("undelivered rows = %d, want 0; a pending note is the next follow-up: %+v", len(got), got)
			}
		})
	}
}

// TestDispatch_SDKFollowUpAfterEvictionOpensWithTheWorkspaceNote: a chat
// whose tree was removed between turns comes back from the snapshot its last
// turn wrote, and the resume's turn opens with the note saying so, ahead of
// the person's message. The follow-up before the eviction, onto the tree the
// turn left, says nothing.
func TestDispatch_SDKFollowUpAfterEvictionOpensWithTheWorkspaceNote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	f := newInteractiveFixture(t, "1421-note")
	wireBlobStore(t, f.s)
	ctx := context.Background()
	const session = "sess-1421-note"

	first := f.endTurn(t, session, "the build is red because a test is flaky")
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	root := first.park.claudeCwd
	if root == "" {
		t.Fatal("the first turn never launched")
	}
	assertSnapshotPresent(t, f.s, f.taskID, true)
	// A follow-up resumes on the model the conversation started with, so the
	// row carries one.
	if _, err := f.database.Exec(`UPDATE conversations SET model = 'claude-sonnet-4-6' WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("stamp the model: %v", err)
	}

	noteRows := func() []domain.Message {
		t.Helper()
		var out []domain.Message
		for _, r := range allRows(t, f.s, f.conv.ID) {
			if r.Subtype == domain.MessageSubtypeInjectionExecutorChanged {
				out = append(out, r)
			}
		}
		return out
	}

	warm := f.endTurn(t, session, "it was the retry")
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "which test?"), time.Now())
	if warm.opts.Message != "which test?" {
		t.Errorf("the resume onto the tree the turn left sent %q, want only the person's message", warm.opts.Message)
	}
	if got := noteRows(); len(got) != 0 {
		t.Errorf("the resume onto the tree the turn left recorded %d workspace notes, want none", len(got))
	}

	// The eviction: the tree and the session file kept outside it go together,
	// once the snapshot holds both.
	worktree.RemoveClaudeProjectDir(root)
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the tree: %v", err)
	}

	cold := f.endTurn(t, session, "rerun it")
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "can you rerun it?"), time.Now())
	note := workspaceNote(root, domain.WorkspaceProvenanceRehydrated)
	if !strings.HasPrefix(cold.opts.Message, note) || !strings.HasSuffix(cold.opts.Message, "can you rerun it?") {
		t.Errorf("the resume onto the restored tree sent %q, want the workspace note and then the person's message", cold.opts.Message)
	}
	if got := noteRows(); len(got) != 1 || got[0].Content != note {
		t.Errorf("workspace note rows after the restore = %+v, want one carrying the note", got)
	}
}
