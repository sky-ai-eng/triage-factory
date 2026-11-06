package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeLiveProc stands in for *agentproc.LiveRun in the driver tests so
// driveLiveConversation can be exercised without spawning a subprocess. Close()
// closes done (idempotent) so a subsequent select on Done() doesn't block.
type fakeLiveProc struct {
	mu        sync.Mutex
	done      chan struct{}
	closed    bool
	sessionID string
	result    *agentproc.Result
	err       error
	sendCount int
	// sendErr is what Send answers, so a test can model a process that will
	// not take a message.
	sendErr error
	// queued is what QueuedTurns answers: the turns the process still owes.
	// A test stages it to model a message steered in while the turn ran —
	// the real process counts its own sends against its results, which a
	// fake fed results by hand cannot.
	queued int
	// onSend fires (with the 1-based send count) after each Send so a test can
	// feed the next turn's result onto the results channel — the turn a nudge
	// the driver sent produces.
	onSend func(count int)
	// onClose fires inside Close, before done is closed, so a test can pin
	// what the world looks like at the moment the driver lets go of the
	// process — the park must not have landed yet.
	onClose func()
}

func newFakeLiveProc(sessionID string) *fakeLiveProc {
	return &fakeLiveProc{done: make(chan struct{}), sessionID: sessionID}
}

func (f *fakeLiveProc) Done() <-chan struct{}     { return f.done }
func (f *fakeLiveProc) SessionID() string         { return f.sessionID }
func (f *fakeLiveProc) Stderr() string            { return "" }
func (f *fakeLiveProc) Result() *agentproc.Result { f.mu.Lock(); defer f.mu.Unlock(); return f.result }
func (f *fakeLiveProc) Err() error                { f.mu.Lock(); defer f.mu.Unlock(); return f.err }
func (f *fakeLiveProc) QueuedTurns() int          { f.mu.Lock(); defer f.mu.Unlock(); return f.queued }

// owe stages n turns the process still has to run, the reading a real
// process gives while a steered message is queued behind the current turn.
func (f *fakeLiveProc) owe(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = n
}

func (f *fakeLiveProc) Send(_ context.Context, _ string) error {
	f.mu.Lock()
	if f.sendErr != nil {
		err := f.sendErr
		f.mu.Unlock()
		return err
	}
	f.sendCount++
	n := f.sendCount
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return nil
}

func (f *fakeLiveProc) sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendCount
}

func (f *fakeLiveProc) Close() error {
	f.mu.Lock()
	hook := f.onClose
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakeLiveProc) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// exit simulates the process terminating on its own (a crash, or a Close
// from elsewhere) with the given folded result/err, closing Done() so the
// driver's proc.Done() branch fires.
func (f *fakeLiveProc) exit(result *agentproc.Result, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = result
	f.err = err
	if !f.closed {
		f.closed = true
		close(f.done)
	}
}

// TestDriveLiveConversation_TerminalResultClosesAndReturns: a turn the model
// ended itself closes the process (freeing the session for the gate's resume)
// and is handed back for processCompletion to conclude — no hibernation.
func TestDriveLiveConversation_TerminalResultClosesAndReturns(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	want := &agentproc.Result{StopReason: "end_turn", Result: "Done."}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nil)

	if out.result != want {
		t.Errorf("result = %+v, want %+v", out.result, want)
	}
	if out.hibernated {
		t.Error("did not expect hibernation on a terminal result")
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed after the terminal result")
	}
}

// TestDriveLiveConversation_AStopIsTheConclusion: a turn stop_blueprint ended
// concludes on the call, as it does natively: the would-stop hook is not
// asked, and the process closes and the result goes back for
// processCompletion.
func TestDriveLiveConversation_AStopIsTheConclusion(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	want := &agentproc.Result{
		StopReason: "tool_use",
		Result:     "Stopping here.",
		Terminal:   &agentproc.Terminal{Outcome: domain.ConversationOutcomeAbort, Reason: "blocked", Summary: "tried"},
	}
	results <- want
	asked := 0
	nudge := func(context.Context) string { asked++; return "did you mean to?" }

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nudge)

	if out.result != want || out.hibernated {
		t.Errorf("outcome = %+v, want the stopped turn's result handed back, not a park", out)
	}
	if asked != 0 || proc.sends() != 0 {
		t.Errorf("nudge asked %d times, %d sends; a stop is the verdict and asks nothing", asked, proc.sends())
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed after the stopped turn")
	}
}

// TestDriveLiveConversation_CtxCancelReturnsErr: a cancelled ctx (the hard-kill path)
// closes the process and surfaces the ctx error so the caller routes through
// its cancelled handling.
func TestDriveLiveConversation_CtxCancelReturnsErr(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := s.driveLiveConversation(ctx, liveParkContext{}, proc, make(chan *agentproc.Result), nil)

	if out.err == nil {
		t.Error("expected a ctx error on cancel")
	}
	if out.hibernated || out.result != nil {
		t.Errorf("expected only an error on cancel, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed on ctx cancel")
	}
}

// TestDriveLiveConversation_ProcessExitCarriesResult covers the proc.Done() branch:
// when the process exits on its own (rather than us closing it off the
// results channel), the driver hands back the folded result for
// processCompletion — and does not hibernate.
func TestDriveLiveConversation_ProcessExitCarriesResult(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	want := &agentproc.Result{StopReason: "end_turn", Result: "exited"}
	proc.exit(want, nil) // process already gone, Done() closed, with a result set

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), nil)

	if out.result != want {
		t.Errorf("result = %+v, want %+v", out.result, want)
	}
	if out.hibernated {
		t.Error("did not expect hibernation on a process exit")
	}
}

// TestDriveLiveConversation_ProcessExitCarriesErr is the crash half of the proc.Done()
// branch: an exit with no result surfaces the process error so the caller
// routes through failConversation.
func TestDriveLiveConversation_ProcessExitCarriesErr(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	wantErr := errors.New("agent runtime exited with error")
	proc.exit(nil, wantErr)

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), nil)

	if out.result != nil {
		t.Errorf("result = %+v, want nil on a crash exit", out.result)
	}
	if !errors.Is(out.err, wantErr) {
		t.Errorf("err = %v, want %v", out.err, wantErr)
	}
}

// TestDriveLiveConversation_AnUnfinishedTurnClosesAndHandsBack: a turn whose
// stop reason is not an end of turn, with nothing queued behind it, closes the
// process and hands the result back for processCompletion to park — the
// driver does not keep the process for a message that has not arrived, and
// asks the would-stop hook nothing, since the turn is not concluding. A second
// result left on the channel proves it: the driver returned on the refusal and
// never read it.
func TestDriveLiveConversation_AnUnfinishedTurnClosesAndHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	refused := &agentproc.Result{StopReason: "refusal"}
	results <- refused
	results <- &agentproc.Result{StopReason: "end_turn", Result: "done"}
	asked := 0
	nudge := func(context.Context) string { asked++; return "did you mean to?" }

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nudge)

	if out.result != refused {
		t.Errorf("result = %+v, want the unfinished turn handed back", out.result)
	}
	if out.hibernated {
		t.Error("an unfinished turn is parked by processCompletion, not by the driver")
	}
	if !proc.wasClosed() {
		t.Error("an unfinished turn with nothing queued must close the process")
	}
	if len(results) != 1 {
		t.Errorf("results left = %d, want 1 (the driver must not read past the turn it returned)", len(results))
	}
	if asked != 0 || proc.sends() != 0 {
		t.Errorf("nudge asked %d times, %d sends; a turn that is not concluding asks nothing", asked, proc.sends())
	}
}

// TestDriveLiveConversation_QueuedTurnOutlivesTheTurnEnd: a message steered in
// while the turn ran is queued by the process and starts the next turn, so a
// turn the model ended with a turn still owed does not conclude — the model
// has not seen the message — and the driver stays on the same process, under
// the same claim, and reads the turn the steer produces. The would-stop hook
// is not asked either: the steer comes first, as the native loop's recheck
// puts it. Delivering prose first and a stop second proves it: the stop is
// only reachable by staying past the prose.
func TestDriveLiveConversation_QueuedTurnOutlivesTheTurnEnd(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	proc.owe(1)
	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Some prose; the steered turn runs next."}
	want := &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}
	results <- want
	asked := 0
	nudge := func(context.Context) string { asked++; return "did you mean to?" }

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nudge)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion the queued turn produced", out.result)
	}
	if out.hibernated {
		t.Error("a turn end with a turn still owed must not park")
	}
	if asked != 0 {
		t.Errorf("nudge asked %d times; the queued turn comes first", asked)
	}
}

// TestDriveLiveConversation_QueuedTurnOutlivesThePause is the same for a
// paused turn: an interrupt that lands with a steer queued behind it ends
// this turn, and the process starts the queued one next.
func TestDriveLiveConversation_QueuedTurnOutlivesThePause(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	proc.owe(1)
	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	want := &agentproc.Result{Terminal: &agentproc.Terminal{Outcome: domain.ConversationOutcomeFinish, Reason: "r", Summary: "s"}}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nil)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion the queued turn produced", out.result)
	}
	if out.hibernated {
		t.Error("a pause with a turn still owed must not park")
	}
}

// TestDriveLiveConversation_NudgeKeepsTheRunGoing: a turn the model ended
// itself is first put to the would-stop hook. A note it returns is sent into
// the process and the driver reads the turn that answers it, and that turn is
// the conclusion: the hook is not asked again in the same engagement, even
// one that would ask again (its note failed to record, say).
func TestDriveLiveConversation_NudgeKeepsTheRunGoing(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	answered := &agentproc.Result{StopReason: "end_turn", Result: "No artifact was needed: the PR was already merged."}
	proc.onSend = func(int) { results <- answered }
	results <- &agentproc.Result{StopReason: "end_turn", Result: "Looked into it."}
	asked := 0
	nudge := func(context.Context) string { asked++; return "did you mean to?" }

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, nudge)

	if out.result != answered {
		t.Errorf("result = %+v, want the turn that answered the nudge", out.result)
	}
	if asked != 1 || proc.sends() != 1 {
		t.Errorf("nudge asked %d times with %d sends, want 1 and 1", asked, proc.sends())
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed after the conclusion")
	}
}

// TestDriveLiveConversation_ANudgeTheProcessRefusesConcludes: a note the
// process will not take does not hold up the conclusion — the turn concludes
// as it would have with nothing to ask.
func TestDriveLiveConversation_ANudgeTheProcessRefusesConcludes(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	proc.sendErr = agentproc.ErrRunClosing
	results := make(chan *agentproc.Result, 1)
	want := &agentproc.Result{StopReason: "end_turn", Result: "Looked into it."}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results,
		func(context.Context) string { return "did you mean to?" })

	if out.result != want || out.fenced {
		t.Errorf("outcome = %+v, want the turn concluded on", out)
	}
}

// TestDriveLiveConversation_TheNudgeIsRecordedUnderTheClaim: the note the
// driver sends lands on the transcript as a nudge row under the engagement's
// claim, which is what the hook reads back to know it has asked.
func TestDriveLiveConversation_TheNudgeIsRecordedUnderTheClaim(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-nudge", "sess-nudge", "/tmp/wt-nudge")
	claimID := markEngaged(t, database, "r-nudge")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	proc := newFakeLiveProc("sess-nudge")
	results := make(chan *agentproc.Result, 8)
	answered := &agentproc.Result{StopReason: "end_turn", Result: "answered"}
	proc.onSend = func(int) { results <- answered }
	results <- &agentproc.Result{StopReason: "end_turn", Result: "first"}
	sent := false
	nudge := func(context.Context) string {
		if sent {
			return ""
		}
		sent = true
		return artifactNudgeNote
	}
	park := liveParkContext{orgID: runmode.LocalDefaultOrgID, conversationID: "r-nudge", claimID: claimID}

	if out := s.driveLiveConversation(context.Background(), park, proc, results, nudge); out.result != answered {
		t.Fatalf("result = %+v, want the turn that answered the nudge", out.result)
	}
	var subtype, content string
	var rowClaim sql.NullString
	if err := database.QueryRow(`SELECT subtype, content, claim_id FROM messages WHERE conversation_id = 'r-nudge' AND role = 'user' ORDER BY id DESC LIMIT 1`).Scan(&subtype, &content, &rowClaim); err != nil {
		t.Fatalf("read the nudge row: %v", err)
	}
	if subtype != domain.MessageSubtypeInjectionNudge || content != artifactNudgeNote || rowClaim.String != claimID {
		t.Errorf("nudge row = (subtype %q, claim %q, content %.40q), want the nudge under claim %s", subtype, rowClaim.String, content, claimID)
	}
}

// activeClaimState reads one claim's release state: whether it is still
// live, and the outcome it released with (empty while live).
func activeClaimState(t *testing.T, database *sql.DB, claimID string) (live bool, outcome string) {
	t.Helper()
	var released sql.NullString
	var out sql.NullString
	if err := database.QueryRow(`SELECT released_at, outcome FROM claims WHERE id = ?`, claimID).Scan(&released, &out); err != nil {
		t.Fatalf("read claim %s: %v", claimID, err)
	}
	return !released.Valid, out.String
}

// TestDriveLiveConversation_NoneLeavesTheClaimForCompletion pins the order the
// fix depends on: an unfinished turn does NOT park from inside the driver.
// The process is closed and the result handed back with the engagement's claim
// still live, and processCompletion parks — so the claim is released only
// after the process that wrote under it is gone. A park written here, with
// the process still alive to take a steer, is what killed steered turns on
// the fence.
func TestDriveLiveConversation_NoneLeavesTheClaimForCompletion(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-none", "sess-none", "/tmp/wt-none")
	claimID := markEngaged(t, database, "r-none")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-none'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-none")
	results := make(chan *agentproc.Result, 8)
	none := &agentproc.Result{StopReason: "refusal"}
	results <- none
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-none",
		namespace: "seedbpr-r-none", claudeCwd: "/tmp/wt-none",
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)
	if out.result != none {
		t.Fatalf("result = %+v, want the unfinished turn handed back", out.result)
	}
	if !proc.wasClosed() {
		t.Error("the process must be closed before the result is handed back")
	}
	if live, outcome := activeClaimState(t, database, claimID); !live {
		t.Errorf("claim released %q by the driver; the park (and the release) belong to processCompletion, after the process is gone", outcome)
	}
	var status sql.NullString
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id='r-none'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status.Valid && status.String == "open" {
		t.Error("status = open; the driver must not flip a row its process could still write to")
	}
}

// TestDriveLiveConversation_InterruptParksOpenNotTerminal pins the pause
// semantics: a result the reader marked Interrupted (our own interrupt() ended
// the turn) is a pause, not a failure — the run parks open with its claim
// released 'parked', and the process is closed BEFORE that park lands, never
// left alive for a message that has not arrived. A second result left on the
// channel proves the driver returned on the pause.
func TestDriveLiveConversation_InterruptParksOpenNotTerminal(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-pause", "sess-pause", "/tmp/wt-pause")
	claimID := markEngaged(t, database, "r-pause")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-pause'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-pause")
	// At the moment the driver lets go of the process, the claim is still
	// live: the park comes after, once nothing can write under it.
	proc.onClose = func() {
		if live, outcome := activeClaimState(t, database, claimID); !live {
			t.Errorf("claim released %q before the process was closed", outcome)
		}
	}

	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	results <- &agentproc.Result{StopReason: "end_turn", Result: "done"}
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-pause",
		namespace: "seedbpr-r-pause", claudeCwd: "/tmp/wt-pause",
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}

	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)
	if !out.hibernated {
		t.Fatalf("a pause with nothing queued should park open (hibernated), got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("a pause with nothing queued must close the process")
	}
	if len(results) != 1 {
		t.Errorf("results left = %d, want 1 (the driver must not read past the pause)", len(results))
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id='r-pause'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (the paused turn parks the run open)", status)
	}
	if live, outcome := activeClaimState(t, database, claimID); live || outcome != "parked" {
		t.Errorf("claim live=%v outcome=%q, want released 'parked'", live, outcome)
	}
}

// TestDriveLiveConversation_ErrorWithoutInterruptStaysTerminal pins the inverse: the
// same is_error/error_during_execution shape with NO pause in flight is a
// genuine failure and stays terminal.
func TestDriveLiveConversation_ErrorWithoutInterruptStaysTerminal(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess-err")
	results := make(chan *agentproc.Result, 1)
	r := &agentproc.Result{IsError: true, Subtype: "error_during_execution"}
	results <- r

	out := s.driveLiveConversation(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrgID, conversationID: "r-err"}, proc, results, nil)
	if out.result != r {
		t.Fatalf("expected the error result back, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("a terminal error should close the process")
	}
}

// TestFoldAccounting_PauseDoesNotPoisonConclusion pins the merge-fold split
// that keeps a paused run's valid conclusion valid: MergeResult's IsError is
// deliberately sticky across turns, so a benign interrupted (pause) turn
// earlier in the conversation infects the accounting fold — the disposition
// processCompletion acts on must come from the turn the driver classified,
// with only cost/duration/turns taken from the fold, which carries the
// process's newest running total.
func TestFoldAccounting_PauseDoesNotPoisonConclusion(t *testing.T) {
	pause := &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true, CostUSD: 0.25, DurationMs: 1000, NumTurns: 15}
	conclusion := &agentproc.Result{Result: "done", StopReason: "end_turn", Subtype: "success", CostUSD: 0.75, DurationMs: 1500, NumTurns: 20}
	merged := agentproc.MergeResult(pause, conclusion)
	if !merged.IsError || !merged.Interrupted {
		t.Fatal("precondition: MergeResult keeps IsError/Interrupted sticky across turns")
	}

	got := foldAccounting(conclusion, merged)
	if got.IsError || got.Interrupted {
		t.Errorf("disposition must come from the classified turn: IsError=%v Interrupted=%v", got.IsError, got.Interrupted)
	}
	if got.Subtype != "success" || got.Result != conclusion.Result {
		t.Errorf("classified turn's text/subtype must survive the fold: %+v", got)
	}
	if got.CostUSD != 0.75 || got.DurationMs != 1500 || got.NumTurns != 20 {
		t.Errorf("accounting must be the process's running total: cost=%v dur=%v turns=%v", got.CostUSD, got.DurationMs, got.NumTurns)
	}
	if conclusion.CostUSD != 0.75 {
		t.Error("foldAccounting must not mutate the classified result in place")
	}
}

// TestDriveLiveConversation_PausedParkSettlesProcessSpend: a run that goes
// dormant carries what its process reported spending into the park, settled
// as this engagement's lump on its own claim's row — so the ledger, and every
// usage read over it, counts the run from the moment it parks rather than
// from whenever a later resume concludes.
func TestDriveLiveConversation_PausedParkSettlesProcessSpend(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-idle-spend", "sess-idle-spend", "/tmp/wt-idle-spend")
	claimID := markEngaged(t, database, "r-idle-spend")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	msgID, err := s.conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: "r-idle-spend", Role: "assistant", Content: "working…", Model: "claude-sonnet-4-6", ClaimID: claimID,
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-idle-spend'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-idle-spend")
	proc.result = &agentproc.Result{CostUSD: 0.42, NumTurns: 2}
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-idle-spend", claimID: claimID,
		namespace: "seedbpr-r-idle-spend", claudeCwd: "/tmp/wt-idle-spend",
	}

	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if !out.hibernated {
		t.Fatalf("expected hibernation, got %+v", out)
	}
	if out.costUSD != 0.42 {
		t.Errorf("outcome costUSD = %v, want 0.42 (the process's reported total)", out.costUSD)
	}
	var stamped sql.NullFloat64
	if err := database.QueryRow(`SELECT cost_usd FROM messages WHERE id = ?`, msgID).Scan(&stamped); err != nil {
		t.Fatalf("read cost_usd: %v", err)
	}
	if !stamped.Valid || stamped.Float64 != 0.42 {
		t.Errorf("claim row cost_usd = %+v, want 0.42 settled at park", stamped)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id='r-idle-spend'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open", status)
	}
}

// TestDriveLiveConversation_FencedParkStillSettlesSpend pins the ordering a
// refused park depends on: the claim was released under the engagement, so
// its own park is refused — and its spend, which nothing else holds, must
// land on its claim's row anyway.
func TestDriveLiveConversation_FencedParkStillSettlesSpend(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-fenced-spend", "sess-fenced-spend", "/tmp/wt-fenced-spend")
	claimID := markEngaged(t, database, "r-fenced-spend")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	msgID, err := s.conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: "r-fenced-spend", Role: "assistant", Content: "working…", Model: "claude-sonnet-4-6", ClaimID: claimID,
	})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	proc := newFakeLiveProc("sess-fenced-spend")
	proc.result = &agentproc.Result{CostUSD: 0.31}
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-fenced-spend", claimID: claimID,
	}

	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	out := s.driveLiveConversation(context.Background(), park, proc, results, nil)

	if !out.fenced {
		t.Fatalf("expected the fenced park to report fenced, got %+v", out)
	}
	if stub.cancels != 1 {
		t.Errorf("fenced park writes = %d, want 1", stub.cancels)
	}
	var stamped sql.NullFloat64
	if err := database.QueryRow(`SELECT cost_usd FROM messages WHERE id = ?`, msgID).Scan(&stamped); err != nil {
		t.Fatalf("read cost_usd: %v", err)
	}
	if !stamped.Valid || stamped.Float64 != 0.31 {
		t.Errorf("claim row cost_usd = %+v, want 0.31 settled despite the refused park", stamped)
	}
}
