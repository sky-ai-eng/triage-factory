package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A conversation keeps the system text its first claim sent, because the
// provider binds every thinking block to it, and each later claim tells it
// what changed in an update appended after the claim's input. These tests pin
// both halves: what a claim sends, and what its update carries.

// pinnedInputs is one claim's inputs for a native PR step, with the branch
// convention and the usable sources as the two a test moves.
func pinnedInputs(branch string, kinds ...string) systemInputs {
	return systemInputs{
		spec:           nativeSpec(agentprompt.SurfaceMachinist),
		mission:        "Review the pull request named above.",
		scope:          "Repository: owner/repo\nPR: #7",
		prCheckout:     "/work/owner/repo/pr-7",
		branchTemplate: branch,
		runURL:         "https://tf.example/runs/r-1",
		sources:        sourceDocs{kinds: kinds},
		stepEnd:        agentprompt.NonTerminalCompletion(),
	}
}

func partByKey(t *testing.T, parts []domain.SystemPart, key string) agentprompt.Part {
	t.Helper()
	for _, p := range parts {
		if p.Key == key {
			return agentprompt.Part(p)
		}
	}
	t.Fatalf("no part %q in the composition", key)
	return agentprompt.Part{}
}

// TestSystemUpdate_ABranchChangeIsThatOneLine: an update carries the parts
// that changed and nothing else from either block, so a changed branch
// convention arrives as its one line.
func TestSystemUpdate_ABranchChangeIsThatOneLine(t *testing.T) {
	was := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))
	now := composeSystemParts(pinnedInputs("team/<ticket-id>", "github"))

	got := systemUpdate(was, now)
	want := agentprompt.InstructionsUpdate([]agentprompt.Part{partByKey(t, now, partBranch)}, nil)
	if got != want {
		t.Errorf("update =\n%s\n\nwant only the branch convention:\n%s", got, want)
	}
}

// TestSystemUpdate_ASourceTurnedOnArrivesInFull: a source whose verbs became
// available is documented in full, and so are the workspace verbs the first
// tracker source brings with it.
func TestSystemUpdate_ASourceTurnedOnArrivesInFull(t *testing.T) {
	was := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))
	now := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github", "jira"))

	arrived := agentprompt.ToolsReferenceParts(agentprompt.RuntimeNative, []string{"jira"})
	if len(arrived) == 0 {
		t.Fatal("Jira composes no verb reference; this test would pass vacuously")
	}
	if got, want := systemUpdate(was, now), agentprompt.InstructionsUpdate(arrived, nil); got != want {
		t.Errorf("update =\n%s\n\nwant the Jira reference in full:\n%s", got, want)
	}
}

// TestSystemUpdate_ASourceTurnedOffIsOneLine: a source whose verbs went away is
// withdrawn by name, one line each, with none of the reference it had.
func TestSystemUpdate_ASourceTurnedOffIsOneLine(t *testing.T) {
	was := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github", "jira"))
	now := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))

	gone := agentprompt.ToolsReferenceParts(agentprompt.RuntimeNative, []string{"jira"})
	got := systemUpdate(was, now)
	if want := agentprompt.InstructionsUpdate(nil, gone); got != want {
		t.Errorf("update =\n%s\n\nwant one line per withdrawn part:\n%s", got, want)
	}
	for _, p := range gone {
		if strings.Contains(got, strings.TrimSpace(p.Text)) {
			t.Errorf("the update repeats the withdrawn %s", p.Name)
		}
	}
}

// TestSystemUpdate_NothingChangedSaysNothing: a claim whose composition matches
// what the conversation last heard adds no update.
func TestSystemUpdate_NothingChangedSaysNothing(t *testing.T) {
	parts := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github", "jira"))
	if got := systemUpdate(parts, composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github", "jira"))); got != "" {
		t.Errorf("update = %q, want none", got)
	}
}

// TestResolveSystemClaim_ALaunchSendsAndRecordsItsComposition: a claim that
// opens the conversation has nothing to keep, so it sends what it composed and
// records it before the agent is handed anything.
func TestResolveSystemClaim_ALaunchSendsAndRecordsItsComposition(t *testing.T) {
	composed := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))
	got := resolveSystemClaim(false, domain.SystemText{}, nil, composed)
	want := renderSystem(composed)
	if got.send.Prompt != want.Prompt || got.send.Block != want.Block || !got.stamp || got.update != "" {
		t.Errorf("launch claim = %+v, want its own composition, recorded, with no update", got)
	}
}

// TestResolveSystemClaim_AnOpenedConversationKeepsItsFrozenText covers a
// release that changed a framework block while the conversation was parked.
// The claim sends the prompt the conversation was frozen with, and the changed
// block arrives in the update.
func TestResolveSystemClaim_AnOpenedConversationKeepsItsFrozenText(t *testing.T) {
	composed := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))
	released := partByKey(t, composed, composed[0].Key)

	// The frozen text is the same composition as the previous release wrote it.
	earlier := append([]domain.SystemPart(nil), composed...)
	earlier[0].Text = "<identity>\nThe previous release's identity block.\n</identity>"
	frozen := renderSystem(earlier)

	got := resolveSystemClaim(true, frozen, nil, composed)
	if got.send.Prompt != frozen.Prompt || got.send.Block != frozen.Block {
		t.Error("the claim did not send the text the conversation was frozen with")
	}
	if got.stamp {
		t.Error("the claim records over the frozen text")
	}
	if want := agentprompt.InstructionsUpdate([]agentprompt.Part{released}, nil); got.update != want {
		t.Errorf("update =\n%s\n\nwant the one changed framework block:\n%s", got.update, want)
	}
	if got.current.Prompt != renderSystem(composed).Prompt {
		t.Error("the claim's current text is not this release's composition, which a compaction adopts")
	}
}

// TestResolveSystemClaim_ComparesAgainstTheNewestUpdate: the conversation has
// heard whatever the newest update in its window told it, so a change it was
// already told of is not repeated, and a further one is told once.
func TestResolveSystemClaim_ComparesAgainstTheNewestUpdate(t *testing.T) {
	frozen := renderSystem(composeSystemParts(pinnedInputs("first/<ticket-id>", "github")))
	told := composeSystemParts(pinnedInputs("second/<ticket-id>", "github"))

	if got := resolveSystemClaim(true, frozen, told, composeSystemParts(pinnedInputs("second/<ticket-id>", "github"))); got.update != "" {
		t.Errorf("a change already told was told again:\n%s", got.update)
	}
	third := composeSystemParts(pinnedInputs("third/<ticket-id>", "github"))
	got := resolveSystemClaim(true, frozen, told, third)
	if want := agentprompt.InstructionsUpdate([]agentprompt.Part{partByKey(t, third, partBranch)}, nil); got.update != want {
		t.Errorf("update =\n%s\n\nwant the third convention alone:\n%s", got.update, want)
	}
}

// TestResolveSystemClaim_CarriesTheMissionAndTheRunsFacts: the mission and the
// run's fixed facts are the step's. A claim that composed them from today's
// prompt row and run takes the frozen ones, and tells the conversation nothing
// about them.
func TestResolveSystemClaim_CarriesTheMissionAndTheRunsFacts(t *testing.T) {
	frozen := renderSystem(composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github")))
	today := pinnedInputs("tfac/<ticket-id>", "github")
	today.mission = "Close the pull request without reading it."
	today.scope = "Repository: owner/renamed\nPR: #7"
	today.runURL = "https://tf.example/runs/elsewhere"

	got := resolveSystemClaim(true, frozen, nil, composeSystemParts(today))
	if got.update != "" {
		t.Errorf("a carried part reached the update:\n%s", got.update)
	}
	if got.current.Block != frozen.Block {
		t.Errorf("the claim's current text took today's carried parts;\ngot:\n%s\n\nwant:\n%s", got.current.Block, frozen.Block)
	}
}

// TestResolveSystemClaim_AConversationWithNoRecordedPartsKeepsItsBlock covers a
// conversation opened before parts were recorded. It keeps the one block that
// was kept, tells it nothing, and records this claim's composition from here on.
func TestResolveSystemClaim_AConversationWithNoRecordedPartsKeepsItsBlock(t *testing.T) {
	composed := composeSystemParts(pinnedInputs("tfac/<ticket-id>", "github"))
	got := resolveSystemClaim(true, domain.SystemText{Block: "the block it was launched with"}, nil, composed)
	if got.send.Block != "the block it was launched with" || got.send.Prompt != renderSystem(composed).Prompt {
		t.Errorf("send = %+v, want the kept block behind this claim's framework prompt", got.send)
	}
	if !got.stamp || got.update != "" {
		t.Errorf("claim = %+v, want its composition recorded and no update", got)
	}
}

// TestLastToldParts reads the newest update still in the window, in the shape
// it was written and in the shape a store hands back.
func TestLastToldParts(t *testing.T) {
	first := []domain.SystemPart{{Key: partBranch, Name: "the branch naming convention", Text: "first"}}
	second := []domain.SystemPart{{Key: partBranch, Name: "the branch naming convention", Text: "second"}}
	var decoded any
	raw, _ := json.Marshal(second)
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	update := func(parts any, window domain.MessageWindowState) domain.Message {
		return domain.Message{
			Role: "system", Subtype: domain.MessageSubtypeInjectionSystemUpdate, WindowState: window,
			Metadata: map[string]any{domain.SystemUpdateMetadataKey: parts},
		}
	}
	for _, tc := range []struct {
		name string
		rows []domain.Message
		want []domain.SystemPart
	}{
		{"no update", []domain.Message{{Role: "user", Content: "go"}}, nil},
		{"the newest", []domain.Message{update(first, ""), {Role: "assistant"}, update(decoded, "")}, second},
		{"one compacted away", []domain.Message{update(first, ""), update(second, domain.MessageWindowInactive)}, first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lastToldParts(tc.rows)
			if (got == nil) != (tc.want == nil) || (got != nil && (len(got) != 1 || got[0] != tc.want[0])) {
				t.Errorf("lastToldParts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPersistSystemText_RefusedOnceTheEngagementIsFencedOut: a zombie whose
// launch composed late must not leave its text on a row a successor is
// driving, since every later claim sends what the row holds.
func TestPersistSystemText_RefusedOnceTheEngagementIsFencedOut(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fenced", "sess-fenced", "/tmp/wt-fenced")
	zombie := markEngaged(t, database, "r-fenced")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	ctx := context.Background()

	launched := renderSystem(composeSystemParts(pinnedInputs("launched/<ticket-id>", "github")))
	if fenced := s.persistSystemText(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, launched); fenced {
		t.Fatal("persistSystemText reported the fence on a live claim")
	}
	if _, err := database.Exec(
		`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'requeued' WHERE id = ?`, zombie); err != nil {
		t.Fatalf("release the claim: %v", err)
	}

	late := renderSystem(composeSystemParts(pinnedInputs("zombie/<ticket-id>", "github")))
	if fenced := s.persistSystemText(ctx, runmode.LocalDefaultOrgID, "r-fenced", zombie, late); !fenced {
		t.Error("a released claim's system-text write was accepted")
	}
	got, err := s.conversations.SystemTextSystem(ctx, runmode.LocalDefaultOrgID, "r-fenced")
	if err != nil || got.Block != launched.Block {
		t.Errorf("the row moved under the fence: (%q, %v)", got.Block, err)
	}
}

// TestNativeLaunch_StampsTheSystemTextOnTheRow is the write the rest rests on:
// the launch records both blocks and their parts before the agent is handed
// anything, so the bytes survive the engagement that composed them.
//
// Driven through runNativeAgent rather than the composer, because the stamp is
// the launch's: the tool host will not come up in a test, and the row must
// already carry the text by the time it fails.
func TestNativeLaunch_StampsTheSystemTextOnTheRow(t *testing.T) {
	f := newLaunchFixture(t, "native-stamp")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}

	stamped := f.storedSystemText(t)
	if stamped.Prompt != agentprompt.Build(nativeSpec(agentprompt.SurfaceMachinist)) {
		t.Error("the stamped prompt is not the framework blocks")
	}
	for _, want := range []string{"review the failing check", "Branch naming convention"} {
		if !strings.Contains(stamped.Block, want) {
			t.Errorf("the stamped block is missing %q;\n%s", want, stamped.Block)
		}
	}
	if rendered := renderSystem(stamped.Parts); rendered.Prompt != stamped.Prompt || rendered.Block != stamped.Block {
		t.Error("the stamped parts do not render to the stamped blocks")
	}
}

// TestNativeReclaim_SendsTheFrozenTextAndCarriesTheChange: a native
// conversation woken after a park sends the text it was frozen with, byte for
// byte, and the branch convention a team admin rewrote meanwhile reaches it as
// an update of that one line.
func TestNativeReclaim_SendsTheFrozenTextAndCarriesTheChange(t *testing.T) {
	f := newLaunchFixture(t, "native-replay")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	launched := f.storedSystemText(t)
	f.open(t)

	f.setBranchTemplate(t, "after-the-park/<ticket-id>")
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened {
		t.Fatal("an opened conversation read as fresh")
	}
	woke := f.nativeLaunchText(t, "review the failing check", replay).system
	if woke.send.Prompt != launched.Prompt || woke.send.Block != launched.Block {
		t.Errorf("the wake does not send the launch's text;\ngot:\n%s\n\nwant:\n%s", woke.send.Block, launched.Block)
	}
	branch := partByKey(t, woke.current.Parts, partBranch)
	if !strings.Contains(branch.Text, "after-the-park/<ticket-id>") {
		t.Fatalf("the wake did not compose today's branch convention: %q", branch.Text)
	}
	if want := agentprompt.InstructionsUpdate([]agentprompt.Part{branch}, nil); woke.update != want {
		t.Errorf("update =\n%s\n\nwant the branch convention alone:\n%s", woke.update, want)
	}

	// The second claim leaves the row alone: the text it sends is the one
	// already on it.
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the second claim's tool host launched")
	}
	if got := f.storedSystemText(t); got.Block != launched.Block || got.Prompt != launched.Prompt {
		t.Error("the re-claim rewrote the row")
	}
}

// TestNativeReclaim_SendsTheFrozenPromptAfterARelease: the framework prompt a
// conversation was frozen with is what its later claims send, even after a
// release changed a block of it, and the block that changed arrives in the
// update.
func TestNativeReclaim_SendsTheFrozenPromptAfterARelease(t *testing.T) {
	f := newLaunchFixture(t, "native-release")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	f.open(t)

	// The previous release wrote a different first framework block.
	stamped := f.storedSystemText(t)
	released := agentprompt.Part(stamped.Parts[0])
	earlier := append([]domain.SystemPart(nil), stamped.Parts...)
	earlier[0].Text = "<identity>\nThe previous release's identity block.\n</identity>"
	if fenced := f.s.persistSystemText(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID, f.conv.ClaimID, renderSystem(earlier)); fenced {
		t.Fatal("persistSystemText reported the fence on a live claim")
	}
	frozen := f.storedSystemText(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	woke := f.nativeLaunchText(t, "review the failing check", replay).system
	if woke.send.Prompt != frozen.Prompt {
		t.Error("the wake sends this release's framework prompt rather than the one it was frozen with")
	}
	if want := agentprompt.InstructionsUpdate([]agentprompt.Part{released}, nil); woke.update != want {
		t.Errorf("update =\n%s\n\nwant the changed block alone:\n%s", woke.update, want)
	}
}

// TestNativeClaim_AnUnreadableSystemTextFailsTheLaunch: composing in place of
// the frozen text would send a system prompt the conversation's thinking was
// not produced under, which the provider refuses. So the claim fails, and the
// row keeps the launch's text for the claim after it.
func TestNativeClaim_AnUnreadableSystemTextFailsTheLaunch(t *testing.T) {
	f := newLaunchFixture(t, "native-textdown")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	launched := f.storedSystemText(t)
	f.open(t)

	f.s.conversations = storeReadDown{ConversationStore: f.s.conversations, system: true}
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if _, err := f.s.buildNativeLaunchText(context.Background(), f.conv.ID, f.task(t), "review the failing check", f.cfg(), replay); err == nil {
		t.Error("the claim composed in place of a frozen text it could not read")
	}
	f.s.conversations = f.stores.Conversations
	if got := f.storedSystemText(t); got.Block != launched.Block {
		t.Error("the row lost the launch's text")
	}
}

// TestNativeClaim_AnUnreadableOpeningRecordsNothing: a claim that cannot read
// the opening reads the conversation as fresh, which is the memory gate's cheap
// mistake. It cannot tell an empty row from one holding the launch's text, so
// it must not write its own composition over it.
func TestNativeClaim_AnUnreadableOpeningRecordsNothing(t *testing.T) {
	f := newLaunchFixture(t, "native-openingdown")
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	launched := f.storedSystemText(t)
	f.open(t)
	f.setBranchTemplate(t, "after-the-park/<ticket-id>")

	f.s.conversations = storeReadDown{ConversationStore: f.s.conversations, opening: true}
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the wake's tool host launched")
	}
	if got := f.storedSystemText(t); got.Block != launched.Block {
		t.Errorf("a failed read rewrote the row;\ngot:\n%s\n\nwant:\n%s", got.Block, launched.Block)
	}
}

// TestNativeClaim_OpenedRowWithNoPartsStampsOnce covers a conversation opened
// before parts were recorded: its claim composes, records, and the claim after
// it sends what this one recorded.
func TestNativeClaim_OpenedRowWithNoPartsStampsOnce(t *testing.T) {
	f := newLaunchFixture(t, "native-legacy")
	f.open(t)

	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	stamped := f.storedSystemText(t)
	if stamped.Parts == nil || !strings.Contains(stamped.Block, "review the failing check") {
		t.Fatalf("the claim recorded nothing onto the row: %+v", stamped)
	}
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if next := f.nativeLaunchText(t, "review the failing check", replay).system; next.send.Block != stamped.Block || next.stamp {
		t.Error("the next claim does not send what this one recorded")
	}
}

// TestNativeReclaim_ACompactedConversationIsStillOpened: a compaction takes the
// task-context row out of the assembly window, and the conversation it opened
// has still been opened. Its next claim sends the frozen text and keeps the
// opening's task context.
func TestNativeReclaim_ACompactedConversationIsStillOpened(t *testing.T) {
	f := newLaunchFixture(t, "native-compacted")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	launched := f.storedSystemText(t)
	f.open(t)
	f.speak(t, "assistant", "on it")
	f.compact(t)

	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if !replay.opened || replay.taskContext != fixtureTaskContextRow {
		t.Fatalf("replay = %+v, want an opened conversation with the opening's task context", replay)
	}
	if woke := f.nativeLaunchText(t, "review the failing check", replay).system; woke.send.Block != launched.Block || woke.stamp {
		t.Error("the claim after a compaction does not send the frozen text")
	}
}

// TestNativeNextStep_ComposesItsOwnText: step N+1 is a new conversation, which
// has run nothing and recorded nothing, so it composes against today's inputs.
func TestNativeNextStep_ComposesItsOwnText(t *testing.T) {
	f := newLaunchFixture(t, "native-step-two")
	f.setBranchTemplate(t, "step-one/<ticket-id>")
	if disp := f.runNative(t, "review the failing check"); disp.launchErr == nil {
		t.Fatal("the fixture's tool host launched; this test needs the launch to fail after the stamp")
	}
	f.open(t)
	stepOne := f.storedSystemText(t)

	f.setBranchTemplate(t, "step-two/<ticket-id>")
	stepTwo := f.nextStep(t)
	replay := f.s.nativeClaimReplay(context.Background(), runmode.LocalDefaultOrgID, stepTwo)
	if replay.opened {
		t.Fatal("a new step read as opened")
	}
	composed := f.nativeLaunchText(t, "open the pull request", replay).system
	if !strings.Contains(composed.send.Block, "step-two/<ticket-id>") || !composed.stamp {
		t.Errorf("the new step did not compose and record against today's inputs;\n%s", composed.send.Block)
	}
	if composed.send.Block == stepOne.Block {
		t.Error("the new step sent step one's text")
	}
}

// TestNativeLaunch_ToolsAreTheNativeComposition: a native run's <tools> section
// is the composition for its runtime, not the SDK's.
func TestNativeLaunch_ToolsAreTheNativeComposition(t *testing.T) {
	f := newLaunchFixture(t, "native-tools")
	kinds := []string{"github", "jira"}
	cfg := f.cfg()
	cfg.sources = sourceDocs{kinds: kinds}
	text, err := f.s.buildNativeLaunchText(context.Background(), "", f.task(t), "triage the ticket", cfg, claimReplay{})
	if err != nil {
		t.Fatalf("compose the launch text: %v", err)
	}
	if want := agentprompt.ToolsReferenceForSources(agentprompt.RuntimeNative, kinds); !strings.Contains(text.system.send.Block, want) {
		t.Errorf("the native block does not carry the native tools section;\n%s", text.system.send.Block)
	}
}

// TestSDKResume_SendsTheFrozenAppendAndCarriesTheChange is the SDK half. Claude
// Code replays the session but not the prompt appended to it, so a resume
// hands it the append the conversation was frozen with, and what changed rides
// the turn's message. The step-ending note is the step's place in its plan,
// which the resume reads off the frozen text, so it is neither dropped nor
// re-announced.
func TestSDKResume_SendsTheFrozenAppendAndCarriesTheChange(t *testing.T) {
	f := newLaunchFixture(t, "sdk-resume")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	f.setBranchTemplate(t, "at-launch/<ticket-id>")
	task := f.task(t)

	launch := func() domain.SystemText {
		return renderSystem(composeSystemParts(systemInputs{
			spec:           sdkSpec(surfaceFor(f.conv.Type)),
			mission:        "review the pull request and leave a summary",
			scope:          "Repository: owner/repo\nPR: #7",
			runRoot:        "/work",
			taskState:      f.s.closedTaskContext(ctx, org, task.ID),
			siblingWork:    f.s.siblingWorkContext(ctx, org, f.conv.ID),
			branchTemplate: f.s.resolveBranchTemplate(ctx, task),
			sources:        f.s.sourceDocsFor(ctx, org, f.conv.CreatorUserID, f.conv.ID, task.EntitySource),
			stepEnd:        agentprompt.NonTerminalCompletion(),
		}))
	}
	launched := launch()
	if fenced := f.s.persistSystemText(ctx, org, f.conv.ID, f.conv.ClaimID, launched); fenced {
		t.Fatal("persistSystemText reported the fence on a live claim")
	}

	if got := f.s.resumeSystemClaim(ctx, org, &f.conv, task); got.update != "" {
		t.Errorf("a resume with nothing changed carries an update:\n%s", got.update)
	}

	f.setBranchTemplate(t, "after-the-park/<ticket-id>")
	got := f.s.resumeSystemClaim(ctx, org, &f.conv, task)
	if sdkAppend(got.send, "/bin/tf") != sdkAppend(launched, "/bin/tf") {
		t.Error("the resume's append is not the launch's")
	}
	if got.stamp {
		t.Error("a resume records over the frozen text")
	}
	branch := partByKey(t, got.current.Parts, partBranch)
	if want := agentprompt.InstructionsUpdate([]agentprompt.Part{branch}, nil); got.update != want {
		t.Errorf("update =\n%s\n\nwant the branch convention alone:\n%s", got.update, want)
	}
}

// TestSDKResume_AnUnreadableTextRecordsNothing: a resume that cannot read what
// the conversation was frozen with sends its own composition and writes
// nothing, so a transient read cannot replace the launch's text.
func TestSDKResume_AnUnreadableTextRecordsNothing(t *testing.T) {
	f := newLaunchFixture(t, "sdk-textdown")
	f.s.conversations = storeReadDown{ConversationStore: f.s.conversations, system: true}
	got := f.s.resumeSystemClaim(context.Background(), runmode.LocalDefaultOrgID, &f.conv, f.task(t))
	if got.stamp || got.update != "" {
		t.Errorf("claim = %+v, want no record and no update", got)
	}
	if got.send.Prompt != agentprompt.Build(sdkSpec(surfaceFor(f.conv.Type))) {
		t.Error("the degraded resume does not lead with the framework blocks")
	}
}

// TestComposeLaunchTurn_AResumedSessionCarriesTheUpdate: an SDK launch that
// resumes a session ends its message with the update and records it as a
// system row whose parts the next claim compares against.
func TestComposeLaunchTurn_AResumedSessionCarriesTheUpdate(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-update", "", "/tmp/wt-sdk-update")
	claimID := markEngaged(t, database, "r-sdk-update")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-update", claimID, "event", runmode.LocalDefaultUserID)
	ctx := context.Background()

	parts := composeSystemParts(pinnedInputs("after-the-park/<ticket-id>", "github"))
	update := launchUpdate{
		text:  agentprompt.InstructionsUpdate([]agentprompt.Part{partByKey(t, parts, partBranch)}, nil),
		parts: parts,
	}
	resumed := agentproc.RunOptions{SessionID: "sess-survived"}
	if err := s.composeLaunchTurn(ctx, &resumed, sink, runmode.LocalDefaultOrgID, "r-sdk-update",
		runmode.LocalDefaultUserID, "", "", update, false, openingTestMemories(), openingTestTaskContext); err != nil {
		t.Fatalf("the resuming launch: %v", err)
	}
	if !strings.HasSuffix(resumed.Message, update.text) {
		t.Errorf("the turn's message does not end with the update;\n%s", resumed.Message)
	}

	var row *domain.Message
	rows := allRows(t, s, "r-sdk-update")
	for i := range rows {
		if rows[i].Subtype == domain.MessageSubtypeInjectionSystemUpdate {
			row = &rows[i]
		}
	}
	if row == nil || row.Role != "system" || row.Content != update.text {
		t.Fatalf("no system row recorded the update: %+v", row)
	}
	if told := lastToldParts(rows); len(told) != len(parts) || told[len(told)-1] != parts[len(parts)-1] {
		t.Errorf("the recorded parts do not read back as what the conversation was told")
	}

	// The turn never reaches the agent: the update was not told, so the next
	// claim must not count it.
	if resumed.OnOpeningUnsent == nil {
		t.Fatal("the launch carries no hook to take the update back")
	}
	resumed.OnOpeningUnsent()
	if told := lastToldParts(allRows(t, s, "r-sdk-update")); told != nil {
		t.Errorf("an update whose turn never reached the agent still reads as told: %+v", told)
	}
}

// storeReadDown is a conversation store with one read broken and the rest of
// it intact: the shape of a transient failure on a read that decides what a
// claim sends.
type storeReadDown struct {
	db.ConversationStore
	opening bool
	system  bool
}

func (d storeReadDown) OpeningTaskContextSystem(ctx context.Context, orgID, conversationID string) (string, bool, error) {
	if d.opening {
		return "", false, errors.New("read opening: connection reset")
	}
	return d.ConversationStore.OpeningTaskContextSystem(ctx, orgID, conversationID)
}

func (d storeReadDown) SystemTextSystem(ctx context.Context, orgID, conversationID string) (domain.SystemText, error) {
	if d.system {
		return domain.SystemText{}, errors.New("read system text: connection reset")
	}
	return d.ConversationStore.SystemTextSystem(ctx, orgID, conversationID)
}

// runNative drives one native engagement over the fixture's claim. Its tool host
// cannot launch in a test, so what it exercises is everything ahead of that — the
// staging, the system-text decision and the stamp — and the launchErr it returns
// is the expected end of the call rather than a failure of the test.
func (f *launchFixture) runNative(t *testing.T, mission string) engagementDisposition {
	t.Helper()
	cfg := f.cfg()
	cfg.claimID = f.conv.ClaimID
	cfg.blueprintRunID = f.br.ID
	cfg.runRoot = f.worktree
	return f.s.runNativeAgent(context.Background(), f.conv.ID, f.task(t), mission, cfg,
		time.Now(), "claude-sonnet-4-6", "manual", runmode.LocalDefaultUserID)
}

func (f *launchFixture) cfg() runConfig {
	return runConfig{
		orgID:   runmode.LocalDefaultOrgID,
		teamID:  runmode.LocalDefaultTeamID,
		surface: agentprompt.SurfaceMachinist,
		sources: sourceDocs{kinds: []string{"github"}},
	}
}

// nativeLaunchText resolves one claim's launch text the way runNativeAgent
// does, for the conversation the fixture claimed, with what the claim
// inherited handed in.
func (f *launchFixture) nativeLaunchText(t *testing.T, mission string, replay claimReplay) nativeLaunchText {
	t.Helper()
	text, err := f.s.buildNativeLaunchText(context.Background(), f.conv.ID, f.task(t), mission, f.cfg(), replay)
	if err != nil {
		t.Fatalf("compose the launch text: %v", err)
	}
	return text
}

func (f *launchFixture) task(t *testing.T) domain.Task {
	t.Helper()
	task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load the fixture task: (%v, %v)", task, err)
	}
	return *task
}

func (f *launchFixture) storedSystemText(t *testing.T) domain.SystemText {
	t.Helper()
	text, err := f.stores.Conversations.SystemTextSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID)
	if err != nil {
		t.Fatalf("SystemTextSystem: %v", err)
	}
	return text
}

// setBranchTemplate rewrites the team's branch-naming convention, the live
// input a test moves to watch a claim hold still and tell the conversation.
// Read-modify-write, because the settings upsert writes the whole row.
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
