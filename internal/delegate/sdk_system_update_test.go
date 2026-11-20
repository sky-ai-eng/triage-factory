package delegate

import (
	"context"
	"os"
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
	at := strings.Index(message, "<system-update>")
	if at < 0 {
		t.Fatalf("the resume's message carries no update;\n%s", message)
	}
	if strings.Index(message, "which test?") > at {
		t.Error("the update precedes the person's message; it belongs after the input that starts the claim")
	}
	update := message[at:]
	for _, want := range []string{
		"Branch naming convention for this team: after-the-park/SKY-1440-resume",
		`name="` + sharedPartName(agentprompt.Sections(sdkSpec(agentprompt.SurfaceWorkbench))[0].Path) + `"`,
	} {
		if !strings.Contains(update, want) {
			t.Errorf("the update is missing %q;\n%s", want, update)
		}
	}
	if n := strings.Count(update, "<part "); n != 2 {
		t.Errorf("the update gives %d parts, want the two that changed;\n%s", n, update)
	}
	if rows := f.systemRows(t); len(rows) != 1 || !strings.Contains(message, rows[0].Content) {
		t.Errorf("system rows = %+v, want the one update the message carries", rows)
	}
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
	if strings.Contains(resumed.opts.Message, "<system-update>") {
		t.Errorf("a resume that composed what the session has carries an update;\n%s", resumed.opts.Message)
	}
	if rows := f.systemRows(t); len(rows) != 0 {
		t.Errorf("system rows = %d, want none", len(rows))
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
				note, at := strings.Index(input, domain.SessionContinuationNote), strings.Index(input, "<system-update>")
				if note < 0 || at < note {
					t.Fatalf("the opening input does not carry the update after the continuation note;\n%s", input)
				}
				input = input[at:]
			}
			if !strings.HasPrefix(input, "<system-update>") || !strings.Contains(input, "Branch naming convention for this team: after-the-park/SKY-1440-crash") {
				t.Errorf("the opening input does not end with the update carrying the new branch convention;\n%s", input)
			}
		})
	}
}
