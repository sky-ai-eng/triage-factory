package delegate

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// interactiveFixture is a Jira task a person holds, and the conversation their
// claim minted on it: an interactive SDK conversation with no blueprint run and
// no session. A Jira task's workspace is a bare run root, so the dispatch
// builds it from nothing without touching the network.
type interactiveFixture struct {
	s        *Spawner
	database *sql.DB
	stores   db.Stores
	taskID   string
	conv     domain.Conversation
}

func newInteractiveFixture(t *testing.T, suffix string) *interactiveFixture {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)

	key := "SKY-" + suffix
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreateJiraSystem(ctx, org, "https://jira.example.com", key,
		dbseed.JiraIssueID(key), dbseed.JiraIssueProject(key), "T-"+suffix, "https://x/"+suffix)
	if err != nil {
		t.Fatalf("create jira entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: domain.EventJiraIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), org, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := database.Exec(`UPDATE tasks SET claimed_by_user_id = ?, status = 'in_progress' WHERE id = ?`, runmode.LocalDefaultUserID, task.ID); err != nil {
		t.Fatalf("claim the task for the person: %v", err)
	}
	minted, err := stores.Conversations.MintForPersonClaimSystem(ctx, org, task.ID, runmode.LocalDefaultUserID)
	if err != nil || minted == nil {
		t.Fatalf("MintForPersonClaimSystem = (%v, %v)", minted, err)
	}
	return &interactiveFixture{
		s:        NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6"),
		database: database,
		stores:   stores,
		taskID:   task.ID,
		conv:     *minted,
	}
}

// claim sends the person's message, which wakes the conversation, and claims
// it the way the dispatcher's scan does.
func (f *interactiveFixture) claim(t *testing.T, message string) *domain.Conversation {
	t.Helper()
	ctx := context.Background()
	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, f.conv.ID, runmode.LocalDefaultUserID, message, domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextConversation = (%v, %v)", claimed, err)
	}
	if claimed.ID != f.conv.ID {
		t.Fatalf("claimed %s, want the person's conversation %s", claimed.ID, f.conv.ID)
	}
	claimed.OrgID = runmode.LocalDefaultOrgID
	return claimed
}

// errLaunchRecorded is what the recording runtime answers with once it has
// read what the launch handed it, so the dispatch ends without a process.
var errLaunchRecorded = errors.New("the test's runtime recorded the launch and did not start")

// recordLaunch makes the SDK process start record the spec the driver would
// run and refuse to start.
func (f *interactiveFixture) recordLaunch() *liveRunSpec {
	got := &liveRunSpec{}
	f.s.liveRunOverride = func(_ context.Context, spec liveRunSpec) liveOutcome {
		*got = spec
		return liveOutcome{err: errLaunchRecorded}
	}
	return got
}

// dispatch claims the person's message and drives it through the real
// dispatcher, returning what the launch handed the SDK runtime.
func (f *interactiveFixture) dispatch(t *testing.T, message string) *liveRunSpec {
	t.Helper()
	launched := f.recordLaunch()
	claimed := f.claim(t, message)
	if claimed.BlueprintRunID != "" {
		t.Fatalf("claimed conversation has blueprint run %q, want none", claimed.BlueprintRunID)
	}
	f.s.dispatchClaimedConversation(context.Background(), claimed, time.Now())
	if launched.opts.Cwd == "" {
		t.Fatal("the runtime was never started; the dispatch stopped before the launch")
	}
	return launched
}

// TestDispatch_InteractiveConversationRunsWithNoBlueprintRun drives a person's
// message on the conversation their claim minted through the real dispatcher,
// on the SDK runtime this harness reaches. The claim is not failed as
// blueprint_missing: it builds the task's workspace and launches under the
// workbench prompt, with no mission and no stop_blueprint. No reactor runs:
// there is no blueprint run for one to read, and the dispatch returns past the
// runtime without asking.
func TestDispatch_InteractiveConversationRunsWithNoBlueprintRun(t *testing.T) {
	f := newInteractiveFixture(t, "1421")
	launched := f.dispatch(t, "why is the build red?")

	selfBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	framework := resolveCLIPath(strings.TrimSpace(agentprompt.Build(sdkSpec(agentprompt.SurfaceWorkbench))), agentproc.AgentVisibleBinary(selfBin))
	if !strings.HasPrefix(launched.opts.SystemPrompt, framework) {
		t.Error("the launch's system prompt does not open with the workbench composition")
	}
	if launched.hasBlueprint {
		t.Error("the launch reports a blueprint run for a conversation with none")
	}
	for _, tool := range launched.opts.TFTools {
		if tool.Name == tooldefs.StopBlueprintName {
			t.Errorf("the launch offers %s to a conversation with no blueprint run", tool.Name)
		}
	}

	// No mission: the conversation's own block holds the run's facts and its
	// verb reference and nothing after them.
	block, err := f.stores.Conversations.SystemBlockSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatalf("SystemBlockSystem: %v", err)
	}
	if block == "" {
		t.Fatal("the launch stamped no system block")
	}
	if tail := strings.TrimSpace(block); !strings.HasSuffix(tail, "</tools>") && !strings.HasSuffix(tail, "</run_context>") {
		t.Errorf("the system block carries text after the run context and tools, which only a mission would put there:\n%s", block)
	}

	// The runtime refused to start, so the conversation failed on that, and
	// not because it has no blueprint run.
	var failure string
	for _, r := range allRows(t, f.s, f.conv.ID) {
		if r.Subtype == domain.MessageSubtypeStopNote {
			failure = r.Content
		}
	}
	if strings.Contains(failure, "blueprint_run") || !strings.Contains(failure, errLaunchRecorded.Error()) {
		t.Errorf("the conversation's failure = %q, want the runtime's refusal", failure)
	}
}

// TestDispatch_InteractiveSDKFirstTurnCarriesThePersonsMessage: an interactive
// SDK conversation with no session launches a fresh session rather than
// resuming one, and its first turn is the opening followed by the person's
// message, which is taken off the queue.
func TestDispatch_InteractiveSDKFirstTurnCarriesThePersonsMessage(t *testing.T) {
	f := newInteractiveFixture(t, "1421-first")
	launched := f.dispatch(t, "why is the build red?")

	if launched.opts.SessionID != "" {
		t.Errorf("the launch resumes session %q, want a fresh session", launched.opts.SessionID)
	}
	if launched.opts.Message != "" {
		t.Errorf("the launch sends %q as a resume message, want the first turn in its blocks", launched.opts.Message)
	}
	var opening []domain.Message
	for _, r := range allRows(t, f.s, f.conv.ID) {
		switch r.Subtype {
		case domain.MessageSubtypeInjectionMemory, domain.MessageSubtypeInjectionTaskContext:
			opening = append(opening, r)
		}
	}
	want, err := openingContentBlocks(opening)
	if err != nil {
		t.Fatalf("assemble the minted opening: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("the launch minted no opening")
	}
	want = append(want, agentproc.ContentBlock{Type: "text", Text: "why is the build red?"})
	if got := blockTexts(launched.opts.OpeningBlocks); strings.Join(got, "\x00") != strings.Join(blockTexts(want), "\x00") {
		t.Errorf("first turn =\n  %q\nwant the opening and then the message\n  %q", got, blockTexts(want))
	}
	if _, _, ok, err := f.s.pendingInput.Peek(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID); err != nil || ok {
		t.Errorf("the person's message is still pending after the launch: ok=%v err=%v", ok, err)
	}
}

// TestDispatch_InteractiveTurnIsNotNudgedForAnArtifact: the artifact-contract
// nudge sends a blueprint's final step that produced nothing back to work. A
// turn in a person's conversation that produced nothing is ordinary, so the
// launch arms no nudge, though the conversation, with no artifact and no
// deliverable, is one an armed check would send back.
func TestDispatch_InteractiveTurnIsNotNudgedForAnArtifact(t *testing.T) {
	f := newInteractiveFixture(t, "1421-nudge")
	launched := f.dispatch(t, "what does this ticket want?")

	if launched.nudge != nil {
		t.Errorf("the launch arms a turn-end nudge: %q", launched.nudge(context.Background()))
	}
	task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.taskID)
	if err != nil || task == nil {
		t.Fatalf("load the task: (%v, %v)", task, err)
	}
	armed := f.s.artifactContractNudge(runmode.LocalDefaultOrgID, f.conv.ID, *task, true)
	if armed == nil || armed(context.Background(), 1, "") != artifactNudgeNote {
		t.Fatal("an armed check does not nudge this conversation; the test cannot tell the launch's gate from a conversation with output")
	}
}

// TestFailedInteractiveConversationIsReplaced: a conversation a person works
// in that fails, whether its runtime never started or its turn ended in an
// error, is ended, and the task they still hold gets a new interactive
// conversation as its current one, created for that person, while the failure
// stays on the old row. A task the person no longer holds gets none.
func TestFailedInteractiveConversationIsReplaced(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unclaim     bool
		errorResult bool
		wantReplace bool
	}{
		{name: "the person still holds the task", wantReplace: true},
		{name: "the turn ends in an error result", errorResult: true, wantReplace: true},
		{name: "nobody holds the task", unclaim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInteractiveFixture(t, "1421-replace")
			ctx := context.Background()
			if tc.unclaim {
				if _, err := f.database.Exec(`UPDATE tasks SET claimed_by_user_id = NULL, status = 'queued' WHERE id = ?`, f.taskID); err != nil {
					t.Fatalf("hand the task back: %v", err)
				}
			}
			if tc.errorResult {
				f.s.liveRunOverride = func(_ context.Context, spec liveRunSpec) liveOutcome {
					return liveOutcome{result: &agentproc.Result{IsError: true, Subtype: "error_during_execution", Result: "the model call failed"}, sessionID: "sess-1421-error"}
				}
				f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
			} else {
				// The runtime refuses to start, which fails the conversation.
				f.dispatch(t, "why is the build red?")
			}

			failed, err := f.stores.Conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, f.conv.ID)
			if err != nil || failed == nil {
				t.Fatalf("GetSystem: (%v, %v)", failed, err)
			}
			if failed.Status != domain.StatusFailed || failed.EndedReason != domain.EndedFailed {
				t.Fatalf("the failed conversation = (status %q, ended %q), want (failed, %q)", failed.Status, failed.EndedReason, domain.EndedFailed)
			}

			task, err := f.stores.Tasks.GetSystem(ctx, runmode.LocalDefaultOrgID, f.taskID)
			if err != nil || task == nil {
				t.Fatalf("load the task: (%v, %v)", task, err)
			}
			if !tc.wantReplace {
				if task.ConversationID != "" {
					t.Errorf("the task's current conversation = %q, want none on a task nobody holds", task.ConversationID)
				}
				return
			}
			if task.ConversationID == "" || task.ConversationID == f.conv.ID {
				t.Fatalf("the task's current conversation = %q, want a new one in place of %s", task.ConversationID, f.conv.ID)
			}
			replacement, err := f.stores.Conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, task.ConversationID)
			if err != nil || replacement == nil {
				t.Fatalf("load the replacement: (%v, %v)", replacement, err)
			}
			if replacement.Type != domain.ConversationTypeInteractive || replacement.CreatorUserID != runmode.LocalDefaultUserID {
				t.Errorf("replacement = (type %q, creator %q), want an interactive conversation for %s",
					replacement.Type, replacement.CreatorUserID, runmode.LocalDefaultUserID)
			}
			if replacement.EndedAt != nil || replacement.Status != domain.StatusOpen {
				t.Errorf("replacement = (status %q, ended %v), want parked and un-ended", replacement.Status, replacement.EndedAt)
			}
		})
	}
}

// TestFailureBeforeTheModelSpokeOwesNoMemory: the failure record is TF's
// statement, not a turn the model took, so a conversation that fails before
// its model speaks has not run and owes its task no memory. The person's next
// message on the replacement is claimed at once rather than held for a memory
// of work nobody did.
func TestFailureBeforeTheModelSpokeOwesNoMemory(t *testing.T) {
	f := newInteractiveFixture(t, "1421-unrun")
	ctx := context.Background()
	// The runtime refuses to start, which fails the conversation.
	f.dispatch(t, "why is the build red?")

	failed, err := f.stores.Conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil || failed == nil {
		t.Fatalf("GetSystem: (%v, %v)", failed, err)
	}
	if failed.Status != domain.StatusFailed {
		t.Fatalf("the conversation's status = %q, want failed", failed.Status)
	}
	if failed.HasRun {
		t.Error("a conversation whose only rows are the person's message and TF's failure record reads as one that has run")
	}

	task, err := f.stores.Tasks.GetSystem(ctx, runmode.LocalDefaultOrgID, f.taskID)
	if err != nil || task == nil || task.ConversationID == "" || task.ConversationID == f.conv.ID {
		t.Fatalf("the task's replacement conversation = (%+v, %v), want a new one", task, err)
	}
	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, task.ConversationID, runmode.LocalDefaultUserID, "try again", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage to the replacement: %v", err)
	}
	claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	if claimed == nil || claimed.ID != task.ConversationID {
		t.Errorf("claimed %+v, want the replacement %s with no memory owed ahead of it", claimed, task.ConversationID)
	}
}

// endTurn makes the SDK process stand-in end one turn the way a model ending
// its reply does: the stream stamps its session, the reply lands through the
// launch's own sink, and the session file is on disk where --resume reads
// it. It records the spec it was handed.
func (f *interactiveFixture) endTurn(t *testing.T, sessionID, reply string) *liveRunSpec {
	t.Helper()
	got := &liveRunSpec{}
	f.s.liveRunOverride = func(_ context.Context, spec liveRunSpec) liveOutcome {
		*got = spec
		sink := spec.sink.(*conversationSink)
		if err := sink.OnSession(sessionID); err != nil {
			t.Errorf("stamp the session: %v", err)
		}
		if err := sink.OnMessage(&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: reply}); err != nil {
			t.Errorf("record the reply: %v", err)
		}
		path, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(spec.park.claudeCwd), sessionID)
		if err != nil {
			t.Fatalf("ClaudeSessionPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make the session dir: %v", err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write the session file: %v", err)
		}
		return liveOutcome{result: &agentproc.Result{StopReason: "end_turn", Result: reply}, sessionID: sessionID}
	}
	return got
}

// seedPerson adds a second person who can hold the task and send messages.
func (f *interactiveFixture) seedPerson(t *testing.T, name string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := f.database.Exec(`INSERT INTO users (id, display_name) VALUES (?, ?)`, id, name); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return id
}

// TestDispatch_InteractiveSDKTurnKeepsItsSession: a turn the model ends
// itself leaves the conversation open for the person's next message, which
// resumes this session, so the session file the next --resume reads survives
// the turn's end.
func TestDispatch_InteractiveSDKTurnKeepsItsSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newInteractiveFixture(t, "1421-keep")
	launched := f.endTurn(t, "sess-1421-keep", "the build is red because a test is flaky")
	claimed := f.claim(t, "why is the build red?")
	f.s.dispatchClaimedConversation(context.Background(), claimed, time.Now())

	if launched.opts.Cwd == "" {
		t.Fatal("the runtime was never started")
	}
	conv, err := f.stores.Conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: (%v, %v)", conv, err)
	}
	if conv.Status != domain.StatusOpen || conv.EndedAt != nil {
		t.Fatalf("after the turn: (status %q, ended %v), want open and un-ended", conv.Status, conv.EndedAt)
	}
	// With no verdict the row is one the restart sweep keeps the tree of
	// (ListResumableWorktreePathsSystem), so the session survives a restart
	// as well as the turn.
	if conv.Concluded() {
		t.Errorf("the turn recorded a verdict (outcome %q); a chat between turns waits on its next message", conv.Outcome)
	}
	if !sessionTranscriptExists(launched.park.claudeCwd, "sess-1421-keep") {
		t.Error("the turn's end removed the session file the next message resumes from")
	}
}

// TestDispatch_InteractiveSDKCutOffTurnResumesItsSession: the claim that took
// a person's message can end before the reply does (a crash, the app quitting
// mid-turn), leaving the conversation claimable with nothing waiting. Its
// next claim resumes the session that holds the message, with the
// continuation note, rather than parking with the question unanswered. With
// no session file left there is nothing to resume, and it parks.
func TestDispatch_InteractiveSDKCutOffTurnResumesItsSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		loseSession bool
	}{
		{name: "the session survived"},
		{name: "the session file is gone", loseSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			f := newInteractiveFixture(t, "1421-cut")
			ctx := context.Background()
			const session = "sess-1421-cut"
			first := f.endTurn(t, session, "the build is red because a test is flaky")
			f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
			root := first.park.claudeCwd
			// A follow-up resumes on the model the conversation started with,
			// so the row carries one.
			if _, err := f.database.Exec(`UPDATE conversations SET model = 'claude-sonnet-4-6' WHERE id = ?`, f.conv.ID); err != nil {
				t.Fatalf("stamp the model: %v", err)
			}

			// The follow-up is sent, and an engagement takes it off the queue
			// and ends before its reply does.
			if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, f.conv.ID, runmode.LocalDefaultUserID, "which test?", domain.TurnSettings{}); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if _, _, ok, err := f.stores.ConversationPendingInput.Consume(ctx, runmode.LocalDefaultOrgID, f.conv.ID); err != nil || !ok {
				t.Fatalf("take the message off the queue = (%v, %v)", ok, err)
			}
			if tc.loseSession {
				worktree.RemoveClaudeProjectDir(root)
			}

			resumed := f.endTurn(t, session, "it was the retry")
			claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
			if err != nil || claimed == nil || claimed.ID != f.conv.ID {
				t.Fatalf("ClaimNextConversation = (%v, %v), want the cut-off conversation", claimed, err)
			}
			claimed.OrgID = runmode.LocalDefaultOrgID
			f.s.dispatchClaimedConversation(ctx, claimed, time.Now())

			if tc.loseSession {
				if resumed.opts.Cwd != "" {
					t.Errorf("a cut-off turn with no session file launched the runtime (session %q); want a park", resumed.opts.SessionID)
				}
			} else {
				if resumed.opts.SessionID != session {
					t.Fatalf("the re-claim launched session %q, want the surviving %q resumed", resumed.opts.SessionID, session)
				}
				if !strings.HasSuffix(resumed.opts.Message, domain.SessionContinuationNote) {
					t.Errorf("the resumed turn sent %q, want it to end with the continuation note", resumed.opts.Message)
				}
			}
			conv, err := f.stores.Conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, f.conv.ID)
			if err != nil || conv == nil {
				t.Fatalf("GetSystem: (%v, %v)", conv, err)
			}
			if conv.Status != domain.StatusOpen || conv.EndedAt != nil {
				t.Errorf("after the re-claim: (status %q, ended %v), want open and un-ended", conv.Status, conv.EndedAt)
			}
			// Only the park says, on the transcript, that the reply did not
			// finish.
			wantNotes := 0
			if tc.loseSession {
				wantNotes = 1
				if conv.ParkReason != domain.ParkReasonTurnIncomplete {
					t.Errorf("park_reason = %q, want %q", conv.ParkReason, domain.ParkReasonTurnIncomplete)
				}
			}
			if n := noMessageParkNotes(t, f.database, f.conv.ID); n != wantNotes {
				t.Errorf("%d no-message stop notes on the transcript, want %d", n, wantNotes)
			}
		})
	}
}

// TestRunAgent_AResumedSessionRetainsItsOpeningRow: a re-claim whose session
// survived sends no opening, so it fetches no PR history, and the task-context
// file it retains is the conversation's own task-context row.
func TestRunAgent_AResumedSessionRetainsItsOpeningRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a re-claim that resumes its session fetched %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	f := newInteractiveFixture(t, "1439-resume")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const session = "sess-1439-resume"
	first := f.endTurn(t, session, "the build is red because a test is flaky")
	f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
	root := first.park.claudeCwd
	if root == "" {
		t.Fatal("the first turn never launched")
	}

	// Rewritten so that only a copy of the row can match it: a task context
	// composed now would render the same bytes the first launch did.
	const opening = "<task_context>\nJira issue SKY-1439-resume, as the opening rendered it\n</task_context>"
	if _, err := f.database.Exec(`UPDATE messages SET content = ? WHERE conversation_id = ? AND subtype = ?`,
		opening, f.conv.ID, domain.MessageSubtypeInjectionTaskContext); err != nil {
		t.Fatalf("rewrite the task-context row: %v", err)
	}
	file := filepath.Join(entityMemoryTarget(&runConfig{}, f.conv.ID, root), taskContextFileName)
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove the first launch's task-context file: %v", err)
	}

	// An engagement takes the follow-up off the queue and ends before its
	// reply does, so the next claim launches the runtime on the session. A
	// follow-up resumes on the model the conversation started with, so the row
	// carries one.
	if _, err := f.database.Exec(`UPDATE conversations SET model = 'claude-sonnet-4-6' WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("stamp the model: %v", err)
	}
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

	resumed := f.endTurn(t, session, "it was the retry")
	f.s.runAgent(ctx, f.conv.ID, *task, "", runConfig{
		orgID:       org,
		teamID:      runmode.LocalDefaultTeamID,
		claimID:     claimed.ClaimID,
		runRoot:     root,
		surface:     agentprompt.SurfaceWorkbench,
		interactive: true,
		prSkeleton:  prSkeletonSource{client: ghclient.NewClient(srv.URL, "t"), owner: "owner", repo: "repo", prNumber: 7},
	}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID, session)

	if resumed.opts.SessionID != session {
		t.Fatalf("the re-claim launched session %q, want %q resumed", resumed.opts.SessionID, session)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read the task-context file: %v", err)
	}
	// writeTaskContextFile ends the file with a newline.
	if string(got) != opening+"\n" {
		t.Errorf("task-context file = %q, want the task-context row's %q", got, opening)
	}
}

// TestDispatch_InteractiveSDKFirstTurnCarriesEverythingAsked: a message an
// earlier engagement took off the queue without any model answering it (a
// claim that paused before its agent ran, which settles the waiting input) is
// still part of what the person has asked, so the first turn carries it ahead
// of the message that woke the conversation.
func TestDispatch_InteractiveSDKFirstTurnCarriesEverythingAsked(t *testing.T) {
	f := newInteractiveFixture(t, "1421-asked")
	delivered := true
	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: f.conv.ID, UserID: runmode.LocalDefaultUserID, Role: "user",
		Content: "why is the build red?", Delivered: &delivered,
	}); err != nil {
		t.Fatalf("record the settled message: %v", err)
	}
	launched := f.dispatch(t, "try again")

	want := "why is the build red?" + db.PendingInputSeparator + "try again"
	blocks := launched.opts.OpeningBlocks
	if len(blocks) == 0 || blocks[len(blocks)-1].Text != want {
		t.Errorf("the first turn ends with %q, want %q", blockTexts(blocks), want)
	}
}

// TestDispatch_InteractiveSDKSessionThatNeverSpokeStartsFresh: a first turn
// can be stopped or handed back after its session started and before the
// model said anything. The send gate reads that conversation as one that has
// not run, so the dispatcher must too: it starts a fresh session carrying the
// person's message rather than resuming one that holds no turn.
func TestDispatch_InteractiveSDKSessionThatNeverSpokeStartsFresh(t *testing.T) {
	f := newInteractiveFixture(t, "1421-unspoken")
	if _, err := f.database.Exec(`UPDATE conversations SET sdk_session_id = 'sess-unspoken' WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("stamp a session: %v", err)
	}
	launched := f.dispatch(t, "why is the build red?")

	if launched.opts.SessionID != "" {
		t.Errorf("the launch resumes session %q, which took no turn", launched.opts.SessionID)
	}
	blocks := launched.opts.OpeningBlocks
	if len(blocks) == 0 || blocks[len(blocks)-1].Text != "why is the build red?" {
		t.Errorf("the first turn = %q, want it to end with the person's message", blockTexts(blocks))
	}
}

// TestDispatch_InteractiveTurnActsForThePersonWhoAsked: a reassignment within
// the team keeps the conversation, so its creator is no longer the person
// talking to it. A turn acts for the person whose message it answers, and the
// rows the turn writes are theirs, which is what the spend view charges.
func TestDispatch_InteractiveTurnActsForThePersonWhoAsked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newInteractiveFixture(t, "1421-asker")
	bob := f.seedPerson(t, "Bob")
	if _, err := f.database.Exec(`UPDATE tasks SET claimed_by_user_id = ? WHERE id = ?`, bob, f.taskID); err != nil {
		t.Fatalf("reassign the task: %v", err)
	}
	ctx := context.Background()
	launched := f.endTurn(t, "sess-1421-asker", "here is what I found")
	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, f.conv.ID, bob, "what changed?", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage as the new holder: %v", err)
	}
	claimed, err := f.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextConversation = (%v, %v)", claimed, err)
	}
	claimed.OrgID = runmode.LocalDefaultOrgID
	f.s.dispatchClaimedConversation(ctx, claimed, time.Now())

	sink, ok := launched.sink.(*conversationSink)
	if !ok || sink.creatorUserID != bob {
		t.Errorf("the launch acts for %q, want the person who asked (%s), not the conversation's creator", sink.creatorUserID, bob)
	}
	var replyUser string
	if err := f.database.QueryRow(
		`SELECT COALESCE(user_id, '') FROM messages WHERE conversation_id = ? AND role = 'assistant'`, f.conv.ID,
	).Scan(&replyUser); err != nil {
		t.Fatalf("read the reply: %v", err)
	}
	if replyUser != bob {
		t.Errorf("the reply is attributed to %q, want %s", replyUser, bob)
	}
}

// TestNativeTranscript_AttributesATurnToThePersonItAnswers: the native loop's
// own rows arrive unattributed. They belong to the person the engagement acts
// for until a person's message is delivered, and to that message's sender
// from then on, so a warm engagement that answers a second person charges
// that person for the answer.
func TestNativeTranscript_AttributesATurnToThePersonItAnswers(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-native-asker", "", t.TempDir())
	claimID := markEngaged(t, database, "r-native-asker")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	bob := uuid.New().String()
	if _, err := database.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'Bob')`, bob); err != nil {
		t.Fatalf("seed Bob: %v", err)
	}

	tr := newNativeTranscript(s, org, "r-native-asker", claimID).actFor(runmode.LocalDefaultUserID)
	reply := func(content string) string {
		t.Helper()
		id, err := tr.Insert(ctx, org, &domain.Message{ConversationID: "r-native-asker", Role: "assistant", Content: content})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		var user string
		if err := database.QueryRow(`SELECT COALESCE(user_id, '') FROM messages WHERE id = ?`, id).Scan(&user); err != nil {
			t.Fatalf("read %q: %v", content, err)
		}
		return user
	}
	if got := reply("before anyone else spoke"); got != runmode.LocalDefaultUserID {
		t.Errorf("a row before any delivered message is attributed to %q, want the person the engagement acts for", got)
	}

	asked, err := s.conversations.InsertMessageSystem(ctx, org, pendingUserInput("r-native-asker", bob, "and the other test?", domain.TurnSettings{}))
	if err != nil {
		t.Fatalf("queue Bob's message: %v", err)
	}
	if _, err := tr.ListForAssembly(ctx, org, "r-native-asker"); err != nil {
		t.Fatalf("ListForAssembly: %v", err)
	}
	if err := tr.MarkDelivered(ctx, org, "r-native-asker", []int{int(asked)}, ""); err != nil {
		t.Fatalf("deliver Bob's message: %v", err)
	}
	if got := reply("answering Bob"); got != bob {
		t.Errorf("the answer to Bob's message is attributed to %q, want %s", got, bob)
	}
}
