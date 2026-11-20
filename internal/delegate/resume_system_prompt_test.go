package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A conversation keeps the two system blocks its first claim sent, because
// the provider binds every thinking block to the system prompt it was produced
// under. Every claim still composes both blocks from today's inputs, and a
// change reaches the conversation as an update carrying the parts that
// changed. A native conversation never takes the SDK's resume dispatch: a wake
// and a crash re-claim both come back through the fresh-claim path, and it is
// the conversation's task-context row, not a session id, that says whether the
// claim opens it.

// TestNativeLaunch_SavesBothBlocksAndTheirParts is the write the rest rests
// on. The launch composes both blocks and puts them on the row, with the parts
// they were composed from, before the agent is handed anything.
//
// Driven through runNativeAgent rather than the composer, because the save is
// the launch's and not the composition's: the tool host will not come up in a
// test, and the row must already carry the blocks by the time it fails.
func TestNativeLaunch_SavesBothBlocksAndTheirParts(t *testing.T) {
	f := newLaunchFixture(t, "native-stamp")

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the save")
	}

	saved := f.storedBlocks(t)
	if saved.Prompt != agentprompt.Build(nativeSpec(agentprompt.SurfaceMachinist)) {
		t.Error("the saved shared prompt is not the native machinist's framework blocks")
	}
	if !strings.Contains(saved.Block, "review the failing check") || !strings.Contains(saved.Block, "Branch naming convention") {
		t.Errorf("the saved block 2 carries no mission or no run context;\n%s", saved.Block)
	}
	if saved.Parts == nil {
		t.Fatal("the launch saved no parts; its next claim would have nothing to compare with")
	}
	sections := agentprompt.Sections(nativeSpec(agentprompt.SurfaceMachinist))
	if len(saved.Parts.Shared) != len(sections) {
		t.Errorf("saved %d shared parts, want one per framework block file (%d)", len(saved.Parts.Shared), len(sections))
	}
	if text, ok := partText(saved.Parts.Conversation, partBranchConvention); !ok || !strings.Contains(saved.Block, text) {
		t.Errorf("the saved parts carry no branch convention line that block 2 renders (%q, %v)", text, ok)
	}
}

// TestNativeLaunch_ToolsAreTheNativeComposition: a native run's <tools> section
// is the composition for its runtime, not the SDK's.
func TestNativeLaunch_ToolsAreTheNativeComposition(t *testing.T) {
	f := newLaunchFixture(t, "native-tools")
	kinds := []string{"github", "jira"}
	text := f.s.buildNativeLaunchText(context.Background(), "", f.task(t), "triage the ticket", runConfig{
		orgID:   runmode.LocalDefaultOrgID,
		teamID:  runmode.LocalDefaultTeamID,
		surface: agentprompt.SurfaceMachinist,
		sources: sourceDocs{kinds: kinds},
	}, "", claimReplay{})
	if want := agentprompt.ToolsReferenceForSources(agentprompt.RuntimeNative, kinds); !strings.Contains(text.system.sent.Block, want) {
		t.Errorf("the native block does not carry the native tools section;\n%s", text.system.sent.Block)
	}
}

// TestNativeReclaim_SendsTheBlocksItsFirstClaimSent is the ticket. A release
// that changes a framework block, and a team that rewrites its branch
// convention while the conversation is parked, both move what a claim
// composes; the re-claim still sends the bytes its first claim sent, which
// every earlier thinking block is bound to, and it rewrites nothing on the row.
func TestNativeReclaim_SendsTheBlocksItsFirstClaimSent(t *testing.T) {
	f := newLaunchFixture(t, "native-replay")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the save")
	}
	f.open(t)
	// The first claim ran an earlier release, whose identity block read
	// differently.
	launched := f.earlierRelease(t, "You are an earlier release's machinist.")

	f.setBranchTemplate(t, "after-the-park/<ticket-id>")
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened {
		t.Fatal("an opened conversation read as fresh")
	}
	system := f.nativeLaunchText(t, "review the failing check", "", replay).system
	if system.sent.Prompt != launched.Prompt || system.sent.Block != launched.Block {
		t.Errorf("the re-claim does not send the first claim's blocks;\nprompt equal: %v\nblock:\n%s", system.sent.Prompt == launched.Prompt, system.sent.Block)
	}
	if system.stamp {
		t.Error("a re-claim with the first claim's blocks in hand would save over them")
	}
	if system.current.Prompt != agentprompt.Build(nativeSpec(agentprompt.SurfaceMachinist)) {
		t.Error("the re-claim did not compose today's framework blocks to compare with")
	}

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the second claim's tool host launched")
	}
	if got := f.storedBlocks(t); got.Prompt != launched.Prompt || got.Block != launched.Block {
		t.Error("the re-claim rewrote the row")
	}
}

// TestNativeReclaim_AChangeArrivesAsAnUpdateOfThatPartAlone: each of the
// three kinds of change a parked conversation meets — the team's branch
// convention, a release's verb reference, a release's framework block —
// arrives as an update naming that part, with its current text, and nothing
// else from either block.
func TestNativeReclaim_AChangeArrivesAsAnUpdateOfThatPartAlone(t *testing.T) {
	identity := agentprompt.Sections(nativeSpec(agentprompt.SurfaceMachinist))[0]
	jira := agentprompt.ToolsReferenceParts(agentprompt.RuntimeNative, []string{"jira"})[0]
	for _, tc := range []struct {
		name   string
		change func(*launchFixture, *testing.T)
		key    string // the changed part's key
		part   string // the name the update gives the part
		holds  string // what the part's current text says
	}{
		{
			name:   "the branch convention",
			change: func(f *launchFixture, t *testing.T) { f.setBranchTemplate(t, "after-the-park/<ticket-id>") },
			key:    partBranchConvention,
			part:   conversationPartName(partBranchConvention),
			holds:  "after-the-park/<ticket-id>",
		},
		{
			name: "a verb reference",
			change: func(f *launchFixture, t *testing.T) {
				f.rewriteSavedPart(t, false, partToolsPrefix+"jira", "An earlier release's Jira verbs.")
			},
			key:   partToolsPrefix + "jira",
			part:  conversationPartName(partToolsPrefix + "jira"),
			holds: strings.TrimSpace(jira.Text),
		},
		{
			name:   "the shared prompt",
			change: func(f *launchFixture, t *testing.T) { f.earlierRelease(t, "You are an earlier release's machinist.") },
			key:    identity.Path,
			part:   sharedPartName(identity.Path),
			holds:  identity.Text,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t, "native-change")
			f.setBranchTemplate(t, "at-launch/<ticket-id>")
			if disp := f.runNativeWith(t, "review the failing check", []string{"github", "jira"}); disp.launchErr == nil {
				t.Fatal("the fixture's tool host launched")
			}
			f.open(t)
			tc.change(f, t)

			replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
			system := f.nativeLaunchTextWith(t, "review the failing check", []string{"github", "jira"}, replay).system
			update := system.update
			if update == "" {
				t.Fatal("the change produced no update")
			}
			if n := strings.Count(update, "<part "); n != 1 {
				t.Errorf("the update gives %d parts, want the one that changed;\n%s", n, update)
			}
			current := append(append([]domain.SystemPart{}, system.current.Parts.Shared...), system.current.Parts.Conversation...)
			changed := -1
			for i, p := range current {
				if p.Key == tc.key {
					changed = i
				}
			}
			if changed < 0 || !strings.Contains(current[changed].Text, tc.holds) {
				t.Fatalf("the claim did not compose the change to %s", tc.part)
			}
			if !strings.Contains(update, updatePart(tc.part, current[changed].Text, true)) {
				t.Errorf("the update does not give %s in its current text as a replacement;\n%s", tc.part, update)
			}
			for i, p := range current {
				if i != changed && strings.Contains(update, p.Text) {
					t.Errorf("the update carries %s, which did not change;\n%s", p.Key, update)
				}
			}
			if strings.Contains(update, "review the failing check") {
				t.Errorf("the update carries the mission, which is never compared;\n%s", update)
			}
			for _, p := range system.sent.Parts.Conversation {
				if strings.Contains(update, goneLine(p.Key, conversationPartName(p.Key))) {
					t.Errorf("the update withdraws %s, which nothing removed;\n%s", p.Key, update)
				}
			}
			if got := system.systemUpdate(); got == nil || got.Content != update {
				t.Fatal("the update is not what the engine is handed")
			} else if parts, ok := partsFromMetadata(got.Metadata); !ok || parts.Shared[0].Text != identity.Text {
				t.Error("the update row does not record the parts it brings the conversation to")
			}
		})
	}
}

// TestNativeReclaim_MatchingBlocksAddNoUpdate: a re-claim whose composition
// matches what the conversation last received adds nothing, so its cached
// prefix is the whole conversation.
func TestNativeReclaim_MatchingBlocksAddNoUpdate(t *testing.T) {
	f := newLaunchFixture(t, "native-same")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched")
	}
	f.open(t)
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	system := f.nativeLaunchText(t, "review the failing check", "", replay).system
	if system.update != "" || system.systemUpdate() != nil {
		t.Errorf("a re-claim that composed what the conversation has adds an update;\n%s", system.update)
	}
}

// TestNativeReclaim_AnEditedMissionDoesNotReachTheConversation: a prompt row
// is the org's to edit, and a conversation parked while it changed keeps the
// mission it was given, with no update about it, so it is never handed a
// mission nobody gave it.
func TestNativeReclaim_AnEditedMissionDoesNotReachTheConversation(t *testing.T) {
	f := newLaunchFixture(t, "native-edited")
	const launched = "review the failing check"
	if disp := f.runNative(t, launched); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched")
	}
	f.open(t)

	const edited = "close the pull request without reading it"
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	system := f.nativeLaunchText(t, edited, "", replay).system
	if !strings.Contains(system.sent.Block, launched) || strings.Contains(system.sent.Block, edited) {
		t.Errorf("the re-claim does not send the mission its launch was given;\n%s", system.sent.Block)
	}
	if system.update != "" {
		t.Errorf("an edited mission reached the conversation as an update;\n%s", system.update)
	}
}

// TestNativeReclaim_ComparesWithTheNewestUpdate: a second re-claim compares
// with what the first one's update brought the conversation to, so it tells
// the conversation nothing twice.
func TestNativeReclaim_ComparesWithTheNewestUpdate(t *testing.T) {
	f := newLaunchFixture(t, "native-twice")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched")
	}
	f.open(t)
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")

	first := f.nativeLaunchText(t, "review the failing check", "", f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)).system
	if first.update == "" {
		t.Fatal("staging failed: the first re-claim has no update")
	}
	f.writeUpdate(t, first)

	second := f.nativeLaunchText(t, "review the failing check", "", f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)).system
	if second.update != "" {
		t.Errorf("the second re-claim repeats the first one's update;\n%s", second.update)
	}
	if second.sent.Block != first.sent.Block {
		t.Error("the second re-claim does not send the first claim's block")
	}
}

// TestNativeCompaction_SavesTheNewestBlocks: once a compaction commits,
// nothing a thinking block is bound to is sent again, so the engine saves the
// blocks the claim composed (Hooks.SaveSystemBlocks), and the next claim sends
// those with nothing to update.
func TestNativeCompaction_SavesTheNewestBlocks(t *testing.T) {
	f := newLaunchFixture(t, "native-compact-save")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched")
	}
	f.open(t)
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")
	system := f.nativeLaunchText(t, "review the failing check", "", f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)).system
	if system.update == "" {
		t.Fatal("staging failed: the re-claim has no update")
	}

	save := f.s.saveSystemBlocksHook(runmode.LocalDefaultOrgID, f.conv.ID, f.conv.ClaimID, system.current)
	if err := save(context.Background(), agentloop.SystemBlocks{Prompt: system.current.Prompt, Addendum: system.current.Block}); err != nil {
		t.Fatalf("save the newest blocks: %v", err)
	}
	saved := f.storedBlocks(t)
	if saved.Prompt != system.current.Prompt || saved.Block != system.current.Block {
		t.Error("the compaction did not make the newest blocks the saved ones")
	}
	if !strings.Contains(saved.Block, "after-the-park/<ticket-id>") {
		t.Errorf("the saved block 2 does not carry today's convention;\n%s", saved.Block)
	}

	f.compact(t)
	next := f.nativeLaunchText(t, "review the failing check", "", f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)).system
	if next.sent.Block != system.current.Block || next.update != "" {
		t.Errorf("the claim after the compaction sends (%q) with update %q, want the saved blocks and nothing to add", next.sent.Block, next.update)
	}
}

// TestPersistSystemBlocks_RefusedOnceTheEngagementIsFencedOut: the blocks are
// a resume coordinate, so they take the fence the others take. A zombie whose
// launch composed late must not leave its blocks on a row a successor is
// driving — the successor's next claim would send them.
func TestPersistSystemBlocks_RefusedOnceTheEngagementIsFencedOut(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fenced", "sess-fenced", "/tmp/wt-fenced")
	zombie := markEngaged(t, database, "r-fenced")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	if fenced := s.persistSystemBlocks(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, domain.SystemBlocks{Block: "the launched mission"}); fenced {
		t.Fatal("persistSystemBlocks reported the fence on a live claim")
	}
	if _, err := database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'requeued' WHERE id = ?`, zombie); err != nil {
		t.Fatalf("release the claim: %v", err)
	}

	if fenced := s.persistSystemBlocks(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, domain.SystemBlocks{Block: "the zombie's mission"}); !fenced {
		t.Error("a released claim's system-block write was accepted; the successor's next claim would send it")
	}
	got, err := s.conversations.SystemBlocksSystem(ctx, runmode.LocalDefaultOrgID, "r-fenced")
	if err != nil || got.Block != "the launched mission" {
		t.Errorf("the row moved under the fence: (%q, %v)", got.Block, err)
	}
}

// TestNativeClaim_AnOpenedRowWithNoPartsSavesThem covers an opened row that
// holds no parts: on this runtime, one whose first claim's save did not land.
// A row with nothing on it composes and saves both blocks, as a launch does;
// one with a block keeps it and is saved with today's shared prompt and parts.
// Either way the claim after it compares rather than composing a third time.
func TestNativeClaim_AnOpenedRowWithNoPartsSavesThem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
	}{
		{name: "an empty row"},
		{name: "a row with only block 2", block: "<run_context>\nRun root: /work\n</run_context>\n\nthe launched mission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t, "native-no-parts")
			f.open(t)
			if _, err := f.database.Exec(`UPDATE conversations SET system_block = ? WHERE id = ?`, tc.block, f.conv.ID); err != nil {
				t.Fatalf("seed the row's block: %v", err)
			}
			replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
			if !replay.opened || replay.saved.Parts != nil || replay.readErr != nil {
				t.Fatalf("nativeClaimReplay = %+v, want an opened row with no parts", replay)
			}

			if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
				t.Fatal("the fixture's tool host launched")
			}
			saved := f.storedBlocks(t)
			if saved.Prompt != agentprompt.Build(nativeSpec(agentprompt.SurfaceMachinist)) || saved.Parts == nil {
				t.Fatal("the claim did not save today's shared prompt and parts onto the row")
			}
			if tc.block != "" && saved.Block != tc.block {
				t.Errorf("the claim replaced the block the row held;\n%s", saved.Block)
			}
			if tc.block == "" && !strings.Contains(saved.Block, "review the failing check") {
				t.Errorf("the claim composed nothing onto the empty row;\n%s", saved.Block)
			}
			next := f.nativeLaunchText(t, "review the failing check", "", f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)).system
			if next.stamp || next.sent.Block != saved.Block {
				t.Errorf("the next claim would not send what this one saved (stamp %v)", next.stamp)
			}
		})
	}
}

// storeReadDown is a conversation store with exactly one read broken, the rest
// of it intact — the shape of a transient failure on the call that decides
// what a claim sends.
type storeReadDown struct {
	db.ConversationStore
	opening bool
	blocks  bool
}

func (d storeReadDown) OpeningTaskContextSystem(ctx context.Context, orgID, conversationID string) (string, bool, error) {
	if d.opening {
		return "", false, errors.New("read opening: connection reset")
	}
	return d.ConversationStore.OpeningTaskContextSystem(ctx, orgID, conversationID)
}

func (d storeReadDown) SystemBlocksSystem(ctx context.Context, orgID, conversationID string) (domain.SystemBlocks, error) {
	if d.blocks {
		return domain.SystemBlocks{}, errors.New("read system blocks: connection reset")
	}
	return d.ConversationStore.SystemBlocksSystem(ctx, orgID, conversationID)
}

// TestNativeClaim_AnUnreadableRowIsNotAnEmptyOne is the asymmetry the save is
// gated on. A claim that cannot read what the row holds has to compose blocks
// to get its turn moving — but it must not then write them back, because the
// row may already carry the first claim's, and a save would make one failed
// read the permanent prompt of every claim after it.
func TestNativeClaim_AnUnreadableRowIsNotAnEmptyOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		down       storeReadDown
		wantOpened bool
	}{
		{name: "the system-blocks read is down", down: storeReadDown{blocks: true}, wantOpened: true},
		// An unreadable opening cannot say the conversation was opened, and the
		// claim reads as fresh — which is the memory gate's cheap mistake, and
		// must still not cost the row its blocks.
		{name: "the opening read is down", down: storeReadDown{opening: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t, "native-readfail")
			f.setBranchTemplate(t, "at-launch/<ticket-id>")
			if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
				t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the save")
			}
			launched := f.storedBlocks(t)
			if launched.Block == "" {
				t.Fatal("the launch saved nothing; this test would pass vacuously")
			}
			f.open(t)
			f.setBranchTemplate(t, "after-the-park/<ticket-id>")

			down := tc.down
			down.ConversationStore = f.s.conversations
			f.s.conversations = down

			replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
			if replay.opened != tc.wantOpened {
				t.Errorf("opened = %v, want %v", replay.opened, tc.wantOpened)
			}
			if want := map[bool]string{true: fixtureTaskContextRow}[tc.wantOpened]; replay.taskContext != want {
				t.Errorf("taskContext = %q, want %q", replay.taskContext, want)
			}
			system := f.nativeLaunchText(t, "review the failing check", "", replay).system
			if system.stamp {
				t.Error("a claim that could not read the row would save over it")
			}
			if system.update != "" {
				t.Errorf("a claim with nothing to compare with sends an update;\n%s", system.update)
			}

			if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
				t.Fatal("the wake's tool host launched")
			}
			if got := f.storedBlocks(t); got.Block != launched.Block || got.Prompt != launched.Prompt {
				t.Error("a failed read rewrote the row")
			}
		})
	}
}

// TestNativeReclaim_ACompactedConversationIsStillOpened: a compaction takes the
// task-context row out of the assembly window, and the conversation it opened
// has still been opened. Its next claim sends the saved blocks rather than
// composing and saving new ones, and keeps the opening's task context.
func TestNativeReclaim_ACompactedConversationIsStillOpened(t *testing.T) {
	f := newLaunchFixture(t, "native-compacted")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the save")
	}
	launched := f.storedBlocks(t)
	f.open(t)
	f.speak(t, "assistant", "on it")
	f.compact(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened {
		t.Fatal("a compacted conversation reads as never opened")
	}
	system := f.nativeLaunchText(t, "review the failing check", "", replay).system
	if system.sent.Block != launched.Block || system.stamp {
		t.Errorf("system = (block %q, stamp %v), want the saved block and no save", system.sent.Block, system.stamp)
	}
	if replay.taskContext != fixtureTaskContextRow {
		t.Errorf("taskContext = %q, want the opening's %q", replay.taskContext, fixtureTaskContextRow)
	}
}

// TestNativeNextStep_ComposesItsOwnBlocks is the other side of the rule, and
// the reason it is keyed on the conversation rather than the task or the
// blueprint run. Step N+1 is a new conversation: it has run nothing and saved
// nothing, so it composes against today's inputs and saves what it composed.
func TestNativeNextStep_ComposesItsOwnBlocks(t *testing.T) {
	f := newLaunchFixture(t, "native-step-two")
	f.setBranchTemplate(t, "step-one/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the save")
	}
	f.open(t)
	stepOne := f.storedBlocks(t)

	f.setBranchTemplate(t, "step-two/<ticket-id>")
	stepTwo := f.nextStep(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, stepTwo)
	if replay.opened || replay.readErr != nil {
		t.Fatalf("nativeClaimReplay for a new step = %+v, want a fresh conversation", replay)
	}
	system := f.s.buildNativeLaunchText(context.Background(), stepTwo, f.task(t), "open the pull request", runConfig{
		orgID:   runmode.LocalDefaultOrgID,
		teamID:  runmode.LocalDefaultTeamID,
		surface: agentprompt.SurfaceMachinist,
		sources: sourceDocs{kinds: []string{"github"}},
	}, "", replay).system
	if !system.stamp || system.update != "" {
		t.Errorf("a new step = (stamp %v, update %q), want its own blocks saved and no update", system.stamp, system.update)
	}
	if !strings.Contains(system.sent.Block, "step-two/<ticket-id>") {
		t.Errorf("the new step did not compose against today's inputs;\n%s", system.sent.Block)
	}
	if system.sent.Block == stepOne.Block {
		t.Error("the new step sent step one's block")
	}
}

// runNative drives one native engagement over the fixture's claim. Its tool host
// cannot launch in a test, so what it exercises is everything ahead of that — the
// staging, the system-block decision and the save — and the launchErr it returns
// is the expected end of the call rather than a failure of the test.
func (f *launchFixture) runNative(t *testing.T, mission string) engagementDisposition {
	t.Helper()
	return f.runNativeWith(t, mission, []string{"github"})
}

func (f *launchFixture) runNativeWith(t *testing.T, mission string, kinds []string) engagementDisposition {
	t.Helper()
	return f.s.runNativeAgent(context.Background(), f.conv.ID, f.task(t), mission, runConfig{
		orgID:          runmode.LocalDefaultOrgID,
		teamID:         runmode.LocalDefaultTeamID,
		claimID:        f.conv.ClaimID,
		blueprintRunID: f.br.ID,
		surface:        agentprompt.SurfaceMachinist,
		runRoot:        f.worktree,
		sources:        sourceDocs{kinds: kinds},
	}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID)
}

// nativeLaunchText resolves one claim's launch text the way runNativeAgent does,
// with the two inputs a test varies handed in: the listing this claim's staging
// pass rendered, and what the claim inherited (nativeClaimReplay).
func (f *launchFixture) nativeLaunchText(t *testing.T, mission, knowledge string, replay claimReplay) nativeLaunchText {
	t.Helper()
	return f.s.buildNativeLaunchText(context.Background(), f.conv.ID, f.task(t), mission, runConfig{
		orgID:   runmode.LocalDefaultOrgID,
		teamID:  runmode.LocalDefaultTeamID,
		surface: agentprompt.SurfaceMachinist,
		sources: sourceDocs{kinds: []string{"github"}},
	}, knowledge, replay)
}

func (f *launchFixture) nativeLaunchTextWith(t *testing.T, mission string, kinds []string, replay claimReplay) nativeLaunchText {
	t.Helper()
	return f.s.buildNativeLaunchText(context.Background(), f.conv.ID, f.task(t), mission, runConfig{
		orgID:   runmode.LocalDefaultOrgID,
		teamID:  runmode.LocalDefaultTeamID,
		surface: agentprompt.SurfaceMachinist,
		sources: sourceDocs{kinds: kinds},
	}, "", replay)
}

func (f *launchFixture) task(t *testing.T) domain.Task {
	t.Helper()
	task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load the fixture task: (%v, %v)", task, err)
	}
	return *task
}

func (f *launchFixture) storedBlocks(t *testing.T) domain.SystemBlocks {
	t.Helper()
	blocks, err := f.stores.Conversations.SystemBlocksSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatalf("SystemBlocksSystem: %v", err)
	}
	return blocks
}

// earlierRelease rewrites the saved blocks as a release whose first framework
// block read identity would have saved them, and returns what it saved.
func (f *launchFixture) earlierRelease(t *testing.T, identity string) domain.SystemBlocks {
	t.Helper()
	saved := f.storedBlocks(t)
	if saved.Parts == nil || len(saved.Parts.Shared) == 0 {
		t.Fatal("the row holds no parts to rewrite")
	}
	now := saved.Parts.Shared[0].Text
	saved.Prompt = strings.Replace(saved.Prompt, now, identity, 1)
	saved.Parts.Shared[0].Text = identity
	f.saveBlocks(t, saved)
	return saved
}

// rewriteSavedPart rewrites one saved part's text, as a release that composed
// that part differently would have saved it.
func (f *launchFixture) rewriteSavedPart(t *testing.T, shared bool, key, text string) {
	t.Helper()
	saved := f.storedBlocks(t)
	list := saved.Parts.Conversation
	if shared {
		list = saved.Parts.Shared
	}
	for i := range list {
		if list[i].Key == key {
			list[i].Text = text
			f.saveBlocks(t, saved)
			return
		}
	}
	t.Fatalf("the saved parts have no %s", key)
}

func (f *launchFixture) saveBlocks(t *testing.T, blocks domain.SystemBlocks) {
	t.Helper()
	if _, err := f.stores.Conversations.SetSystemBlocksForClaimSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID, f.conv.ClaimID, blocks); err != nil {
		t.Fatalf("SetSystemBlocksForClaimSystem: %v", err)
	}
}

// writeUpdate writes a claim's update the way the engine does.
func (f *launchFixture) writeUpdate(t *testing.T, system claimSystem) {
	t.Helper()
	update := system.systemUpdate()
	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: f.conv.ID, Role: "system", Subtype: domain.MessageSubtypeSystemUpdate,
		Content: update.Content, Metadata: update.Metadata,
	}); err != nil {
		t.Fatalf("insert the update: %v", err)
	}
}

func partText(parts []domain.SystemPart, key string) (string, bool) {
	for _, p := range parts {
		if p.Key == key {
			return p.Text, true
		}
	}
	return "", false
}

// setBranchTemplate rewrites the team's branch-naming convention — the live
// input block 2's run context is composed from, and the one a test moves to
// watch a re-claim compare. Read-modify-write, because the settings upsert
// writes the whole row.
func (f *launchFixture) setBranchTemplate(t *testing.T, tmpl string) {
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

// nextStep advances the blueprint onto its second step, the way the reactor
// does: one transaction moves the pointer, ends the step it moves past and
// mints a new conversation on the same task, sharing the run tree its
// predecessor built. Returns its id.
func (f *launchFixture) nextStep(t *testing.T) string {
	t.Helper()
	stepIndex := 1
	advanced, _, next, err := f.stores.Blueprints.AdvanceRunToStepSystem(
		context.Background(), runmode.LocalDefaultOrgID, 0, f.conv.ID, domain.Conversation{
			ID: f.conv.ID + "-s2", TaskID: f.conv.TaskID, PromptID: f.conv.PromptID, Model: f.conv.Model,
			TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
			BlueprintRunID: f.br.ID, BlueprintStepIndex: &stepIndex, WorktreePath: f.worktree,
		})
	if err != nil || !advanced || next == nil {
		t.Fatalf("AdvanceRunToStepSystem to step 2 = (%v, %v, %v)", advanced, next, err)
	}
	return next.ID
}
