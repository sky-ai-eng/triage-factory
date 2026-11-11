package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

// Warm claims for the native runtime, at the fleet level: executor X holds the
// engagement, a control pod with no process sends the messages and the stops,
// and the doorbell between them is the real signal outbox, applied by X's own
// apply loop. The engagement is the real tail of a native engagement past
// agent-live (runNativeTail) driving the real loop against a scripted
// provider, with a stand-in for the jail that only records when it is stopped.

// warmModel is a model the loop knows the window and the price of.
const warmModel = "claude-sonnet-4-5"

// warmProvider answers each call with the next scripted turn and records what
// every call was sent.
type warmProvider struct {
	mu       sync.Mutex
	turns    []gateTurn
	requests []inference.Request
}

func (p *warmProvider) Stream(ctx context.Context, req inference.Request) (*inference.Completion, error) {
	p.mu.Lock()
	n := len(p.requests)
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if n >= len(p.turns) {
		return nil, errors.New("warm provider ran out of turns")
	}
	return (&scriptedGateProvider{turns: p.turns[n : n+1]}).Stream(ctx, req)
}

func (p *warmProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// sent is everything the call at index i carried in its user rows.
func (p *warmProvider) sent(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, r := range p.requests[i].Rows {
		if r.Role == "user" {
			b.WriteString(r.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// warmEngagement is X's engagement on the fixture's conversation.
type warmEngagement struct {
	*parkFleet
	claim    *domain.Conversation
	provider *warmProvider
	// cancel cancels the engagement's context with a cause, the way a stop, a
	// fence and a shutdown each do.
	cancel context.CancelCauseFunc
	// jailStopped is set when the engagement stops its jail, which every
	// ending does first.
	jailStopped atomic.Bool
	done        chan engagementDisposition

	mu sync.Mutex
	// runs are each turn's Run, in order: what it ran under and what it
	// returned.
	runs []warmRun
}

type warmRun struct {
	params agentloop.Params
	result agentloop.Result
}

// warmOpts shapes one scenario.
type warmOpts struct {
	window time.Duration
	// blueprintRunning leaves the step's blueprint run `running`. Otherwise it
	// is completed, as it is for a conversation a person writes to after its
	// blueprint finished.
	blueprintRunning bool
	// noPersonMessage starts the engagement with nothing a person wrote.
	noPersonMessage bool
	// beforeStart runs once the claim is held, before the first turn.
	beforeStart func(f *warmEngagement)
	turns       []gateTurn
}

// startWarmEngagement claims the fixture's conversation on X and runs its
// tail in a goroutine the fixture joins.
func startWarmEngagement(t *testing.T, o warmOpts) *warmEngagement {
	t.Helper()
	pf := seedParkFleet(t)
	ctx := context.Background()
	pf.x.SetWarmWindow(o.window)

	claim := pf.claim(t, pf.x)
	if claim == nil {
		t.Fatal("executor X could not claim the fixture's conversation")
	}
	if claim.Runtime != domain.ConversationRuntimeNative {
		t.Fatalf("the fixture's conversation runs %q, want native", claim.Runtime)
	}
	if !o.blueprintRunning {
		pgtest.MustExec(t, pf.h.AdminDB, `UPDATE blueprint_runs SET status = 'completed', completed_at = now() WHERE id = $1`, pf.brID)
	}
	if !o.noPersonMessage {
		if _, err := pf.stores.Conversations.InsertMessageSystem(ctx, pf.orgID, pendingUserInput(claim.ID, pf.userID, "please fix the flaky test")); err != nil {
			t.Fatalf("queue the person's message: %v", err)
		}
	}
	task, err := pf.stores.Tasks.GetSystem(ctx, pf.orgID, pf.taskID)
	if err != nil || task == nil {
		t.Fatalf("load task: %v", err)
	}

	f := &warmEngagement{parkFleet: pf, claim: claim, provider: &warmProvider{turns: o.turns}, done: make(chan engagementDisposition, 1)}
	engCtx, cancel := context.WithCancelCause(context.Background())
	f.cancel = cancel
	pf.x.mu.Lock()
	pf.x.cancels[claim.ID] = func() { cancel(errStopRequested) }
	pf.x.mu.Unlock()
	t.Cleanup(pf.x.startActivityTracker(claim, cancel))

	// X's signal apply loop, which is what answers the control pod's doorbell
	// and its stops. Its stop is registered before anything below can fail
	// the test, and runs after the engagement's below has ended it.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	t.Cleanup(stopLoop)
	pf.spawn(func() { pf.x.ConversationSignalApplyLoop(loopCtx, 20*time.Millisecond) })

	if o.beforeStart != nil {
		o.beforeStart(f)
	}

	ckpt := pf.x.startCheckpointer(engCtx, pf.orgID, claim.ID, pf.keyID, claim.ClaimID, pf.wtPath)
	transcript := newNativeTranscript(pf.x, pf.orgID, claim.ID, claim.ClaimID)
	timings := pf.x.resolvedActivityTimings()
	engine := &agentloop.Engine{
		Transcript:  transcript,
		Credentials: staticGateCredentials{provider: f.provider},
		Tools:       &recordingToolHost{},
		Hooks:       checkpointHooks(ckpt, agentloop.Hooks{}),
		Activity:    engineActivity{pf.x.activityFor(claim.ID)},
		ActivityBounds: agentloop.ActivityBounds{
			Provider: timings.providerByte,
			Tool:     timings.toolCall,
		},
		Retry: agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	}
	tail := nativeTail{
		orgID:          pf.orgID,
		conversationID: claim.ID,
		task:           *task,
		cfg:            runConfig{orgID: pf.orgID, teamID: pf.teamID, claimID: claim.ClaimID, blueprintRunID: pf.brID},
		namespace:      pf.keyID,
		claudeCwd:      pf.wtPath,
		creatorUserID:  pf.userID,
		startTime:      time.Now(),
		run: func(ctx context.Context, p agentloop.Params) agentloop.Result {
			r := engine.Run(ctx, p)
			f.mu.Lock()
			f.runs = append(f.runs, warmRun{params: p, result: r})
			f.mu.Unlock()
			return r
		},
		params: agentloop.Params{
			OrgID:          pf.orgID,
			ConversationID: claim.ID,
			ClaimID:        claim.ClaimID,
			Model:          warmModel,
			SystemPrompt:   "system",
			HasBlueprint:   true,
			UserID:         pf.userID,
			Workspace:      domain.WorkspaceProvenanceWarm,
			NewSandbox:     true,
		},
		transcript: transcript,
		ckpt:       ckpt,
		release:    func() { f.jailStopped.Store(true) },
	}

	// Registered after the fixture's own cleanups, so it runs first: an
	// engagement still waiting when the scenario ends is stopped, and its park
	// lands before the fixture joins its goroutines.
	t.Cleanup(func() {
		cancel(errStopRequested)
		pf.gate.unhold(false)
		select {
		case <-f.done:
		case <-time.After(30 * time.Second):
			t.Error("the engagement did not end within 30s of the test stopping it")
		}
		ckpt.stop()
	})
	pf.spawn(func() {
		f.done <- pf.x.runNativeTail(engCtx, tail)
		close(f.done)
	})
	return f
}

// await waits for the engagement to end and returns its disposition.
func (f *warmEngagement) await(t *testing.T, within time.Duration) engagementDisposition {
	t.Helper()
	select {
	case d := <-f.done:
		return d
	case <-time.After(within):
		t.Fatalf("the engagement did not let go within %s", within)
		return engagementDisposition{}
	}
}

// awaitWaiting waits until the engagement is waiting warm after its n-th turn.
func (f *warmEngagement) awaitWaiting(t *testing.T, turns int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f.provider.calls() == turns && f.claimPhase(t) == domain.ClaimPhaseAwaitingMessage && f.x.warmWaits.Len() == 1 {
			return
		}
		select {
		case d := <-f.done:
			t.Fatalf("the engagement let go (%+v) instead of waiting after turn %d", d, turns)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engagement is not waiting after turn %d: calls %d, phase %q, registered %d",
				turns, f.provider.calls(), f.claimPhase(t), f.x.warmWaits.Len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitCalls waits until the provider has been called n times.
func (f *warmEngagement) awaitCalls(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for f.provider.calls() < n {
		if time.Now().After(deadline) {
			t.Fatalf("provider calls = %d after %s, want %d", f.provider.calls(), within, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *warmEngagement) claimPhase(t *testing.T) string {
	t.Helper()
	var phase string
	if err := f.h.AdminDB.QueryRow(`SELECT COALESCE(phase, '') FROM claims WHERE id = $1`, f.claim.ClaimID).Scan(&phase); err != nil {
		t.Fatalf("read claim phase: %v", err)
	}
	return phase
}

// claimRelease is the engagement's claim as its release left it.
func (f *warmEngagement) claimRelease(t *testing.T) (released bool, outcome string, durationMs int64) {
	t.Helper()
	if err := f.h.AdminDB.QueryRow(
		`SELECT released_at IS NOT NULL, COALESCE(outcome, ''), COALESCE(duration_ms, 0) FROM claims WHERE id = $1`, f.claim.ClaimID,
	).Scan(&released, &outcome, &durationMs); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	return released, outcome, durationMs
}

func (f *warmEngagement) claimRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.AdminDB.QueryRow(`SELECT count(*) FROM claims WHERE conversation_id = $1`, f.conversationID).Scan(&n); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	return n
}

func (f *warmEngagement) stopNotes(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.AdminDB.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id = $1 AND subtype = $2`,
		f.conversationID, domain.MessageSubtypeStopNote).Scan(&n); err != nil {
		t.Fatalf("count stop notes: %v", err)
	}
	return n
}

func (f *warmEngagement) run(i int) warmRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[i]
}

// assertTurnEndWrites checks what a natural turn end writes when its
// engagement lets go: the conversation parked `open` with the turn's verdict,
// the claim released by that write, and a snapshot of the tree on the record.
// The scenarios that let go at once and the one that lets go at expiry assert
// through this one helper, which is the claim that the two write the same.
func (f *warmEngagement) assertTurnEndWrites(t *testing.T, outcome domain.ConversationOutcome, summary string) {
	t.Helper()
	conv := f.read(t)
	if conv.Status != domain.StatusOpen || !conv.Concluded() || conv.Outcome != string(outcome) || conv.ResultSummary != summary || conv.ParkReason != "" {
		t.Errorf("conversation = {status %q, concluded %v, outcome %q, summary %q, park %q}, want {open, true, %q, %q, none}",
			conv.Status, conv.Concluded(), conv.Outcome, conv.ResultSummary, conv.ParkReason, outcome, summary)
	}
	if released, claimOutcome, _ := f.claimRelease(t); !released || claimOutcome != "completed" {
		t.Errorf("claim = {released %v, outcome %q}, want released by the verdict's write (completed)", released, claimOutcome)
	}
	if !f.jailStopped.Load() {
		t.Error("the engagement let go without stopping its jail")
	}
	if !f.blobPresent(t) {
		t.Error("no snapshot of the tree in the blob store")
	}
	f.assertState(t, domain.WorkspaceSnapshotWritten, f.claim.ClaimID)
}

// After a person's message and a natural turn end, the engagement holds its
// claim and its jail, and shows as awaiting a message.
func TestWarmClaim_WaitsAfterAPersonsTurn(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Minute, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	if got := f.read(t).Status; got != domain.ClaimPhaseAwaitingMessage {
		t.Errorf("displayed status = %q, want %q", got, domain.ClaimPhaseAwaitingMessage)
	}
	if f.jailStopped.Load() {
		t.Error("the jail was stopped while the engagement waits")
	}
	if released, _, _ := f.claimRelease(t); released {
		t.Error("the claim was released while the engagement waits")
	}
	if conv := f.read(t); conv.Concluded() || conv.Outcome != "" {
		t.Errorf("the turn's verdict was written while the engagement waits: concluded %v, outcome %q", conv.Concluded(), conv.Outcome)
	}
}

// A second message within the window starts a turn on the same claim, in the
// same sandbox.
func TestWarmClaim_SecondMessageRunsOnTheSameClaim(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Minute, turns: []gateTurn{{text: "fixed it"}, {text: "also updated the README"}}})
	f.awaitWaiting(t, 1)

	if err := f.followUp("now update the README too"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWaiting(t, 2)

	if n := f.claimRows(t); n != 1 {
		t.Errorf("claim rows = %d, want the one claim the first turn ran on", n)
	}
	if f.jailStopped.Load() {
		t.Error("the jail was stopped between the turns")
	}
	if !strings.Contains(f.provider.sent(1), "now update the README too") {
		t.Errorf("the second turn was not sent the message:\n%s", f.provider.sent(1))
	}
	second := f.run(1).params
	if second.NewSandbox || second.Workspace != domain.WorkspaceProvenanceUnknown || second.WorkspaceAsOf != nil || second.ExecutorChanged {
		t.Errorf("the second turn claims a new start: NewSandbox %v, Workspace %q, WorkspaceAsOf %v, ExecutorChanged %v",
			second.NewSandbox, second.Workspace, second.WorkspaceAsOf, second.ExecutorChanged)
	}
	if second.ClaimID != f.claim.ClaimID {
		t.Errorf("the second turn ran under claim %s, want %s", second.ClaimID, f.claim.ClaimID)
	}
}

// At expiry the engagement lets go with the turn-end writes it would have
// made without a window.
func TestWarmClaim_ExpiryLetsGoWithTheTurnEndWrites(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: 2 * time.Second, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	if d := f.await(t, 20*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition at expiry = %+v, want the ordinary ending", d)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
	if n := f.x.warmWaits.Len(); n != 0 {
		t.Errorf("registered waits after the let-go = %d, want none", n)
	}
}

// letsGoAtOnce runs a scenario whose turn end must not open a window, with a
// window long enough that waiting through it would fail the test.
func letsGoAtOnce(t *testing.T, o warmOpts) *warmEngagement {
	t.Helper()
	o.window = time.Hour
	f := startWarmEngagement(t, o)
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	return f
}

// A stop_blueprint verdict ends the work it names, so it never opens a window.
func TestWarmClaim_StopBlueprintVerdictLetsGoAtOnce(t *testing.T) {
	f := letsGoAtOnce(t, warmOpts{turns: []gateTurn{{calls: []domain.ToolCall{{ID: "c1", Name: agentloop.ToolStopBlueprint, Input: map[string]any{
		"type": "finish", "reason": "nothing to change", "summary": "The test was already fixed upstream.",
	}}}}}})
	f.assertTurnEndWrites(t, domain.ConversationOutcomeFinish, "The test was already fixed upstream.")
}

// A turn under a running blueprint is one the reactor advances or finishes,
// so it never opens a window.
func TestWarmClaim_RunningBlueprintLetsGoAtOnce(t *testing.T) {
	f := letsGoAtOnce(t, warmOpts{blueprintRunning: true, turns: []gateTurn{{text: "fixed it"}}})
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
}

// A turn no person took part in never opens a window, even with an injection
// read on it.
func TestWarmClaim_TurnWithNoPersonsMessageLetsGoAtOnce(t *testing.T) {
	f := letsGoAtOnce(t, warmOpts{noPersonMessage: true, turns: []gateTurn{{text: "looked at the event"}},
		beforeStart: func(f *warmEngagement) {
			if _, ok := f.control.stageInjection(context.Background(), f.orgID, f.conversationID, domain.MessageSubtypeInjectionEvent,
				"test", "the CI check failed again", domain.NoteProvenance{}); !ok {
				t.Fatal("stage the event injection")
			}
		}})
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "looked at the event")
}

// A turn-end checkpoint that does not land lets the engagement go at once.
func TestWarmClaim_FailedTurnEndCheckpointLetsGoAtOnce(t *testing.T) {
	var failed atomic.Bool
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}},
		beforeStart: func(f *warmEngagement) {
			// The turn-end checkpoint's upload is the first one the
			// engagement makes. It is held, then failed; the let-go's own
			// snapshot after it lands.
			f.gate.hold()
			f.spawn(func() {
				select {
				case <-f.gate.entered:
					failed.Store(true)
					f.gate.abort()
				case <-time.After(15 * time.Second):
				}
			})
		}})
	if d := f.await(t, 20*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	if !failed.Load() {
		t.Fatal("the turn-end checkpoint never uploaded, so it was not what failed")
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
}

// A stop during the wait is the stopped park, and the stop records no note:
// the turn had ended, so nothing was interrupted.
func TestWarmClaim_StopDuringTheWaitParksStopped(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	if err := f.control.Stop(f.orgID, f.conversationID, f.userID); err != nil {
		t.Fatalf("stop from the control pod: %v", err)
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the stopped park", d)
	}
	conv := f.read(t)
	if conv.Status != domain.StatusOpen || conv.ParkReason != domain.ParkReasonUserCancelled || conv.Concluded() {
		t.Errorf("conversation = {status %q, park %q, concluded %v}, want {open, user_cancelled, false}", conv.Status, conv.ParkReason, conv.Concluded())
	}
	if released, outcome, _ := f.claimRelease(t); !released || outcome != "cancelled" {
		t.Errorf("claim = {released %v, outcome %q}, want released cancelled", released, outcome)
	}
	if n := f.stopNotes(t); n != 0 {
		t.Errorf("stop notes = %d, want none for an engagement that was waiting", n)
	}
}

// A shutdown during the wait lets go with the turn's verdict. It never hands
// the claim back, which would leave the conversation claimable with nothing
// for the next engagement to answer.
func TestWarmClaim_ShutdownDuringTheWaitLetsGo(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	f.cancel(errDispatcherShutdown)
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the let-go, not a hand-back", d)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
}

// A stop or a shutdown that cancels the turn-end checkpoint reaches a turn that
// has already concluded. The conclusion is recorded with a snapshot of the
// tree, not on the cancelled context, where the snapshot would fail.
func TestWarmClaim_CancelDuringTheTurnEndCheckpointRecordsTheConclusion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(f *warmEngagement)
	}{
		{"stop", func(f *warmEngagement) {
			if err := f.control.Stop(f.orgID, f.conversationID, f.userID); err != nil {
				t.Fatalf("stop from the control pod: %v", err)
			}
		}},
		{"shutdown", func(f *warmEngagement) { f.cancel(errDispatcherShutdown) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}},
				beforeStart: func(f *warmEngagement) { f.gate.hold() }})
			select {
			case <-f.gate.entered:
			case <-time.After(15 * time.Second):
				t.Fatal("the turn-end checkpoint never uploaded")
			}
			tc.cancel(f)
			if d := f.await(t, 20*time.Second); d.fenced || d.handedBack {
				t.Fatalf("disposition = %+v, want the conclusion recorded", d)
			}
			f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
		})
	}
}

// A lease fence during the wait writes nothing: the claim is someone else's to
// release, and the conversation keeps no verdict from this engagement.
func TestWarmClaim_LeaseFenceDuringTheWaitWritesNothing(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	f.cancel(errClaimLeaseLost)
	if d := f.await(t, 15*time.Second); !d.fenced {
		t.Fatalf("disposition = %+v, want fenced", d)
	}
	if conv := f.read(t); conv.Concluded() || conv.Outcome != "" {
		t.Errorf("the fenced engagement wrote a verdict: concluded %v, outcome %q", conv.Concluded(), conv.Outcome)
	}
	if released, _, _ := f.claimRelease(t); released {
		t.Error("the fenced engagement released the claim")
	}
	if !f.jailStopped.Load() {
		t.Error("the fenced engagement left its jail running")
	}
	if n := f.x.warmWaits.Len(); n != 0 {
		t.Errorf("registered waits after the fence = %d, want none", n)
	}
}

// The registry letting a waiting engagement go, as reclaiming its slot does,
// ends the wait at once with the turn-end writes.
func TestWarmClaim_RegistryLetGoEndsTheWaitAtOnce(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWaiting(t, 1)

	if !f.x.warmWaits.LetGoOldest() {
		t.Fatal("the registry held no waiting engagement to let go")
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed it")
}

// An event the router folds into the conversation during the wait starts a
// turn at once: the event replaced the run its trigger would have fired, so it
// cannot wait for a message nobody may send. The router rings the engagement
// the way a follow-up does. A turn an event started opens no window of its
// own, since no person took part in it, and lets go when it ends.
func TestWarmClaim_EventInjectionWakesTheWait(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}, {text: "fixed the new failure"}}})
	f.awaitWaiting(t, 1)

	const event = "the CI check failed again on the main branch"
	if got := f.control.StageOrDeliverAdditiveEvent(context.Background(), f.orgID, f.conversationID, "test", event,
		domain.NoteProvenance{}, AdditiveFiringRef{}); got != InjectStagedResumable {
		t.Fatalf("StageOrDeliverAdditiveEvent = %v, want the event staged for the waiting engagement", got)
	}
	f.awaitCalls(t, 2, warmWaitBackstop/3)
	if !strings.Contains(f.provider.sent(1), event) {
		t.Errorf("the turn the event started was not sent it:\n%s", f.provider.sent(1))
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed the new failure")
}

// Input that is waiting when the window expires, with no ring to say so, is
// read rather than concluded over. The event here is staged straight onto the
// transcript, which rings nothing, and the backstop poll is far longer than
// the window.
func TestWarmClaim_ExpiryReadsInputItWasNeverRungFor(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: 2 * time.Second, turns: []gateTurn{{text: "fixed it"}, {text: "fixed the new failure"}}})
	f.awaitWaiting(t, 1)

	const event = "the CI check failed again on the main branch"
	if _, ok := f.control.stageInjection(context.Background(), f.orgID, f.conversationID, domain.MessageSubtypeInjectionEvent,
		"test", event, domain.NoteProvenance{}); !ok {
		t.Fatal("stage the event injection")
	}
	f.awaitCalls(t, 2, 10*time.Second)
	if !strings.Contains(f.provider.sent(1), event) {
		t.Errorf("the turn at expiry was not sent the event:\n%s", f.provider.sent(1))
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "fixed the new failure")
}

// A note that is information rather than work does not start a turn, even with
// the doorbell rung. It reaches the model with the person's next message.
func TestWarmClaim_InformationalNoteWaitsForTheNextTurn(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}, {text: "saw both"}}})
	f.awaitWaiting(t, 1)

	const note = "the pull request's base branch moved"
	f.control.StageOrDeliverInformationalEvent(context.Background(), f.orgID, f.conversationID, "test", note, domain.NoteProvenance{})
	f.x.warmWaits.Wake(f.conversationID)
	time.Sleep(300 * time.Millisecond)
	if n := f.provider.calls(); n != 1 {
		t.Fatalf("provider calls after the note = %d, want 1: a note alone must not start a turn", n)
	}
	if phase := f.claimPhase(t); phase != domain.ClaimPhaseAwaitingMessage {
		t.Fatalf("claim phase after the note = %q, want still awaiting a message", phase)
	}

	if err := f.followUp("and check the other job"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWaiting(t, 2)
	sent := f.provider.sent(1)
	if !strings.Contains(sent, note) || !strings.Contains(sent, "and check the other job") {
		t.Errorf("the person's turn was not sent both the note and the message:\n%s", sent)
	}
}

// A message sent while the engagement is between its turn end and its
// registration, so its doorbell rings nothing, still starts a turn, well before
// the backstop poll would find it.
func TestWarmClaim_MessageWhileEnteringTheWaitIsNotLost(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}, {text: "done that too"}},
		beforeStart: func(f *warmEngagement) { f.gate.hold() }})

	// The turn-end checkpoint's upload is held, so the engagement has ended its
	// turn and has not registered.
	select {
	case <-f.gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the turn-end checkpoint never uploaded")
	}
	if n := f.x.warmWaits.Len(); n != 0 {
		t.Fatalf("registered waits during the checkpoint = %d, want none yet", n)
	}
	if err := f.followUp("one more thing"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitSignalsAcked(t)
	f.gate.let()

	f.awaitCalls(t, 2, warmWaitBackstop/3)
	if !strings.Contains(f.provider.sent(1), "one more thing") {
		t.Errorf("the second turn was not sent the message:\n%s", f.provider.sent(1))
	}
}

// awaitSignalsAcked waits until X's apply loop has answered every signal sent
// to it.
func (f *warmEngagement) awaitSignalsAcked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var unacked int
		if err := f.h.AdminDB.QueryRow(`SELECT count(*) FROM conversation_signals WHERE conversation_id = $1 AND acked_at IS NULL`,
			f.conversationID).Scan(&unacked); err != nil {
			t.Fatalf("count unacked signals: %v", err)
		}
		if unacked == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d signals still unacked after 5s", unacked)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claim's duration is the time its turns ran, not the time it held the
// claim.
func TestWarmClaim_DurationIsTheTurnsNotTheWait(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: 3 * time.Second, turns: []gateTurn{{text: "fixed it"}, {text: "done that too"}}})
	f.awaitWaiting(t, 1)
	const waited = time.Second
	time.Sleep(waited)
	if err := f.followUp("one more thing"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWaiting(t, 2)
	if d := f.await(t, 20*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}

	turns := int64(f.run(0).result.DurationMs + f.run(1).result.DurationMs)
	_, _, durationMs := f.claimRelease(t)
	if durationMs != turns {
		t.Errorf("claims.duration_ms = %d, want the two turns' %d", durationMs, turns)
	}
	if durationMs >= waited.Milliseconds() {
		t.Errorf("claims.duration_ms = %d, which counts the %s the engagement waited", durationMs, waited)
	}
	f.assertTurnEndWrites(t, domain.ConversationOutcomeContinue, "done that too")
}
