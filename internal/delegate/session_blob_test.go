package delegate

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// endTurnWithSession is endTurn with the session file the turn leaves holding
// body. atLaunch is the session file --resume would read when the runtime
// started, "" when there was none.
func (f *interactiveFixture) endTurnWithSession(t *testing.T, sessionID, reply, body string) (spec *liveRunSpec, atLaunch *string) {
	t.Helper()
	spec = f.endTurn(t, sessionID, reply)
	inner := f.s.liveRunOverride
	atLaunch = new(string)
	f.s.liveRunOverride = func(ctx context.Context, launched liveRunSpec) liveOutcome {
		if data, ok := worktree.ReadClaudeSessionTranscript(launched.park.claudeCwd, sessionID); ok {
			*atLaunch = string(data)
		}
		out := inner(ctx, launched)
		writeSession(t, launched.park.claudeCwd, sessionID, body)
		return out
	}
	return spec, atLaunch
}

// stampModel gives the conversation the model a follow-up resumes on.
func (f *interactiveFixture) stampModel(t *testing.T) {
	t.Helper()
	if _, err := f.database.Exec(`UPDATE conversations SET model = 'claude-sonnet-4-6' WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("stamp the model: %v", err)
	}
}

func readSessionBlob(t *testing.T, s *Spawner, conversationID string) string {
	t.Helper()
	rc, err := s.Storage().Get(context.Background(), sessionBlobKey(runmode.LocalDefaultOrgID, conversationID))
	if err != nil {
		t.Fatalf("get the session blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func putSessionBlob(t *testing.T, s *Spawner, conversationID, body string) {
	t.Helper()
	if err := s.Storage().Put(context.Background(), sessionBlobKey(runmode.LocalDefaultOrgID, conversationID), strings.NewReader(body)); err != nil {
		t.Fatalf("put the session blob: %v", err)
	}
}

// TestDispatch_SDKChatWithNoTreeAndNoSnapshotResumesOntoAFreshTree: a chat
// whose task has lost its tree, the session file beside it, and its snapshot
// is still one a person can send to, because its session has a blob of its
// own. The follow-up builds the tree from nothing, puts the session back
// where --resume reads it, and opens with the note saying the tree was built
// from scratch. Without the session blob the send is refused as expired.
func TestDispatch_SDKChatWithNoTreeAndNoSnapshotResumesOntoAFreshTree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	f := newInteractiveFixture(t, "1438-fresh")
	wireBlobStore(t, f.s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const session = "sess-1438-fresh"

	first, _ := f.endTurnWithSession(t, session, "the build is red because a test is flaky", `{"turn":1}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	root := first.park.claudeCwd
	if root == "" {
		t.Fatal("the first turn never launched")
	}
	if got := readSessionBlob(t, f.s, f.conv.ID); got != `{"turn":1}` {
		t.Fatalf("session blob after the first turn = %q, want the session file the turn left", got)
	}
	f.stampModel(t)

	worktree.RemoveClaudeProjectDir(root)
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the tree: %v", err)
	}
	f.s.discardWorkspaceSnapshot(ctx, org, workspaceKey(f.taskID))

	conv, err := f.stores.Conversations.GetSystem(ctx, org, f.conv.ID)
	if err != nil || conv == nil {
		t.Fatalf("GetSystem = (%v, %v)", conv, err)
	}
	if err := f.s.Storage().Delete(ctx, sessionBlobKey(org, f.conv.ID)); err != nil {
		t.Fatal(err)
	}
	if ok, reason := f.s.ResumabilityFor(ctx, org, conv); ok || reason != ResumeBlockedWorkspaceExpired {
		t.Errorf("with no tree, no snapshot and no session blob: resumable = (%v, %q), want refused as %q", ok, reason, ResumeBlockedWorkspaceExpired)
	}
	putSessionBlob(t, f.s, f.conv.ID, `{"turn":1}`)
	if ok, reason := f.s.ResumabilityFor(ctx, org, conv); !ok {
		t.Fatalf("with the session blob: refused as %q, want the follow-up accepted", reason)
	}

	cold, atLaunch := f.endTurnWithSession(t, session, "rerun it", `{"turn":2}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "can you rerun it?"), time.Now())

	if cold.opts.SessionID != session {
		t.Fatalf("the follow-up launched session %q, want %q resumed", cold.opts.SessionID, session)
	}
	if *atLaunch != `{"turn":1}` {
		t.Errorf("session file at launch = %q, want the one its blob holds", *atLaunch)
	}
	if cold.park.claudeCwd != root {
		t.Errorf("the follow-up ran in %q, want a fresh tree at the task's root %q", cold.park.claudeCwd, root)
	}
	note := workspaceNote(root, domain.WorkspaceProvenanceFresh)
	if !strings.HasPrefix(cold.opts.Message, note) || !strings.HasSuffix(cold.opts.Message, "can you rerun it?") {
		t.Errorf("the follow-up sent %q, want the built-from-scratch note and then the person's message", cold.opts.Message)
	}
	if got := readSessionBlob(t, f.s, f.conv.ID); got != `{"turn":2}` {
		t.Errorf("session blob after the follow-up = %q, want the session file its turn left", got)
	}
}

// TestDispatch_SDKFollowUpAfterATreeEndWhoseSnapshotFailedResumesItsSession:
// a turn whose snapshot failed still saves its session, so the tree's end
// removes the session file with the tree and leaves nothing behind, and the
// next follow-up rehydrates the earlier snapshot and resumes the session as
// that failed turn left it.
func TestDispatch_SDKFollowUpAfterATreeEndWhoseSnapshotFailedResumesItsSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	f := newInteractiveFixture(t, "1438-failed")
	wireBlobStore(t, f.s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	wsKey := workspaceKey(f.taskID)
	const session = "sess-1438-failed"

	first, _ := f.endTurnWithSession(t, session, "the build is red because a test is flaky", `{"turn":1}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	root := first.park.claudeCwd
	if root == "" {
		t.Fatal("the first turn never launched")
	}
	assertSnapshotPresent(t, f.s, f.taskID, true)
	f.stampModel(t)

	// The next turn's snapshot fails: the tree reads as one a jail that could
	// not be stopped may still be writing, so it is not captured.
	jailRunning := true
	f.s.treeNotQuiet(root, func() bool { return !jailRunning })
	f.endTurnWithSession(t, session, "it was the retry", `{"turn":2}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "which test?"), time.Now())
	jailRunning = false
	if st, err := f.s.snapshotStateFor(ctx, org, wsKey); err != nil || st == nil || st.State != domain.WorkspaceSnapshotFailed {
		t.Fatalf("snapshot state after the second turn = (%+v, %v), want failed", st, err)
	}

	f.s.runBlueprintWorktreeCleanup(org, "bpr-1438", wsKey)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("run root after the tree's end: stat err = %v, want it removed", err)
	}
	if sessionTranscriptExists(root, session) {
		t.Error("the tree's end left the session file behind")
	}

	cold, atLaunch := f.endTurnWithSession(t, session, "rerun it", `{"turn":3}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "can you rerun it?"), time.Now())
	if cold.opts.SessionID != session {
		t.Fatalf("the follow-up launched session %q, want %q resumed", cold.opts.SessionID, session)
	}
	if *atLaunch != `{"turn":2}` {
		t.Errorf("session file at launch = %q, want the one the failed turn saved", *atLaunch)
	}
	note := workspaceNote(root, domain.WorkspaceProvenanceRehydrated)
	if !strings.HasPrefix(cold.opts.Message, note) {
		t.Errorf("the follow-up sent %q, want it to open with the restored-from-snapshot note", cold.opts.Message)
	}
}

// TestRunAgent_ResumesASessionFromItsBlob: a launch that would resume a
// session whose file is gone from beside the tree puts it back from the
// session's blob and resumes it, rather than opening a fresh session.
func TestRunAgent_ResumesASessionFromItsBlob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	f := newInteractiveFixture(t, "1438-launch")
	wireBlobStore(t, f.s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const session = "sess-1438-launch"

	first, _ := f.endTurnWithSession(t, session, "the build is red because a test is flaky", `{"turn":1}`)
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	root := first.park.claudeCwd
	if root == "" {
		t.Fatal("the first turn never launched")
	}
	f.stampModel(t)
	worktree.RemoveClaudeProjectDir(root)

	// The claim of a turn already taken off the queue launches the runtime on
	// the session rather than delivering a message.
	if err := f.s.SendMessage(ctx, org, f.conv.ID, runmode.LocalDefaultUserID, "which test?", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, _, ok, err := f.stores.ConversationPendingInput.Consume(ctx, org, f.conv.ID); err != nil || !ok {
		t.Fatalf("take the message off the queue = (%v, %v)", ok, err)
	}
	claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil || claimed.ID != f.conv.ID {
		t.Fatalf("ClaimNextConversation = (%v, %v), want the conversation", claimed, err)
	}
	task, err := f.stores.Tasks.GetSystem(ctx, org, f.taskID)
	if err != nil || task == nil {
		t.Fatalf("GetSystem task: (%v, %v)", task, err)
	}

	resumed, atLaunch := f.endTurnWithSession(t, session, "it was the retry", `{"turn":2}`)
	f.s.runAgent(ctx, f.conv.ID, *task, "", runConfig{
		orgID:       org,
		teamID:      runmode.LocalDefaultTeamID,
		claimID:     claimed.ClaimID,
		runRoot:     root,
		surface:     agentprompt.SurfaceWorkbench,
		interactive: true,
	}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID, session)

	if resumed.opts.SessionID != session {
		t.Fatalf("the launch ran session %q, want %q resumed", resumed.opts.SessionID, session)
	}
	if *atLaunch != `{"turn":1}` {
		t.Errorf("session file at launch = %q, want the one its blob holds", *atLaunch)
	}
}

// TestLeaveConversation_SavesTheSessionBeforeTheReleaseWhateverTheSnapshotDoes:
// an SDK ending saves its session before it lets go of the claim, so no
// engagement after it can be running yet, and saves it whether or not the
// snapshot after the release lands.
func TestLeaveConversation_SavesTheSessionBeforeTheReleaseWhateverTheSnapshotDoes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	wireBlobStore(t, s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	cwd := t.TempDir()
	writeSession(t, cwd, "sess-leave", `{"left":true}`)
	s.treeNotQuiet(cwd, func() bool { return false })
	park := liveParkContext{
		orgID: org, conversationID: "conv-leave", namespace: "task-leave", claudeCwd: cwd,
		claimID: "claim-leave", runtime: domain.ConversationRuntimeSDK,
	}
	savedAtRelease := false
	s.leaveConversation(ctx, park, "sess-leave", snapshotReasonPark, func(ctx context.Context, _ liveParkContext) bool {
		savedAtRelease, _ = s.sessionSaved(ctx, org, "conv-leave")
		return false
	})

	if !savedAtRelease {
		t.Error("the session blob was not there when the claim was released")
	}
	if got := readSessionBlob(t, s, "conv-leave"); got != `{"left":true}` {
		t.Errorf("session blob = %q, want the session file the ending left", got)
	}
	assertSnapshotPresent(t, s, "task-leave", false)
}

// TestReapEndedSessions_DeletesTheBlobsOfEndedAndPurgedConversations: the
// sweep keeps a session a conversation can still resume, and deletes the
// session blob of a conversation that has ended or whose row is gone. A key
// of another shape under the prefix is not a session's and is left alone.
func TestReapEndedSessions_DeletesTheBlobsOfEndedAndPurgedConversations(t *testing.T) {
	f := newInteractiveFixture(t, "1438-reap")
	wireBlobStore(t, f.s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const purged = "eeeeeeee-1438-4438-8438-eeeeeeeeeeee"
	const foreign = sessionBlobPrefix + "not-a-session"

	putSessionBlob(t, f.s, f.conv.ID, `{"live":true}`)
	putSessionBlob(t, f.s, purged, `{"purged":true}`)
	if err := f.s.Storage().Put(ctx, foreign, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	exists := func(key string) bool {
		t.Helper()
		ok, err := f.s.Storage().Exists(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	f.s.ReapEndedSessions(ctx)
	if !exists(sessionBlobKey(org, f.conv.ID)) {
		t.Error("the sweep deleted the session of a conversation that can still be resumed")
	}
	if exists(sessionBlobKey(org, purged)) {
		t.Error("the sweep kept the session of a conversation whose row is gone")
	}
	if !exists(foreign) {
		t.Error("the sweep deleted a key that names no session")
	}

	if ended, err := f.stores.Conversations.EndConversationSystem(ctx, org, f.conv.ID, domain.EndedTakenOver); err != nil || ended == nil {
		t.Fatalf("EndConversationSystem = (%v, %v)", ended, err)
	}
	// Through the reaper the app starts, whose first pass runs at once.
	reaperCtx, stopReaper := context.WithCancel(ctx)
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		f.s.RunSnapshotReaper(reaperCtx, time.Hour)
	}()
	t.Cleanup(func() {
		stopReaper()
		<-reaped
	})
	deadline := time.Now().Add(5 * time.Second)
	for exists(sessionBlobKey(org, f.conv.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("the reaper kept the session of a conversation that has ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEvictIdleWorkspaces_KeepsATreeWhoseResumableSessionHasNoBlob: the
// project directory an eviction removes with the tree holds the task's SDK
// sessions, so a tree holding the session of a conversation that can still
// resume it stays until that session has a blob, and goes once it does.
func TestEvictIdleWorkspaces_KeepsATreeWhoseResumableSessionHasNoBlob(t *testing.T) {
	isolateRunNamespace(t)
	t.Setenv("HOME", t.TempDir())
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-session")
	wireBlobStore(t, s)
	ctx := context.Background()
	_, wtPath := seedEvictableTree(t, s, database, conversationID)
	writeSession(t, wtPath, "sess-evict-session", `{}`)

	s.EvictIdleWorkspaces(ctx, time.Hour)
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("the sweep removed a tree holding the only copy of a resumable session (stat err %v)", err)
	}
	if !sessionTranscriptExists(wtPath, "sess-evict-session") {
		t.Fatal("the sweep removed the only copy of a resumable session")
	}

	putSessionBlob(t, s, conversationID, `{}`)
	s.EvictIdleWorkspaces(ctx, time.Hour)
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("tree after the session was saved: stat err = %v, want it evicted", err)
	}
	if sessionTranscriptExists(wtPath, "sess-evict-session") {
		t.Error("the eviction left the session file behind")
	}
}

// TestRestoreSession puts a session back from its blob only where no file is:
// a file that is there is the copy the blob was saved from or newer.
func TestRestoreSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	wireBlobStore(t, s)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	cwd := t.TempDir()

	if err := s.restoreSession(ctx, org, "conv-restore", cwd, "sess-restore"); err != nil {
		t.Fatalf("with no blob: %v", err)
	}
	if sessionTranscriptExists(cwd, "sess-restore") {
		t.Fatal("a session file appeared with no blob to restore it from")
	}

	putSessionBlob(t, s, "conv-restore", `{"saved":true}`)
	if err := s.restoreSession(ctx, org, "conv-restore", cwd, "sess-restore"); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	if data, ok := worktree.ReadClaudeSessionTranscript(cwd, "sess-restore"); !ok || string(data) != `{"saved":true}` {
		t.Errorf("restored session file = %q (present %v), want the blob's", data, ok)
	}

	writeSession(t, cwd, "sess-restore", `{"newer":true}`)
	if err := s.restoreSession(ctx, org, "conv-restore", cwd, "sess-restore"); err != nil {
		t.Fatalf("restoreSession over a file: %v", err)
	}
	if data, _ := worktree.ReadClaudeSessionTranscript(cwd, "sess-restore"); string(data) != `{"newer":true}` {
		t.Errorf("session file = %q, want the one that was there", data)
	}
}

func TestSessionBlobOwner(t *testing.T) {
	org, conv, ok := sessionBlobOwner(sessionBlobKey("org-1", "conv-1"))
	if !ok || org != "org-1" || conv != "conv-1" {
		t.Errorf("sessionBlobOwner(sessionBlobKey) = (%q, %q, %v), want the org and conversation back", org, conv, ok)
	}
	for _, key := range []string{
		"org-1/task-1/workspace.tar",
		sessionBlobPrefix + "conv-1.jsonl",
		sessionBlobPrefix + "org-1/conv-1",
		sessionBlobPrefix + "org-1/a/conv-1.jsonl",
		sessionBlobPrefix + "org-1/" + sessionBlobLeafSuffix,
	} {
		if _, _, ok := sessionBlobOwner(key); ok {
			t.Errorf("sessionBlobOwner(%q) recognized a key of another shape", key)
		}
	}
}
