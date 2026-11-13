package delegate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// Warm claims for the native runtime, at the fleet level: executor X holds the
// engagement, a control pod with no process sends the messages and the stops,
// and the doorbell between them is the real signal outbox, applied by X's own
// apply loop. The engagement is the real tail of a native engagement past
// agent-live (runNativeTail) driving the real loop against a scripted
// provider, with a stand-in for the jail that only records when it is stopped.
//
// What every scenario holds to: a warm claim changes nothing about the
// conversation. Its turn writes what a turn writes, and a person resumes it
// the way a person resumes any conversation.

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
	// teamDefault is the team's default model, which every claim on the
	// conversation after its blueprint finished runs on. It starts as the
	// engagement's.
	teamDefault string
}

// setTeamDefault changes the team's default model, as an admin's settings
// save does.
func (f *warmEngagement) setTeamDefault(model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teamDefault = model
}

func (f *warmEngagement) teamModels(context.Context, string, string) (domain.TeamModels, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.NewTeamModels(f.teamDefault, domain.ModelSet{}), nil
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

	f := &warmEngagement{parkFleet: pf, claim: claim, provider: &warmProvider{turns: o.turns}, done: make(chan engagementDisposition, 1), teamDefault: warmModel}
	for _, s := range []*Spawner{pf.x, pf.control} {
		s.SetRunCredentialResolvers(nil, nil, f.teamModels)
	}
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

// awaitWarm waits until the engagement keeps its claim warm after its n-th
// turn.
func (f *warmEngagement) awaitWarm(t *testing.T, turns int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f.provider.calls() == turns && f.claimPhase(t) == domain.ClaimPhaseWarm && f.x.warmWaits.Len() == 1 {
			return
		}
		select {
		case d := <-f.done:
			t.Fatalf("the engagement let go (%+v) instead of keeping its claim warm after turn %d", d, turns)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claim is not warm after turn %d: calls %d, phase %q, registered %d",
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

func (f *warmEngagement) run(i int) warmRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[i]
}

// turnEndState is what a turn end leaves on the conversation: a reading
// compared before and after a warm claim lets go, which must not move it.
type turnEndState struct {
	status, outcome, summary, parkReason string
	completedAt                          time.Time
}

func (f *warmEngagement) turnEndState(t *testing.T) turnEndState {
	t.Helper()
	conv := f.read(t)
	var at time.Time
	if conv.CompletedAt != nil {
		at = *conv.CompletedAt
	}
	return turnEndState{status: conv.Status, outcome: conv.Outcome, summary: conv.ResultSummary, parkReason: string(conv.ParkReason), completedAt: at}
}

// assertConcluded checks the turn-end state a turn writes under a running
// blueprint: the conversation `open` with the step's verdict, and a snapshot
// of the tree on the record under this engagement's claim.
func (f *warmEngagement) assertConcluded(t *testing.T, outcome domain.ConversationOutcome, summary string) {
	t.Helper()
	conv := f.read(t)
	if conv.Status != domain.StatusOpen || !conv.Concluded() || conv.Outcome != string(outcome) || conv.ResultSummary != summary || conv.ParkReason != "" {
		t.Errorf("conversation = {status %q, concluded %v, outcome %q, summary %q, park %q}, want {open, true, %q, %q, none}",
			conv.Status, conv.Concluded(), conv.Outcome, conv.ResultSummary, conv.ParkReason, outcome, summary)
	}
	if !f.blobPresent(t) {
		t.Error("no snapshot of the tree in the blob store")
	}
	f.assertState(t, domain.WorkspaceSnapshotWritten, f.claim.ClaimID)
}

// assertTurnEnded checks the turn-end state a turn writes when no running
// blueprint waits on it, as on the conversation a person writes to after its
// blueprint finished: `open` with the turn's summary and no verdict, so the
// conversation stays live for the next message, and a snapshot of the tree on
// the record under this engagement's claim.
func (f *warmEngagement) assertTurnEnded(t *testing.T, summary string) {
	t.Helper()
	conv := f.read(t)
	if conv.Status != domain.StatusOpen || conv.Concluded() || conv.Outcome != "" || conv.OutcomeReason != "" || conv.ResultSummary != summary || conv.ParkReason != "" {
		t.Errorf("conversation = {status %q, concluded %v, outcome %q, reason %q, summary %q, park %q}, want {open, false, none, none, %q, none}",
			conv.Status, conv.Concluded(), conv.Outcome, conv.OutcomeReason, conv.ResultSummary, conv.ParkReason, summary)
	}
	if !f.blobPresent(t) {
		t.Error("no snapshot of the tree in the blob store")
	}
	f.assertState(t, domain.WorkspaceSnapshotWritten, f.claim.ClaimID)
}

// assertReleased checks that the engagement let go of its claim the way a
// turn end does, with its jail stopped first.
func (f *warmEngagement) assertReleased(t *testing.T) {
	t.Helper()
	if released, outcome, _ := f.claimRelease(t); !released || outcome != "completed" {
		t.Errorf("claim = {released %v, outcome %q}, want released completed", released, outcome)
	}
	if !f.jailStopped.Load() {
		t.Error("the engagement let go without stopping its jail")
	}
}

// A follow-up turn on a finished blueprint ends with no verdict and keeps its
// claim and its jail. The claim is idle, not working: it holds the
// conversation and the tree, and no agent works under it. No run completed,
// so nobody is told one did.
func TestWarmClaim_KeepsTheClaimAfterAPersonsTurn(t *testing.T) {
	hub, captured := capturingHub(t)
	f := startWarmEngagement(t, warmOpts{window: time.Minute, turns: []gateTurn{{text: "fixed it"}},
		beforeStart: func(f *warmEngagement) { f.x.wsHub = hub }})
	f.awaitWarm(t, 1)

	f.assertTurnEnded(t, "fixed it")
	if f.jailStopped.Load() {
		t.Error("the jail was stopped while the claim is warm")
	}
	if released, _, _ := f.claimRelease(t); released {
		t.Error("the claim was released; it should be kept warm")
	}

	ctx := context.Background()
	active, err := f.stores.Conversations.HasActiveClaimForTaskSystem(ctx, f.orgID, f.taskID)
	if err != nil || !active {
		t.Errorf("HasActiveClaimForTaskSystem = (%v, %v), want the warm claim holding the task's tree", active, err)
	}
	engaged, err := f.stores.Conversations.HasEngagedClaimForTaskSystem(ctx, f.orgID, f.taskID)
	if err != nil || engaged {
		t.Errorf("HasEngagedClaimForTaskSystem = (%v, %v), want false: the warm claim is idle between turns", engaged, err)
	}
	live, err := f.stores.Conversations.LiveConversationIDForTaskSystem(ctx, f.orgID, f.taskID)
	if err != nil || live != f.conversationID {
		t.Errorf("the task's live conversation = %q err=%v, want the person's thread %s", live, err, f.conversationID)
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	turnEndBroadcast := false
	for _, e := range captured.events {
		if e.Type == "conversation_update" && e.ConversationID == f.conversationID {
			turnEndBroadcast = true
		}
		if p, ok := e.Data.(toast.Payload); ok && e.Type == "toast" && strings.Contains(p.Body, "completed") {
			t.Errorf("toast %q fired for a turn that completed no run", p.Body)
		}
	}
	if !turnEndBroadcast {
		t.Error("the turn end broadcast nothing, so the hub the toast would have reached was not the engagement's")
	}
}

// With TF_SNAPSHOT_INTERVAL_SEC=0 and a blob store, a person's turn still keeps
// its claim warm: the interval governs only the checkpoints at batch
// boundaries, and the turn-end capture the window waits on is taken anyway.
// The blob it stored is that capture, at the turn's newest row, not a park's.
func TestWarmClaim_ZeroSnapshotIntervalStillKeepsTheClaim(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Minute, turns: []gateTurn{{text: "fixed it"}},
		beforeStart: func(f *warmEngagement) { f.x.SetSnapshotInterval(0) }})
	f.awaitWarm(t, 1)

	f.assertTurnEnded(t, "fixed it")
	if released, _, _ := f.claimRelease(t); released {
		t.Error("the claim was released; it should be kept warm")
	}
	if f.jailStopped.Load() {
		t.Error("the jail was stopped while the claim is warm")
	}

	rows, err := f.stores.Conversations.ListForAssemblySystem(context.Background(), f.orgID, f.conversationID)
	if err != nil {
		t.Fatalf("list transcript: %v", err)
	}
	var answer *float64
	for _, r := range rows {
		if r.Role == "assistant" && r.Content == "fixed it" {
			key := assemblyKey(r)
			answer = &key
		}
	}
	if answer == nil {
		t.Fatal("the turn's answer is not in the transcript")
	}
	man, _ := unpackSnapshotBlob(t, f.blobs, snapshotKey(f.orgID, f.keyID))
	if man.TranscriptPosition == nil || *man.TranscriptPosition != *answer || man.ConversationID != f.conversationID {
		t.Errorf("manifest position = (%v, %q), want (%v, %q): the turn-end capture at the turn's answer",
			man.TranscriptPosition, man.ConversationID, *answer, f.conversationID)
	}
}

// A second message within the window resumes the conversation as any message
// does, and the turn runs on the same claim, in the same sandbox.
func TestWarmClaim_SecondMessageRunsOnTheSameClaim(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Minute, turns: []gateTurn{{text: "fixed it"}, {text: "also updated the README"}}})
	f.awaitWarm(t, 1)

	if err := f.followUp("now update the README too"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWarm(t, 2)

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
	f.assertTurnEnded(t, "also updated the README")
}

// At expiry the claim is let go and the conversation is not touched: it has
// had its turn-end state since the turn ended.
func TestWarmClaim_ExpiryReleasesTheClaimAndNothingElse(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: 2 * time.Second, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)
	before := f.turnEndState(t)

	if d := f.await(t, 20*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition at expiry = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	if after := f.turnEndState(t); after != before {
		t.Errorf("conversation moved when the claim was let go:\nbefore %+v\nafter  %+v", before, after)
	}
	if n := f.x.warmWaits.Len(); n != 0 {
		t.Errorf("registered waits after the let-go = %d, want none", n)
	}
}

// endsAtOnce runs a scenario whose turn end must not keep the claim, with a
// window long enough that keeping it would fail the test.
func endsAtOnce(t *testing.T, o warmOpts) *warmEngagement {
	t.Helper()
	o.window = time.Hour
	f := startWarmEngagement(t, o)
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	return f
}

// A stop_blueprint call ends the turn at once, so it never keeps a claim. On
// a blueprint that has already finished there is no run for it to stop, and
// the turn records no verdict, as any other turn there does.
func TestWarmClaim_StopBlueprintVerdictEndsAtOnce(t *testing.T) {
	f := endsAtOnce(t, warmOpts{turns: []gateTurn{{calls: []domain.ToolCall{{ID: "c1", Name: agentloop.ToolStopBlueprint, Input: map[string]any{
		"type": "finish", "reason": "nothing to change", "summary": "The test was already fixed upstream.",
	}}}}}})
	f.assertTurnEnded(t, "The test was already fixed upstream.")
}

// A turn under a running blueprint is one the reactor advances or finishes,
// so it never keeps a claim.
func TestWarmClaim_RunningBlueprintEndsAtOnce(t *testing.T) {
	f := endsAtOnce(t, warmOpts{blueprintRunning: true, turns: []gateTurn{{text: "fixed it"}}})
	f.assertConcluded(t, domain.ConversationOutcomeContinue, "fixed it")
}

// A turn no person took part in never keeps a claim.
func TestWarmClaim_TurnWithNoPersonsMessageEndsAtOnce(t *testing.T) {
	f := endsAtOnce(t, warmOpts{noPersonMessage: true, turns: []gateTurn{{text: "looked at the event"}},
		beforeStart: func(f *warmEngagement) {
			if _, ok := f.control.stageInjection(context.Background(), f.orgID, f.conversationID, domain.MessageSubtypeInjectionEvent,
				"test", "the CI check failed again", domain.NoteProvenance{}); !ok {
				t.Fatal("stage the event injection")
			}
		}})
	f.assertTurnEnded(t, "looked at the event")
}

// A turn-end snapshot that does not land keeps no claim, and the turn ends as
// it always has, with a snapshot of its own.
func TestWarmClaim_FailedTurnEndSnapshotEndsAtOnce(t *testing.T) {
	var failed atomic.Bool
	f := endsAtOnce(t, warmOpts{turns: []gateTurn{{text: "fixed it"}},
		beforeStart: func(f *warmEngagement) {
			// The turn-end snapshot's upload is the first one the engagement
			// makes. It is held, then failed; the ending's own snapshot after
			// it lands.
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
	if !failed.Load() {
		t.Fatal("the turn-end snapshot never uploaded, so it was not what failed")
	}
	f.assertTurnEnded(t, "fixed it")
}

// A stop or a shutdown that cancels the turn-end snapshot reaches a turn that
// has already ended. The turn end is recorded as it always is, with its
// snapshot, not on the cancelled context, where the snapshot would fail.
func TestWarmClaim_CancelDuringTheTurnEndSnapshotRecordsTheConclusion(t *testing.T) {
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
				t.Fatal("the turn-end snapshot never uploaded")
			}
			tc.cancel(f)
			if d := f.await(t, 20*time.Second); d.fenced || d.handedBack {
				t.Fatalf("disposition = %+v, want the conclusion recorded", d)
			}
			f.assertReleased(t)
			f.assertTurnEnded(t, "fixed it")
		})
	}
}

// A stop of a thread idle between a person's messages is a stop of a live
// conversation: its engagement lets the warm claim go with nothing to park,
// and the dispatcher's settlement parks the conversation as stopped by the
// person.
func TestWarmClaim_StopLetsTheClaimGoAndParksTheThread(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)

	if err := f.control.Stop(f.orgID, f.conversationID, f.userID); err != nil {
		t.Fatalf("stop of an idle thread = %v, want it accepted", err)
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	if n := f.provider.calls(); n != 1 {
		t.Errorf("provider calls = %d, want 1: the stop started a turn", n)
	}

	if _, err := f.stores.ConversationQueue.SettleUnclaimedStopsSystem(context.Background()); err != nil {
		t.Fatalf("SettleUnclaimedStopsSystem: %v", err)
	}
	conv := f.read(t)
	if conv.Status != domain.StatusOpen || conv.ParkReason != domain.ParkReasonUserCancelled || conv.StopRequestedAt != nil || conv.Concluded() {
		t.Errorf("conversation = {status %q, park %q, stop pending %v, concluded %v}, want {open, %q, no, false}",
			conv.Status, conv.ParkReason, conv.StopRequestedAt != nil, conv.Concluded(), domain.ParkReasonUserCancelled)
	}
	if conv.ResultSummary != "fixed it" {
		t.Errorf("result summary = %q, want the turn's kept", conv.ResultSummary)
	}
}

// A shutdown lets the warm claim go, released as the turn's own write would
// have released it. It never hands the conversation back, because nothing is
// mid-flight.
func TestWarmClaim_ShutdownReleasesTheClaim(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)
	before := f.turnEndState(t)

	f.cancel(errDispatcherShutdown)
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the release, not a hand-back", d)
	}
	f.assertReleased(t)
	if after := f.turnEndState(t); after != before {
		t.Errorf("the shutdown moved the conversation:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A lease fence writes nothing: the claim is someone else's to release.
func TestWarmClaim_LeaseFenceWritesNothing(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)
	before := f.turnEndState(t)

	f.cancel(errClaimLeaseLost)
	if d := f.await(t, 15*time.Second); !d.fenced {
		t.Fatalf("disposition = %+v, want fenced", d)
	}
	if released, _, _ := f.claimRelease(t); released {
		t.Error("the fenced engagement released the claim")
	}
	if !f.jailStopped.Load() {
		t.Error("the fenced engagement left its jail running")
	}
	if after := f.turnEndState(t); after != before {
		t.Errorf("the fence moved the conversation:\nbefore %+v\nafter  %+v", before, after)
	}
}

// The registry letting the claim go, as reclaiming its slot does, releases it
// at once.
func TestWarmClaim_RegistryLetGoReleasesAtOnce(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)

	if _, ok := f.x.warmWaits.LetGoOldest(); !ok {
		t.Fatal("the registry held no warm claim to let go")
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
}

// An event on the task is folded into the thread while it is idle between a
// person's messages: it is staged as a note and starts no turn on the warm
// claim, and the person's next message carries it to the model.
func TestWarmClaim_EventIsStagedForTheThreadAndStartsNoTurn(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}, {text: "looked at the failure too"}}})
	f.awaitWarm(t, 1)

	if got := f.control.StageOrDeliverAdditiveEvent(context.Background(), f.orgID, f.conversationID, "test",
		"the CI check failed again", domain.NoteProvenance{}, AdditiveFiringRef{}); got != InjectStagedResumable {
		t.Fatalf("StageOrDeliverAdditiveEvent = %v, want InjectStagedResumable", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.provider.calls(); n != 1 {
		t.Errorf("provider calls = %d, want 1: the event started a turn on the warm claim", n)
	}
	if f.claimPhase(t) != domain.ClaimPhaseWarm || f.jailStopped.Load() {
		t.Errorf("the event reached the warm claim: phase %q, jail stopped %v", f.claimPhase(t), f.jailStopped.Load())
	}

	if err := f.followUp("anything new?"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWarm(t, 2)
	if sent := f.provider.sent(1); !strings.Contains(sent, "the CI check failed again") || !strings.Contains(sent, "anything new?") {
		t.Errorf("the next turn was not sent the event with the person's message:\n%s", sent)
	}
}

// A person delegating the task again ends the thread, as the delegate route's
// boundary does before it starts the run. Until the warm claim is released,
// the run can't be claimed into the tree. Passing the ended row to
// RingWarmWaitsForEnded releases it well inside the backstop interval, and the
// run is then claimable. The run's own boundary ends only settled
// conversations, so it is not what ends a thread that is live between turns.
func TestWarmClaim_LetsGoWhenANewDelegationEndsTheConversation(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)

	// The next run waits for this conversation's memory, which the test's
	// agent never writes, so it is filed here and the tree is all the run
	// waits on.
	if _, err := f.h.AdminDB.Exec(`
		INSERT INTO conversation_memory (id, org_id, conversation_id, agent_content, source) VALUES ($1, $2, $3, $4, $5)
	`, uuid.New().String(), f.orgID, f.conversationID, "fixed it", string(domain.MemorySourceAgent)); err != nil {
		t.Fatalf("file the conversation's memory: %v", err)
	}
	var blueprintID string
	if err := f.h.AdminDB.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = $1`, f.brID).Scan(&blueprintID); err != nil {
		t.Fatalf("read the blueprint: %v", err)
	}
	endedRow, err := f.stores.Conversations.EndConversationSystem(context.Background(), f.orgID, f.conversationID, domain.EndedDelegated)
	if err != nil || endedRow == nil {
		t.Fatalf("end the thread as the delegate route does = (%+v, %v)", endedRow, err)
	}
	ended := []domain.Conversation{*endedRow}
	step0 := 0
	next, nextRun := uuid.New().String(), uuid.New().String()
	_, _, _, endedByRun, err := f.stores.Blueprints.CreateRunWithFirstStepSystem(context.Background(), f.orgID, domain.BlueprintRun{
		ID: nextRun, BlueprintID: blueprintID, TaskID: f.taskID,
		TriggerType: domain.BlueprintTriggerManual, WorktreePath: f.wtPath,
	}, db.AgentClaimStamp{}, "", domain.Conversation{
		ID: next, TaskID: f.taskID, PromptID: f.promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: f.userID, BlueprintRunID: nextRun, BlueprintStepIndex: &step0,
	})
	if err != nil {
		t.Fatalf("start another run on the task: %v", err)
	}
	if len(endedByRun) != 0 {
		t.Fatalf("the new run ended %+v, want nothing left for it to end", endedByRun)
	}
	if got := f.parkFleet.claim(t, f.y); got != nil {
		t.Fatalf("another executor claimed %s while the warm claim held the task's tree", got.ID)
	}

	f.control.RingWarmWaitsForEnded(context.Background(), f.orgID, ended)
	if d := f.await(t, 5*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	id, epoch := f.y.executorIdentity()
	got, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), id, epoch, f.y.claimPlacement(), db.DefaultClaimLease)
	if err != nil || got == nil || got.ID != next {
		t.Fatalf("claim once the warm claim let go = (%+v, %v), want conversation %s", got, err, next)
	}
}

// A conversation already ended when the wait starts is let go on the wait's
// first read. The test ends it with SQL, which wakes nothing, while the
// turn-end snapshot is held.
func TestWarmClaim_EndingBeforeTheWaitIsNotMissed(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}},
		beforeStart: func(f *warmEngagement) { f.gate.hold() }})

	select {
	case <-f.gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the turn-end snapshot never uploaded")
	}
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET ended_at = now(), ended_reason = $1 WHERE id = $2`,
		string(domain.EndedDelegated), f.conversationID)
	f.gate.let()

	if d := f.await(t, warmWaitBackstop/3); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertTurnEnded(t, "fixed it")
	f.assertReleased(t)
	if phase := f.claimPhase(t); phase != domain.ClaimPhaseWarm {
		t.Errorf("released claim's phase = %q, want %q: the turn ended without keeping its claim warm", phase, domain.ClaimPhaseWarm)
	}
}

// A message sent before the turn's state is written, so nothing rings for it,
// still starts the next turn on the warm claim, well before the backstop poll
// would find it.
func TestWarmClaim_MessageDuringTheTurnEndIsNotLost(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}, {text: "done that too"}},
		beforeStart: func(f *warmEngagement) { f.gate.hold() }})

	// The turn-end snapshot's upload is held, so the turn has ended and its
	// state is not written yet: the conversation still reads as running.
	select {
	case <-f.gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the turn-end snapshot never uploaded")
	}
	if err := f.followUp("one more thing"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.gate.let()

	f.awaitCalls(t, 2, warmWaitBackstop/3)
	if !strings.Contains(f.provider.sent(1), "one more thing") {
		t.Errorf("the second turn was not sent the message:\n%s", f.provider.sent(1))
	}
}

// A message waiting when the window expires, with no ring to say so, is read
// rather than left to a cold claim. It is written straight onto the
// transcript here, which rings nothing, and the backstop poll is far longer
// than the window.
func TestWarmClaim_ExpiryReadsAMessageItWasNeverRungFor(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: 2 * time.Second, turns: []gateTurn{{text: "fixed it"}, {text: "done that too"}}})
	f.awaitWarm(t, 1)

	if _, err := f.stores.Conversations.InsertMessageSystem(context.Background(), f.orgID, pendingUserInput(f.conversationID, f.userID, "one more thing")); err != nil {
		t.Fatalf("queue the message: %v", err)
	}
	f.awaitCalls(t, 2, 10*time.Second)
	if !strings.Contains(f.provider.sent(1), "one more thing") {
		t.Errorf("the turn at expiry was not sent the message:\n%s", f.provider.sent(1))
	}
}

// The claim's duration is the time its turns ran, not the time it was held.
func TestWarmClaim_DurationIsTheTurnsNotTheWait(t *testing.T) {
	spans := recordSpans(t)
	f := startWarmEngagement(t, warmOpts{window: 3 * time.Second, turns: []gateTurn{{text: "fixed it"}, {text: "done that too"}}})
	f.awaitWarm(t, 1)
	const waited = time.Second
	time.Sleep(waited)
	if err := f.followUp("one more thing"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWarm(t, 2)
	if d := f.await(t, 20*time.Second); d.fenced || d.handedBack {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}

	turns := int64(f.run(0).result.DurationMs + f.run(1).result.DurationMs)
	_, _, durationMs := f.claimRelease(t)
	if durationMs != turns {
		t.Errorf("claims.duration_ms = %d, want the two turns' %d", durationMs, turns)
	}
	if durationMs >= waited.Milliseconds() {
		t.Errorf("claims.duration_ms = %d, which counts the %s the claim was held", durationMs, waited)
	}
	f.assertReleased(t)

	// One terminal span for the engagement, at the let-go, carrying both turns.
	span := completionSpanFor(t, spans(), f.conversationID)
	if got := spanAttr(t, span, "outcome").AsString(); got != "concluded_continue" {
		t.Errorf("engagement.complete outcome = %q, want concluded_continue", got)
	}
	if got := spanAttr(t, span, "agent.duration_ms").AsInt64(); got != turns {
		t.Errorf("engagement.complete agent.duration_ms = %d, want the two turns' %d", got, turns)
	}
}

// A close while the claim is warm tells no conversation, because this one is
// not working. The person's next message resumes it on the warm claim, and
// the turn it runs is told the task closed, as a cold claim's turn would be:
// the native loop reads the staged note as a user message.
func TestWarmClaim_ResumeTellsOfACloseWhileWarm(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "staged the review"}, {text: "shortened the comments"}}})
	f.awaitWarm(t, 1)
	ctx := context.Background()

	// CI passed while the conversation waited on its warm claim.
	task, err := f.stores.Tasks.GetSystem(ctx, f.orgID, f.taskID)
	if err != nil || task == nil {
		t.Fatalf("load task: %v", err)
	}
	eventID := uuid.New().String()
	pgtest.MustExec(t, f.h.AdminDB, `
		INSERT INTO events (id, org_id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES ($1, $2, $3, $4, '', '{}'::jsonb, now())
	`, eventID, f.orgID, task.EntityID, domain.EventGitHubPRCICheckPassed)
	out, err := f.stores.Tasks.CloseOnEventSystem(ctx, f.orgID, f.taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed, eventID)
	if err != nil || !out.Closed || len(out.WorkingConversationIDs) != 0 {
		t.Fatalf("close = (%+v, %v), want closed with no working conversation to tell", out, err)
	}

	if err := f.followUp("make the review comments shorter"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	f.awaitWarm(t, 2)

	evt, err := f.stores.Events.GetSystem(ctx, f.orgID, eventID)
	if err != nil || evt == nil {
		t.Fatalf("read the pass: %v", err)
	}
	metadataJSON := evt.MetadataJSON
	if current, err := events.Current(ctx, f.orgID, evt.EventType, metadataJSON); err == nil {
		metadataJSON = current
	}
	sent := f.provider.sent(1)
	note := domain.TaskClosedNote(domain.TaskClose{Kind: domain.TaskClosedByEvent, EventType: evt.EventType, MetadataJSON: metadataJSON})
	if !strings.Contains(sent, note) {
		t.Errorf("the resumed turn was not told of the close %q:\n%s", note, sent)
	}
	if !strings.Contains(sent, "make the review comments shorter") {
		t.Errorf("the resumed turn was not sent the person's message:\n%s", sent)
	}
	if n := f.claimRows(t); n != 1 {
		t.Errorf("claim rows = %d, want the one warm claim both turns ran on", n)
	}
}

// A warm resume after the team's default model changed lets the claim go
// instead of running the turn on the model the engagement was built with. The
// conversation is left queued with no active claim, so a cold claim runs it on
// the team's model.
func TestWarmClaim_ResumeAfterTheTeamsModelChangedLetsTheClaimGo(t *testing.T) {
	f := startWarmEngagement(t, warmOpts{window: time.Hour, turns: []gateTurn{{text: "fixed it"}}})
	f.awaitWarm(t, 1)

	f.setTeamDefault(domain.ModelOpus)
	if err := f.followUp("now update the README too"); err != nil {
		t.Fatalf("follow-up from the control pod: %v", err)
	}
	if d := f.await(t, 15*time.Second); d.fenced || d.handedBack || d.launchErr != nil {
		t.Fatalf("disposition = %+v, want the ordinary ending", d)
	}
	f.assertReleased(t)
	if n := f.provider.calls(); n != 1 {
		t.Errorf("provider calls = %d, want 1: a turn ran on the engagement's model", n)
	}
	if conv := f.read(t); conv.Status != domain.StatusQueued {
		t.Errorf("conversation status = %q, want %q for a cold claim to drive", conv.Status, domain.StatusQueued)
	}
	if n := f.activeClaims(t); n != 0 {
		t.Errorf("active claims = %d, want none", n)
	}
}
