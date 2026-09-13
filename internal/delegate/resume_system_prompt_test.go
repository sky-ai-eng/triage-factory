package delegate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// launchBlock is the block 2 a GitHub PR step's launch composes — the same four
// sections runAgent hands composeConversationSystemBlock, with the mission
// caller-supplied so a test can watch a particular one survive.
func launchBlock(mission string) string {
	return composeConversationSystemBlock(
		mission,
		runContext("Repository: owner/repo\nPR: #7", "/work", "tfac/SKY-9", "https://tf.example/runs/r-1", ""),
		agentprompt.GitHubToolsReference(),
		agentprompt.NonTerminalCompletion(machinistSpec()),
	)
}

// TestResumeAppend_IsTheLaunchAppendByteForByte is what this leaf is for. The
// SDK harness is handed its append once and replays only the session transcript
// afterwards, so a woken turn has to be handed the same string again — not one
// that agrees with it, the same one.
//
// The launch stores block 2 and the resume reads it back; both then reach the
// wire through sdkSystemPrompt, so the bytes are equal by construction rather
// than by two composers being kept in step.
func TestResumeAppend_IsTheLaunchAppendByteForByte(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-append", "sess-append", "/tmp/wt-append")
	claimID := markEngaged(t, database, "r-append")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	block := launchBlock("review the pull request and leave a summary")
	launchAppend := sdkSystemPrompt(block, "/bin/tf")
	if fenced := s.persistSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-append", claimID, block); fenced {
		t.Fatal("persistSystemBlock reported the fence on a live claim")
	}

	resumeAppend := sdkSystemPrompt(
		s.launchedSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-append"), "/bin/tf")
	if resumeAppend != launchAppend {
		t.Errorf("the resume's append is not the launch's;\ngot:\n%s\n\nwant:\n%s", resumeAppend, launchAppend)
	}
	// Named explicitly rather than left to the byte comparison: the three things
	// a resumed turn used to lose are the framework blocks, the mission and the
	// step addendum, and a composer that dropped all three would still make the
	// line above pass.
	if !strings.HasPrefix(resumeAppend, frameworkBlocks(t)) {
		t.Error("the resumed turn does not lead with the framework blocks")
	}
	if !strings.Contains(resumeAppend, "review the pull request and leave a summary") {
		t.Error("the resumed turn carries no mission")
	}
	if !strings.Contains(resumeAppend, "You are one step inside a multi-step blueprint") {
		t.Error("the resumed turn dropped the non-terminal step's addendum")
	}
}

// TestResumeAppend_IsTheLaunchedMissionNotTodaysPromptRow is why the block is a
// stored value rather than a recomposition. A prompt row is the org's to edit,
// and a conversation parked for a week would otherwise wake under whatever it
// says now — a mission nobody gave that conversation.
func TestResumeAppend_IsTheLaunchedMissionNotTodaysPromptRow(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-edited", "sess-edited", "/tmp/wt-edited")
	claimID := markEngaged(t, database, "r-edited")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	const launched = "review the pull request and leave a summary"
	if fenced := s.persistSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-edited", claimID, launchBlock(launched)); fenced {
		t.Fatal("persistSystemBlock reported the fence on a live claim")
	}

	// The org rewrites the step's prompt while the conversation is parked.
	const edited = "close the pull request without reading it"
	if _, err := s.prompts.Update(ctx, runmode.LocalDefaultOrgID, "test-prompt", "T", edited, ""); err != nil {
		t.Fatalf("edit the step prompt: %v", err)
	}
	if p, err := s.prompts.GetSystem(ctx, runmode.LocalDefaultOrgID, "test-prompt"); err != nil || p == nil || p.Body != edited {
		t.Fatalf("the fixture's prompt edit did not land: %v %+v", err, p)
	}

	woke := sdkSystemPrompt(s.launchedSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-edited"), "/bin/tf")
	if !strings.Contains(woke, launched) {
		t.Error("the resumed turn lost the mission its launch was given")
	}
	if strings.Contains(woke, edited) {
		t.Error("the resumed turn picked up a mission edited after the launch")
	}
}

// TestPersistSystemBlock_RefusedOnceTheEngagementIsFencedOut: the append is a
// resume coordinate, so it takes the fence the other three take. A zombie whose
// launch composed late must not leave its own mission on a row a successor is
// driving — the successor's next wake would replay it.
func TestPersistSystemBlock_RefusedOnceTheEngagementIsFencedOut(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fenced", "sess-fenced", "/tmp/wt-fenced")
	zombie := markEngaged(t, database, "r-fenced")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	if fenced := s.persistSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, launchBlock("the launched mission")); fenced {
		t.Fatal("persistSystemBlock reported the fence on a live claim")
	}
	if _, err := database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'requeued' WHERE id = ?`, zombie); err != nil {
		t.Fatalf("release the claim: %v", err)
	}

	if fenced := s.persistSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, launchBlock("the zombie's mission")); !fenced {
		t.Error("a released claim's system-block write was accepted; the successor's next wake would replay it")
	}
	got := s.launchedSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-fenced")
	if !strings.Contains(got, "the launched mission") || strings.Contains(got, "the zombie's mission") {
		t.Errorf("the row moved under the fence;\n%s", got)
	}
}

// TestLaunchedSystemBlock_DegradesToTheFrameworkBlocksAlone covers the two rows
// that answer with nothing: one whose block really was empty, and one this
// process cannot read. Neither refuses the turn — the agent keeps its session
// and loses the mission for it, which is what every SDK resume did before the
// column existed.
func TestLaunchedSystemBlock_DegradesToTheFrameworkBlocksAlone(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-empty", "sess-empty", "/tmp/wt-empty")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	for _, conversationID := range []string{"r-empty", "r-does-not-exist"} {
		if got := s.launchedSystemBlock(ctx, runmode.LocalDefaultOrgID, conversationID); got != "" {
			t.Errorf("%s: launchedSystemBlock = %q, want the empty block", conversationID, got)
		}
		if got := sdkSystemPrompt("", "/bin/tf"); got != frameworkBlocks(t) {
			t.Errorf("%s: an empty block must compose the framework blocks alone;\n%s", conversationID, got)
		}
	}
}

// TestResumeSystemBlockRead_WritesNothing is the opening-rows gate from this
// side. A resume mints no opening and delivers only the queued follow-up, so
// the one thing this leaf adds to that path — a read of the launch's block —
// must leave both the transcript and the queue exactly as it found them.
func TestResumeSystemBlockRead_WritesNothing(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-readonly", "sess-readonly", "/tmp/wt-readonly")
	claimID := markEngaged(t, database, "r-readonly")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-readonly", claimID, "event", runmode.LocalDefaultUserID)

	if _, err := s.openingTurnBlocks(ctx, sink, runmode.LocalDefaultOrgID, "r-readonly",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext); err != nil {
		t.Fatalf("openingTurnBlocks: %v", err)
	}
	if fenced := s.persistSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-readonly", claimID, launchBlock("the launched mission")); fenced {
		t.Fatal("persistSystemBlock reported the fence on a live claim")
	}
	if _, err := s.conversations.InsertMessageSystem(ctx, runmode.LocalDefaultOrgID,
		pendingUserInput("r-readonly", runmode.LocalDefaultUserID, "also update the README")); err != nil {
		t.Fatalf("queue the follow-up: %v", err)
	}
	before := len(allRows(t, s, "r-readonly"))

	if got := s.launchedSystemBlock(ctx, runmode.LocalDefaultOrgID, "r-readonly"); !strings.Contains(got, "the launched mission") {
		t.Fatalf("launchedSystemBlock = %q", got)
	}

	if got := len(allRows(t, s, "r-readonly")); got != before {
		t.Errorf("rows after the resume's read = %d, want %d — a resume mints nothing", got, before)
	}
	pending := pendingRows(t, s, "r-readonly")
	if len(pending) != 1 || pending[0].Content != "also update the README" {
		t.Errorf("pending input = %+v, want the follow-up alone — that is all the resume delivers", pending)
	}
}

// The native half of the same rule, and the two ways it differs from the SDK's.
// A native conversation never takes the resume dispatch: a wake and a crash
// re-claim both come back through the fresh-claim path, and the transcript they
// find is the one the loop is about to replay. So the rule is stated per claim
// rather than per path — a new step composes its own block 2, an opened
// conversation replays the one its launch stamped — and it is the row, not a
// session id, that says which of the two this claim is.

// TestNativeLaunch_StampsBlockTwoOnTheRow is the write the rest of it rests on.
// The launch composes block 2 and puts it on the row before the agent is handed
// anything, so the bytes survive the engagement that composed them.
//
// Driven through runNativeAgent rather than the composer, because the stamp is
// the launch's and not the composition's: the tool host will not come up in a
// test, and the row must already carry the block by the time it fails.
func TestNativeLaunch_StampsBlockTwoOnTheRow(t *testing.T) {
	f := newLaunchFixture(t, "native-stamp")

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}

	stamped := f.storedSystemBlock(t)
	if !strings.Contains(stamped, "review the failing check") {
		t.Errorf("the stamped block carries no mission;\n%s", stamped)
	}
	if !strings.Contains(stamped, "Branch naming convention") {
		t.Errorf("the stamped block carries no run context;\n%s", stamped)
	}
}

// TestNativeReclaim_ReplaysTheStampedBlockByteForByte is the ticket. A native
// conversation woken after a park keeps the transcript it built, so a block 2
// composed afresh would change the model's own instructions mid-conversation —
// and the input that moves under it here is a live one: the team's branch
// convention, which an admin may rewrite while the run is parked.
//
// The assertion is byte equality, not agreement. Block 2 is what block 1's cache
// breakpoint is measured against, so a block that merely says the same things in
// different bytes still costs the conversation its warm prefix.
func TestNativeReclaim_ReplaysTheStampedBlockByteForByte(t *testing.T) {
	f := newLaunchFixture(t, "native-replay")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	launched := f.storedSystemBlock(t)
	if !strings.Contains(launched, "at-launch/<ticket-id>") {
		t.Fatalf("the launch did not compose the team's branch convention;\n%s", launched)
	}
	// What the launch's own mint writes, and what makes this conversation an
	// opened one for every claim after it.
	f.open(t)

	// The team rewrites its branch convention while the conversation is parked.
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened {
		t.Fatal("an opened conversation read as fresh; the wake would compose a second block")
	}
	if replay.stampable {
		t.Error("a wake with the launch's block in hand reported the row as writable")
	}
	woke := f.nativeLaunchText(t, "review the failing check", "", replay.block)
	if woke.systemBlock != launched {
		t.Errorf("the wake's block is not the launch's;\ngot:\n%s\n\nwant:\n%s", woke.systemBlock, launched)
	}
	// Named rather than left to the byte comparison, which a composer that
	// dropped the section entirely would also satisfy.
	if strings.Contains(woke.systemBlock, "after-the-park/<ticket-id>") {
		t.Error("the wake picked up a branch convention set after the launch")
	}

	// And the second claim adds nothing to the row: the bytes it is running
	// under are the ones already on it.
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the second claim's tool host launched")
	}
	if got := f.storedSystemBlock(t); got != launched {
		t.Errorf("the re-claim rewrote the row;\ngot:\n%s\n\nwant:\n%s", got, launched)
	}
}

// TestNativeReclaim_KeepsTheManifestOnAHandedOffTree is the section that cannot
// be recomposed at all. The knowledge manifest is rendered from what a launch
// staged, and a launch stages nothing into a run tree that already belongs to
// the sandbox identity — so a recomposing wake would not merely reword the
// block, it would drop a section naming a tree the agent is still reading from.
func TestNativeReclaim_KeepsTheManifestOnAHandedOffTree(t *testing.T) {
	f := newLaunchFixture(t, "native-manifest")
	const manifest = "Team knowledge is staged at _tfac/knowledge/team/private/runbook.md"

	// The launch's staging pass, which returns the manifest it rendered.
	launched := f.nativeLaunchText(t, "review the failing check", manifest, "").systemBlock
	if !strings.Contains(launched, manifest) {
		t.Fatalf("the launch composed no manifest;\n%s", launched)
	}
	if fenced := f.s.persistSystemBlock(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID, f.conv.ClaimID, launched); fenced {
		t.Fatal("persistSystemBlock reported the fence on a live claim")
	}
	f.open(t)

	// The wake, on a tree handed off: nothing is staged, so the manifest this
	// claim could render is empty. The replay is what keeps the section.
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if got := f.nativeLaunchText(t, "review the failing check", "", replay.block).systemBlock; got != launched {
		t.Errorf("the wake's block is not the launch's;\ngot:\n%s\n\nwant:\n%s", got, launched)
	}
	// The control: composing on that same empty manifest is what the wake used
	// to do, and it is what loses the section.
	if got := f.nativeLaunchText(t, "review the failing check", "", "").systemBlock; strings.Contains(got, manifest) {
		t.Error("a composition on an empty manifest still named the staged tree; this test proves nothing")
	}
}

// TestNativeClaim_OpenedRowWithNoBlockComposesOnceAndStamps covers the rows that
// predate the stamp. They are opened and hold nothing, and the answer for them is
// the one a launch gets: compose live, once, and put it on the row — so the claim
// after this one replays rather than composing a third time.
func TestNativeClaim_OpenedRowWithNoBlockComposesOnceAndStamps(t *testing.T) {
	f := newLaunchFixture(t, "native-legacy")
	f.open(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened || replay.block != "" || !replay.stampable {
		t.Fatalf("nativeClaimReplay = %+v, want an opened row with nothing to replay and nothing to lose", replay)
	}

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	stamped := f.storedSystemBlock(t)
	if !strings.Contains(stamped, "review the failing check") {
		t.Errorf("the claim composed nothing onto the empty row;\n%s", stamped)
	}
	if next := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID); next.block != stamped {
		t.Errorf("the next claim would not replay what this one composed;\ngot:\n%s\n\nwant:\n%s", next.block, stamped)
	}
}

// storeReadDown is a conversation store with exactly one read broken, the rest
// of it intact — the shape of a transient failure on the call that decides
// whether a claim replays or composes.
type storeReadDown struct {
	db.ConversationStore
	transcript bool
	block      bool
}

func (d storeReadDown) ListForAssemblySystem(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	if d.transcript {
		return nil, errors.New("read transcript: connection reset")
	}
	return d.ConversationStore.ListForAssemblySystem(ctx, orgID, conversationID)
}

func (d storeReadDown) SystemBlockSystem(ctx context.Context, orgID, conversationID string) (string, error) {
	if d.block {
		return "", errors.New("read system block: connection reset")
	}
	return d.ConversationStore.SystemBlockSystem(ctx, orgID, conversationID)
}

// TestNativeClaim_AnUnreadableRowIsNotAnEmptyOne is the asymmetry the stamp is
// gated on. A claim that cannot read what the row holds has to compose a block 2
// to get its turn moving — but it must not then write that block back, because
// the row may already carry the launch's, and a stamp would make one failed read
// the permanent prompt of every claim after it. So the turn degrades and the row
// does not: the next claim still reaches the launch's bytes.
//
// Both reads that answer the question are covered. Whichever fails, the claim
// cannot tell an empty row from an unreadable one, which is the whole reason
// "empty" is not what it writes on.
func TestNativeClaim_AnUnreadableRowIsNotAnEmptyOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		down       storeReadDown
		wantOpened bool
	}{
		{name: "the system-block read is down", down: storeReadDown{block: true}, wantOpened: true},
		// An unreadable transcript cannot say the conversation was opened, and
		// the claim reads as fresh — which is the memory gate's cheap mistake,
		// and must still not cost the row its block.
		{name: "the transcript read is down", down: storeReadDown{transcript: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t, "native-readfail")
			f.setBranchTemplate(t, "at-launch/<ticket-id>")
			if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
				t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
			}
			launched := f.storedSystemBlock(t)
			if launched == "" {
				t.Fatal("the launch stamped nothing; this test would pass vacuously")
			}
			f.open(t)
			f.setBranchTemplate(t, "after-the-park/<ticket-id>")

			// The read goes down between the park and the wake. The write does
			// not, so a stamp would land if this claim made one.
			down := tc.down
			down.ConversationStore = f.s.conversations
			f.s.conversations = down

			replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
			if replay.opened != tc.wantOpened {
				t.Errorf("opened = %v, want %v", replay.opened, tc.wantOpened)
			}
			if replay.block != "" {
				t.Errorf("block = %q, want nothing to replay — the read failed", replay.block)
			}
			// Error rather than Fatal: the row assertion below is the damage
			// this one predicts, and a regression should report both.
			if replay.stampable {
				t.Error("a claim that could not read the row reported it as writable; a transient read would rewrite a parked conversation's prompt")
			}

			// The turn runs on a recomposed block, and the row keeps the
			// launch's for the claim after it.
			if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
				t.Fatal("the wake's tool host launched")
			}
			if got := f.storedSystemBlock(t); got != launched {
				t.Errorf("a failed read rewrote the row;\ngot:\n%s\n\nwant:\n%s", got, launched)
			}
		})
	}
}

// TestNativeNextStep_ComposesItsOwnBlock is the other side of the rule, and the
// reason it is keyed on the conversation rather than the task or the blueprint
// run. Step N+1 is a new conversation: it has run nothing and stamped nothing, so
// it composes — and it composes against today's inputs, which is what a new step
// is for.
func TestNativeNextStep_ComposesItsOwnBlock(t *testing.T) {
	f := newLaunchFixture(t, "native-step-two")
	f.setBranchTemplate(t, "step-one/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	f.open(t)
	stepOne := f.storedSystemBlock(t)

	// The team's convention changes, and the blueprint advances onto a second
	// step — a second conversation on the same task and the same run tree.
	f.setBranchTemplate(t, "step-two/<ticket-id>")
	stepTwo := f.nextStep(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, stepTwo)
	if replay.opened || replay.block != "" || !replay.stampable {
		t.Fatalf("nativeClaimReplay for a new step = %+v, want a fresh conversation with a block of its own to stamp", replay)
	}
	composed := f.nativeLaunchText(t, "open the pull request", "", replay.block).systemBlock
	if !strings.Contains(composed, "step-two/<ticket-id>") {
		t.Errorf("the new step did not compose against today's inputs;\n%s", composed)
	}
	if composed == stepOne {
		t.Error("the new step replayed step one's block")
	}
}

// runNative drives one native engagement over the fixture's claim. Its tool host
// cannot launch in a test, so what it exercises is everything ahead of that — the
// staging, the block-2 decision and the stamp — and the launchErr it returns is
// the expected end of the call rather than a failure of the test.
func (f *launchFixture) runNative(t *testing.T, mission string) engagementDisposition {
	t.Helper()
	return f.s.runNativeAgent(context.Background(), f.conv.ID, f.task(t), mission, runConfig{
		orgID:          runmode.LocalDefaultOrgID,
		teamID:         runmode.LocalDefaultTeamID,
		claimID:        f.conv.ClaimID,
		blueprintRunID: f.br.ID,
		wtPath:         f.worktree,
		runRoot:        f.worktree,
		toolsRef:       agentprompt.GitHubToolsReference(),
	}, time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID)
}

// nativeLaunchText resolves one claim's launch text the way runNativeAgent does,
// with the two inputs a test varies handed in: the manifest this claim's staging
// pass rendered, and the block a previous launch stamped.
func (f *launchFixture) nativeLaunchText(t *testing.T, mission, knowledge, replayed string) nativeLaunchText {
	t.Helper()
	return f.s.buildNativeLaunchText(context.Background(), f.task(t), mission, runConfig{
		orgID:    runmode.LocalDefaultOrgID,
		teamID:   runmode.LocalDefaultTeamID,
		toolsRef: agentprompt.GitHubToolsReference(),
	}, knowledge, replayed)
}

func (f *launchFixture) task(t *testing.T) domain.Task {
	t.Helper()
	task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load the fixture task: (%v, %v)", task, err)
	}
	return *task
}

func (f *launchFixture) storedSystemBlock(t *testing.T) string {
	t.Helper()
	block, err := f.stores.Conversations.SystemBlockSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatalf("SystemBlockSystem: %v", err)
	}
	return block
}

// setBranchTemplate rewrites the team's branch-naming convention — the live
// input block 2's run context is composed from, and the one a test moves to
// watch a replay hold still. Read-modify-write, because the settings upsert
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

// nextStep stages the blueprint's second step: a new conversation on the same
// task, sharing the run tree its predecessor built. Returns its id.
func (f *launchFixture) nextStep(t *testing.T) string {
	t.Helper()
	stepIndex := 1
	next, err := f.stores.ConversationQueue.EnqueueConversation(context.Background(), runmode.LocalDefaultOrgID, domain.Conversation{
		ID: f.conv.ID + "-s2", TaskID: f.conv.TaskID, PromptID: f.conv.PromptID, Model: f.conv.Model,
		TriggerType: "manual", CreatorUserID: runmode.LocalDefaultUserID,
		BlueprintRunID: f.br.ID, BlueprintStepIndex: &stepIndex, WorktreePath: f.worktree,
	})
	if err != nil {
		t.Fatalf("EnqueueConversation for step 2: %v", err)
	}
	return next.ID
}

// taskIDOf reads the task a fixture conversation was seeded against.
func taskIDOf(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = ?`, conversationID).Scan(&taskID); err != nil {
		t.Fatalf("read task_id for %s: %v", conversationID, err)
	}
	return taskID
}
