package delegate

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// The SDK runtime's half of the rule. Claude Code owns the session's history
// and takes its append once, at the launch, so a resume is handed the blocks
// the session was started with, and what changed since goes in the resume's
// opening input, the way staged notes do.

// sdkFirstTurn runs the interactive fixture's first turn, which saves the
// conversation's blocks, and readies it for a follow-up.
func sdkFirstTurn(t *testing.T, f *interactiveFixture, session string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	first := f.endTurn(t, session, "the build is red because a test is flaky")
	f.s.dispatchClaimedConversation(context.Background(), f.claim(t, "why is the build red?"), time.Now())
	if first.park.claudeCwd == "" {
		t.Fatal("the first turn never launched")
	}
	f.stampModel(t)
}

// earlierSDKRelease rewrites the conversation's saved blocks as a release
// whose first framework block read differently would have saved them.
func (f *interactiveFixture) earlierSDKRelease(t *testing.T) domain.SystemBlocks {
	t.Helper()
	ctx := context.Background()
	saved, err := f.stores.Conversations.SystemBlocksSystem(ctx, runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil || saved.Parts == nil || saved.Prompt == "" {
		t.Fatalf("the first turn saved no blocks: (%+v, %v)", saved, err)
	}
	const earlier = "You are an earlier release's workbench agent."
	saved.Prompt = strings.Replace(saved.Prompt, saved.Parts.Shared[0].Text, earlier, 1)
	saved.Parts.Shared[0].Text = earlier
	parts, err := db.MarshalSystemParts(saved.Parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.Exec(`UPDATE conversations SET system_prompt = ?, system_parts = ? WHERE id = ?`, saved.Prompt, parts, f.conv.ID); err != nil {
		t.Fatalf("rewrite the saved blocks: %v", err)
	}
	return saved
}

func (f *interactiveFixture) setBranchTemplate(t *testing.T, tmpl string) {
	t.Helper()
	ctx := context.Background()
	settings, err := f.stores.Teams.GetSettingsSystem(ctx, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	settings.BranchTemplate = tmpl
	if _, err := f.stores.Teams.UpdateSettings(ctx, runmode.LocalDefaultTeamID, settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
}

func (f *interactiveFixture) systemRows(t *testing.T) []domain.Message {
	t.Helper()
	var out []domain.Message
	for _, r := range allRows(t, f.s, f.conv.ID) {
		if r.Role == "system" {
			out = append(out, r)
		}
	}
	return out
}

func agentBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return agentproc.AgentVisibleBinary(self)
}

// TestSDKResume_SendsTheBlocksItsFirstClaimSent: a person's next message wakes
// the session, and the append it is resumed with is the one it was started
// with, though a release changed a framework block and the team its branch
// convention since. Both changes reach it in the resume's message, after the
// person's text, and on the transcript as the update's row.
func TestSDKResume_SendsTheBlocksItsFirstClaimSent(t *testing.T) {
	f := newInteractiveFixture(t, "1440-resume")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	sdkFirstTurn(t, f, "sess-1440-resume")
	saved := f.earlierSDKRelease(t)
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")

	resumed := f.endTurn(t, "sess-1440-resume", "it was the retry")
	f.s.dispatchClaimedConversation(context.Background(), f.claim(t, "which test?"), time.Now())
	if resumed.opts.SessionID != "sess-1440-resume" {
		t.Fatalf("the follow-up launched session %q, want the first one resumed", resumed.opts.SessionID)
	}
	if want := sdkSystemPrompt(saved, agentBinary(t)); resumed.opts.SystemPrompt != want {
		t.Error("the resume's append is not the one its session was started with")
	}

	message := resumed.opts.Message
	rows := f.systemRows(t)
	if len(rows) != 1 {
		t.Fatalf("system rows = %+v, want one update", rows)
	}
	update := rows[0].Content
	at := strings.Index(message, update)
	if at < 0 {
		t.Fatalf("the resume's message does not carry the update its row records;\n%s", message)
	}
	if strings.Index(message, "which test?") > at {
		t.Error("the update precedes the person's message; it belongs after the input that starts the claim")
	}
	now, ok := partsFromMetadata(rows[0].Metadata)
	if !ok {
		t.Fatal("the update row records no parts")
	}
	section := agentprompt.Sections(sdkSpec(agentprompt.SurfaceWorkbench))[0]
	if got, want := changedParts(saved.Parts, now), []string{section.Path, partBranchConvention}; !slices.Equal(got, want) {
		t.Errorf("the update brings the session to parts that changed %v, want %v", got, want)
	}
	if !strings.Contains(update, "after-the-park/SKY-1440-resume") {
		t.Errorf("the update does not carry the new branch convention;\n%s", update)
	}
	if want := resolveCLIPath(systemUpdateText(saved.Parts, now), agentBinary(t)); update != want {
		t.Errorf("the update is not what changed since the session's blocks;\n%s", update)
	}
}

// changedParts are the keys of the parts that differ between two
// compositions, shared then conversation, each in its composition's order.
func changedParts(before, after *domain.SystemParts) []string {
	var keys []string
	diff := func(b, a []domain.SystemPart) {
		was := map[string]string{}
		for _, p := range b {
			was[p.Key] = p.Text
		}
		is := map[string]bool{}
		for _, p := range a {
			is[p.Key] = true
			if old, had := was[p.Key]; !had || old != p.Text {
				keys = append(keys, p.Key)
			}
		}
		for _, p := range b {
			if !is[p.Key] {
				keys = append(keys, p.Key)
			}
		}
	}
	diff(before.Shared, after.Shared)
	diff(before.Conversation, after.Conversation)
	return keys
}

// TestSDKResume_MatchingBlocksAddNoUpdate: nothing changed, so the resume's
// message is the person's and nothing more.
func TestSDKResume_MatchingBlocksAddNoUpdate(t *testing.T) {
	f := newInteractiveFixture(t, "1440-same")
	sdkFirstTurn(t, f, "sess-1440-same")
	saved, err := f.stores.Conversations.SystemBlocksSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatal(err)
	}

	resumed := f.endTurn(t, "sess-1440-same", "it was the retry")
	f.s.dispatchClaimedConversation(context.Background(), f.claim(t, "which test?"), time.Now())
	if want := sdkSystemPrompt(saved, agentBinary(t)); resumed.opts.SystemPrompt != want {
		t.Error("the resume's append is not the one its session was started with")
	}
	if rows := f.systemRows(t); len(rows) != 0 {
		t.Errorf("system rows = %d, want none", len(rows))
	}
	if msg := resumed.opts.Message; !strings.HasSuffix(msg, "which test?") {
		t.Errorf("the resume's message carries something after the person's;\n%s", msg)
	}
}

// TestRunAgent_ALaterLaunchSendsItsSavedBlocks: the other SDK re-claim, a
// launch on a conversation its predecessor left mid-turn, sends the saved
// blocks too, whether it resumes the session or starts a fresh one because the
// session file was lost. Its opening input carries the update last: after the
// continuation note, or after the opening rows.
func TestRunAgent_ALaterLaunchSendsItsSavedBlocks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		loseSession bool
	}{
		{name: "resuming the session"},
		{name: "on a fresh session", loseSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInteractiveFixture(t, "1440-crash")
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID
			const session = "sess-1440-crash"
			f.setBranchTemplate(t, "at-launch/<ticket-id>")
			t.Setenv("HOME", t.TempDir())
			first := f.endTurn(t, session, "the build is red because a test is flaky")
			f.s.dispatchClaimedConversation(ctx, f.claim(t, "why is the build red?"), time.Now())
			root := first.park.claudeCwd
			if root == "" {
				t.Fatal("the first turn never launched")
			}
			f.stampModel(t)
			saved := f.earlierSDKRelease(t)
			f.setBranchTemplate(t, "after-the-park/<ticket-id>")
			if tc.loseSession {
				worktree.RemoveClaudeProjectDir(root)
			}

			// An engagement takes the follow-up off the queue and ends before
			// its reply does, so the next claim launches the runtime.
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

			launched := f.endTurn(t, session, "it was the retry")
			f.s.runAgent(ctx, f.conv.ID, *task, "", runConfig{
				orgID:       org,
				teamID:      runmode.LocalDefaultTeamID,
				claimID:     claimed.ClaimID,
				runRoot:     root,
				surface:     agentprompt.SurfaceWorkbench,
				interactive: true,
			}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID, session, 0)

			if resumed := launched.opts.SessionID == session; resumed == tc.loseSession {
				t.Fatalf("the re-claim launched session %q; resumed = %v, want %v", launched.opts.SessionID, resumed, !tc.loseSession)
			}
			if want := sdkSystemPrompt(saved, agentBinary(t)); launched.opts.SystemPrompt != want {
				t.Error("the re-claim's append is not the one the conversation's first claim sent")
			}
			if got, err := f.stores.Conversations.SystemBlocksSystem(ctx, org, f.conv.ID); err != nil || got.Prompt != saved.Prompt || got.Block != saved.Block {
				t.Error("the re-claim rewrote the saved blocks")
			}

			rows := f.systemRows(t)
			if len(rows) != 1 {
				t.Fatalf("system rows = %+v, want one update", rows)
			}
			update := rows[0].Content
			var input, before string
			if tc.loseSession {
				blocks := launched.opts.OpeningBlocks
				if len(blocks) == 0 {
					t.Fatal("the fresh session's launch sent no opening")
				}
				input = blocks[len(blocks)-1].Text
				before = "<task_context>"
				for _, b := range blocks[:len(blocks)-1] {
					if strings.Contains(b.Text, before) {
						before = ""
					}
				}
				if before != "" {
					t.Errorf("the opening carries no task context ahead of the update")
				}
			} else {
				input = launched.opts.Message
				note, at := strings.Index(input, domain.SessionContinuationNote), strings.Index(input, update)
				if note < 0 || at < note {
					t.Fatalf("the opening input does not carry the update after the continuation note;\n%s", input)
				}
			}
			if !strings.HasSuffix(input, update) || !strings.Contains(update, "after-the-park/SKY-1440-crash") {
				t.Errorf("the opening input does not end with the update carrying the new branch convention;\n%s", input)
			}

			// The opening never reaching the agent takes the update back, so
			// the next claim compares with what the session holds.
			if launched.opts.OnOpeningUnsent == nil {
				t.Fatal("the opening carries an update and has no undo for it")
			}
			launched.opts.OnOpeningUnsent()
			if rows := f.systemRows(t); len(rows) != 0 {
				t.Errorf("system rows after an unsent opening = %+v, want the update withdrawn", rows)
			}
			if got, err := f.stores.Conversations.LatestSystemUpdateSystem(ctx, org, f.conv.ID); err != nil || got != nil {
				t.Errorf("LatestSystemUpdateSystem after an unsent opening = (%+v, %v), want none", got, err)
			}
		})
	}
}

// TestSDKResume_AnOpeningThatNeverReachedTheSessionSendsTheUpdateAgain: a
// resume whose message never reached the session takes its update row back
// with the notes it carried, so the next resume tells the session the change.
func TestSDKResume_AnOpeningThatNeverReachedTheSessionSendsTheUpdateAgain(t *testing.T) {
	f := newInteractiveFixture(t, "1440-unsent")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	sdkFirstTurn(t, f, "sess-1440-unsent")
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")

	unsent := f.endTurn(t, "sess-1440-unsent", "it was the retry")
	f.s.dispatchClaimedConversation(context.Background(), f.claim(t, "which test?"), time.Now())
	if len(f.systemRows(t)) != 1 || unsent.opts.OnOpeningUnsent == nil {
		t.Fatal("staging failed: the resume carries no update to take back")
	}
	unsent.opts.OnOpeningUnsent()
	if rows := f.systemRows(t); len(rows) != 0 {
		t.Fatalf("system rows after an unsent opening = %+v, want the update withdrawn", rows)
	}

	next := f.endTurn(t, "sess-1440-unsent", "the retry has a timeout")
	f.s.dispatchClaimedConversation(context.Background(), f.claim(t, "how long?"), time.Now())
	rows := f.systemRows(t)
	if len(rows) != 1 || !strings.Contains(rows[0].Content, "after-the-park/SKY-1440-unsent") {
		t.Fatalf("system rows = %+v, want the change sent again", rows)
	}
	if !strings.Contains(next.opts.Message, rows[0].Content) {
		t.Errorf("the next resume's message does not carry the update;\n%s", next.opts.Message)
	}
}

// TestSDKResume_AnUnreadableRowSendsTheFrameworkBlocksAlone: a resume that
// cannot read what its session was started with sends the framework blocks,
// tells the session nothing, and leaves the row alone.
func TestSDKResume_AnUnreadableRowSendsTheFrameworkBlocksAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	conv := &domain.Conversation{ID: "r-does-not-exist", OrgID: runmode.LocalDefaultOrgID, Type: domain.ConversationTypeInteractive}

	system := s.resumeClaimSystem(context.Background(), conv, domain.Task{ID: "t-none"}, t.TempDir(), true, true)
	want := domain.SystemBlocks{Prompt: agentprompt.Build(sdkSpec(agentprompt.SurfaceWorkbench))}
	if system.sent.Prompt != want.Prompt || system.sent.Block != "" || system.sent.Parts != nil {
		t.Errorf("sent = %+v, want the framework blocks alone", system.sent)
	}
	if system.update != "" || system.stamp {
		t.Errorf("an unreadable row produced an update or a save: %+v", system)
	}
}

// TestSDKResume_AnUnreadableBlueprintRunKeepsTheStepEndingNote: whether a step
// is its blueprint's last is read from the blueprint run, and a resume that
// cannot read it keeps the step-ending note the session holds rather than
// telling a step that its handoff no longer applies.
func TestSDKResume_AnUnreadableBlueprintRunKeepsTheStepEndingNote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-step", "sess-step", "/tmp/wt-step")
	claimID := markEngaged(t, database, "r-step")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()
	conv := &domain.Conversation{ID: "r-step", OrgID: runmode.LocalDefaultOrgID, ClaimID: claimID, BlueprintRunID: "br-step"}
	task, cwd := domain.Task{ID: "t-step"}, t.TempDir()

	// The session was started as a step with another after it.
	if first := s.resumeClaimSystem(ctx, conv, task, cwd, false, true); !hasPart(first.current.Parts, partStepEnd) {
		t.Fatal("staging failed: a non-final step composed no step-ending note")
	}

	if got := s.resumeClaimSystem(ctx, conv, task, cwd, true, false); got.update != "" {
		t.Errorf("a resume that could not read its blueprint run changed the step's ending;\n%s", got.update)
	}
	if got := s.resumeClaimSystem(ctx, conv, task, cwd, true, true); !strings.Contains(got.update, goneLine(partStepEnd, conversationPartName(partStepEnd))) {
		t.Errorf("a resume that read its step is final kept the step-ending note;\n%s", got.update)
	}
}
