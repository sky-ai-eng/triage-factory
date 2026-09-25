package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// signallingToolHost holds a call until the engagement's context ends, like
// blockingToolHost, and says when the call has started, so a test can land
// its cancellation mid-call rather than before the tool ran.
type signallingToolHost struct {
	ctx     context.Context
	entered chan struct{}
}

func (h signallingToolHost) Call(string, map[string]any) (agentloop.ToolOutcome, error) {
	close(h.entered)
	<-h.ctx.Done()
	return agentloop.ToolOutcome{}, errors.New("agentloop: read tool response: use of closed network connection")
}

func (signallingToolHost) Close() error { return nil }

// capturingProvider records the rows of the first request it is sent and
// then blocks until its context ends: all a test needs from a successor
// engagement is what it was about to ask the model.
type capturingProvider struct {
	rows chan []domain.Message
}

func (p capturingProvider) Stream(ctx context.Context, req inference.Request) (*inference.Completion, error) {
	select {
	case p.rows <- req.Rows:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// claimOutcome reads one claim's outcome.
func claimOutcome(t *testing.T, f stallFixture, claimID string) string {
	t.Helper()
	var outcome string
	if err := f.database.QueryRow(`SELECT COALESCE(outcome, '') FROM claims WHERE id = ?`, claimID).Scan(&outcome); err != nil {
		t.Fatalf("read claim %s: %v", claimID, err)
	}
	return outcome
}

// TestShutdown_NativeEngagementIsHandedBackAndTheNextClaimContinuesIt drives
// the whole hand-off through the real engine: an executor shutting down in the
// middle of a tool call gives the conversation back rather than parking it, a
// successor claims it at once with neither budget spent, and the successor's
// first model request carries the transcript the first engagement wrote.
func TestShutdown_NativeEngagementIsHandedBackAndTheNextClaimContinuesIt(t *testing.T) {
	f := newStallFixture(t, "r-shutdown-native", activityTimings{idle: time.Hour})

	tools := signallingToolHost{ctx: f.claimCtx, entered: make(chan struct{})}
	go func() {
		<-tools.entered
		f.fence(errDispatcherShutdown)
	}()
	provider := &scriptedGateProvider{turns: []gateTurn{{calls: []domain.ToolCall{bashCall("c1", "make test")}}}}
	result := f.runNative(t, provider, tools)
	if result.Kind != agentloop.ResultCancelled {
		t.Fatalf("result = %v (err %v), want cancelled", result.Kind, result.Err)
	}

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(), result, nil)
	if !disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want handed back and unfenced", disp)
	}

	if got := storedStatus(t, f.database, f.conversationID); got != "" {
		t.Errorf("stored status = %q, want none — a hand-back leaves the conversation mid-flight, not parked", got)
	}
	var parkReason string
	if err := f.database.QueryRow(`SELECT COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conversationID).Scan(&parkReason); err != nil {
		t.Fatalf("read park_reason: %v", err)
	}
	if parkReason != "" {
		t.Errorf("park_reason = %q, want none — nobody stopped this conversation", parkReason)
	}
	if got := claimOutcome(t, f, f.claimID); got != "requeued_shutdown" {
		t.Errorf("claim outcome = %q, want requeued_shutdown", got)
	}

	next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Fatalf("successor claim = (%+v, %v), want conversation %s at once", next, err, f.conversationID)
	}
	if next.LostEngagements != 0 || next.SetupFailures != 0 {
		t.Errorf("successor's budgets = (lost %d, setup %d), want (0, 0) — a clean shutdown spends neither", next.LostEngagements, next.SetupFailures)
	}

	// The successor's engine, on its own claim. What it sends the model is
	// the whole question: the first engagement's call, answered.
	successorCtx, cancelSuccessor := context.WithCancel(context.Background())
	defer cancelSuccessor()
	capture := capturingProvider{rows: make(chan []domain.Message, 1)}
	done := make(chan agentloop.Result, 1)
	go func() {
		engine := &agentloop.Engine{
			Transcript:  newNativeTranscript(f.s, runmode.LocalDefaultOrgID, f.conversationID, next.ClaimID),
			Credentials: staticGateCredentials{provider: capture},
			Tools:       &recordingToolHost{},
			Retry:       agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
		}
		done <- engine.Run(successorCtx, agentloop.Params{
			OrgID: runmode.LocalDefaultOrgID, ConversationID: f.conversationID,
			Model: "claude-sonnet-4-5", SystemPrompt: "system", HasBlueprint: true,
		})
	}()
	var rows []domain.Message
	select {
	case rows = <-capture.rows:
	case <-time.After(10 * time.Second):
		t.Fatal("the successor never asked the model anything")
	}
	cancelSuccessor()
	<-done

	var sawCall, sawAnswer, sawMission bool
	for _, r := range rows {
		for _, c := range r.ToolCalls {
			if c.ID == "c1" {
				sawCall = true
			}
		}
		if r.Role == "tool" && r.ToolCallID == "c1" {
			sawAnswer = true
		}
		if r.Role == "user" && strings.Contains(r.Content, "do the work") {
			sawMission = true
		}
	}
	if !sawMission || !sawCall || !sawAnswer {
		raw, _ := json.Marshal(rows)
		t.Errorf("successor's request = (mission %v, call %v, answer %v), want the first engagement's transcript continued: %s",
			sawMission, sawCall, sawAnswer, raw)
	}
}

// TestShutdown_AStopPendingAtTheHandBackIsSettledByTheNextDispatcher is the
// second half of first-cancel-wins: a stop requested after the shutdown
// reached the engagement is not lost with it. The hand-back leaves the
// intent where it is, and the settlement any dispatcher runs parks it.
func TestShutdown_AStopPendingAtTheHandBackIsSettledByTheNextDispatcher(t *testing.T) {
	f := newStallFixture(t, "r-shutdown-then-stop", activityTimings{idle: time.Hour})
	f.fence(errDispatcherShutdown)
	if err := f.s.Stop(runmode.LocalDefaultOrgID, f.conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultCancelled, Err: context.Canceled}, nil)
	if !disp.handedBack {
		t.Fatalf("disposition = %+v, want handed back — the shutdown reached the engagement first", disp)
	}
	var intent bool
	if err := f.database.QueryRow(`SELECT stop_requested_at IS NOT NULL FROM conversations WHERE id = ?`, f.conversationID).Scan(&intent); err != nil {
		t.Fatalf("read intent: %v", err)
	}
	if !intent {
		t.Fatal("the hand-back cleared the stop intent; nothing would park the conversation the person stopped")
	}

	f.s.settleUnclaimedStops(context.Background())
	var status, parkReason string
	if err := f.database.QueryRow(`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conversationID).Scan(&status, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "open" || parkReason != string(domain.ParkReasonUserCancelled) {
		t.Errorf("after the settlement = (%q, %q), want (open, user_cancelled)", status, parkReason)
	}
}

// TestShutdown_AStopThatLandedFirstStillParks: the engagement was cancelled by
// a person before the shutdown reached it, so its cause is the stop's and it
// parks as one, whatever the dispatcher did afterwards.
func TestShutdown_AStopThatLandedFirstStillParks(t *testing.T) {
	f := newStallFixture(t, "r-stop-then-shutdown", activityTimings{idle: time.Hour})
	f.fence(errStopRequested)
	f.fence(errDispatcherShutdown)

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultCancelled, Err: context.Canceled}, nil)
	if disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want a plain park", disp)
	}
	if got := storedStatus(t, f.database, f.conversationID); got != "open" {
		t.Errorf("stored status = %q, want open", got)
	}
	if got := claimOutcome(t, f, f.claimID); got == "requeued_shutdown" {
		t.Error("a stop that landed first was handed back as a shutdown")
	}
}

// TestShutdown_TheHandBackSnapshotsBeforeItLetsGo covers the shared ending on
// the SDK runtime, where the session file is the continuity: the snapshot is
// written and owned by the handed-back claim, so a successor on another host
// waits for it and resumes the session from it.
func TestShutdown_TheHandBackSnapshotsBeforeItLetsGo(t *testing.T) {
	f := newStallFixture(t, "r-shutdown-sdk", activityTimings{idle: time.Hour})
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	f.s.SetStorage(blobs)
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "work since the last step")
	namespace := workspaceKey(f.task.ID)

	if fenced := f.s.handBackOnShutdown(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		namespace: namespace, claudeCwd: wtPath, runtime: domain.ConversationRuntimeSDK,
	}, ""); fenced {
		t.Fatal("the hand-back was fenced; the engagement held its claim")
	}
	rc, err := f.s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("the hand-back wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	state, err := f.s.snapshotStateFor(context.Background(), runmode.LocalDefaultOrgID, namespace)
	if err != nil || state == nil || state.State != domain.WorkspaceSnapshotWritten || state.WriterClaimID != f.claimID {
		t.Errorf("snapshot record = (%+v, %v), want written by the handed-back claim", state, err)
	}
	if got := claimOutcome(t, f, f.claimID); got != "requeued_shutdown" {
		t.Errorf("claim outcome = %q, want requeued_shutdown", got)
	}

	// A second hand-back finds the claim gone and says so.
	if fenced := f.s.handBackOnShutdown(f.claimCtx, liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: f.conversationID, claimID: f.claimID,
		runtime: domain.ConversationRuntimeSDK,
	}, ""); !fenced {
		t.Error("a hand-back of a released claim reported unfenced")
	}
}

// TestDispatch_ShutdownDuringBringUpLeavesTheClaimForTheShutdownRelease is the
// dispatcher's half: a shutdown that lands while bring-up is fetching cancels
// the fetch through the claim context, and the engagement reads the failed
// setup as the shutdown it is. Nothing is written — no setup failure charged,
// no park, no transcript row — and the claim is still live for the shutdown
// release to hand back once this engagement has returned.
func TestDispatch_ShutdownDuringBringUpLeavesTheClaimForTheShutdownRelease(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "938", "")

	fetchEntered := make(chan struct{})
	fetchEnded := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetchEntered)
		select {
		case <-r.Context().Done():
			fetchEnded <- true
		case <-time.After(10 * time.Second):
			fetchEnded <- false
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	conv := fx.conv
	conv.OrgID = runmode.LocalDefaultOrgID
	dispatcherCtx, shutDown := context.WithCancel(context.Background())
	defer shutDown()
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		fx.s.dispatchClaimedConversation(dispatcherCtx, &conv, time.Now())
	}()

	select {
	case <-fetchEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("bring-up never reached the PR fetch")
	}
	shutDown()

	select {
	case cancelled := <-fetchEnded:
		if !cancelled {
			t.Fatal("the shutdown did not cancel the in-flight fetch")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the fetch never ended")
	}
	select {
	case <-dispatched:
	case <-time.After(10 * time.Second):
		t.Fatal("the engagement did not return after the shutdown")
	}

	if got := fx.storedStatus(t); got != "" {
		t.Errorf("status = %q, want none — a shutdown parks nothing", got)
	}
	if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != "" {
		t.Errorf("claim outcomes = %v, want the one claim still live — charging it as a setup failure would spend the budget on a deploy", got)
	}
	if got := fx.blueprintStatus(t); got != "running" {
		t.Errorf("blueprint status = %q, want running", got)
	}
	if rows := fx.transcript(t); len(rows) != 0 {
		t.Errorf("transcript = %+v, want nothing written", rows)
	}
}
