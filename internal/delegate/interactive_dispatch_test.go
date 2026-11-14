package delegate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
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
	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, f.conv.ID, runmode.LocalDefaultUserID, message); err != nil {
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
	f.s.startLiveRun = func(_ context.Context, spec liveRunSpec) (*agentproc.LiveRun, error) {
		*got = spec
		return nil, errLaunchRecorded
	}
	return got
}

// dispatch claims the person's message and drives it through the real
// dispatcher, returning what the launch handed the SDK runtime.
func (f *interactiveFixture) dispatch(t *testing.T, message string) *liveRunSpec {
	t.Helper()
	launched := f.recordLaunch()
	claimed := f.claim(t, message)
	if claimed.BlueprintRunID != "" || claimed.SessionID != "" {
		t.Fatalf("claimed conversation = (blueprint run %q, session %q), want neither", claimed.BlueprintRunID, claimed.SessionID)
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
// in that fails leaves the task they still hold with a new interactive
// conversation as its current one, created for that person, while the failure
// stays on the old row. A task the person no longer holds gets none.
func TestFailedInteractiveConversationIsReplaced(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unclaim     bool
		wantReplace bool
	}{
		{name: "the person still holds the task", wantReplace: true},
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
			// The runtime refuses to start, which fails the conversation.
			f.dispatch(t, "why is the build red?")

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
	if err := f.s.SendMessage(ctx, runmode.LocalDefaultOrgID, task.ConversationID, runmode.LocalDefaultUserID, "try again"); err != nil {
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
