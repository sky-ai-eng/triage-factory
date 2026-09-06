package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

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
	// queued is what QueuedTurns answers: the turns the process still owes.
	// A test stages it to model a message steered in while the turn ran —
	// the real process counts its own sends against its results, which a
	// fake fed results by hand cannot.
	queued int
	// onSend fires (with the 1-based send count) after each Send so a test can
	// feed the next turn's result onto the results channel — the way the driver
	// drives the invalid-envelope re-prompt loop.
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

// TestDriveLiveConversation_TerminalResultClosesAndReturns: a turn-terminal result
// closes the process (freeing the session for the gate's resume) and is
// handed back for processCompletion — no hibernation.
func TestDriveLiveConversation_TerminalResultClosesAndReturns(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

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

// TestDriveLiveConversation_CtxCancelReturnsErr: a cancelled ctx (the hard-kill path)
// closes the process and surfaces the ctx error so the caller routes through
// its cancelled handling.
func TestDriveLiveConversation_CtxCancelReturnsErr(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := s.driveLiveConversation(ctx, liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

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
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"exited"}`}
	proc.exit(want, nil) // process already gone, Done() closed, with a result set

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

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

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

	if out.result != nil {
		t.Errorf("result = %+v, want nil on a crash exit", out.result)
	}
	if !errors.Is(out.err, wantErr) {
		t.Errorf("err = %v, want %v", out.err, wantErr)
	}
}

// TestDriveLiveConversation_IdleHibernates is the acceptance check for idle
// hibernation: a live run quiet past the (injected short) threshold closes
// its process and parks to `open`, keeping the worktree.
func TestDriveLiveConversation_IdleHibernates(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-idle", "sess-idle", "/tmp/wt-idle")
	markEngaged(t, database, "r-idle")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-idle'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-idle")
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-idle", taskID: taskID,
		namespace: "seedbpr-r-idle", claudeCwd: "/tmp/wt-idle",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
	}

	out := s.driveLiveConversation(context.Background(), park, proc, make(chan *agentproc.Result), make(chan struct{}), 20*time.Millisecond)

	if !out.hibernated {
		t.Fatalf("expected hibernation, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("expected the idle hibernation to close the process")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id='r-idle'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (idle hibernation flips to open)", status)
	}
}

// TestDriveLiveConversation_ActivityDefersHibernation pins the activity-reset: a
// slow-but-working agent (steady stream activity) must NOT hibernate. We pump
// activity well past the idle window, then deliver a terminal result — if the
// timer reset correctly the run returns the result rather than hibernating.
func TestDriveLiveConversation_ActivityDefersHibernation(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	activity := make(chan struct{}, 8)

	done := make(chan liveOutcome, 1)
	go func() {
		done <- s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, activity, 100*time.Millisecond)
	}()

	// Pump activity every 20ms for 400ms — 4x the idle window. If the reset
	// works, no hibernation fires in that span.
	deadline := time.After(400 * time.Millisecond)
	pump := time.NewTicker(20 * time.Millisecond)
	defer pump.Stop()
pumping:
	for {
		select {
		case <-deadline:
			break pumping
		case <-pump.C:
			activity <- struct{}{}
		}
	}
	results <- &agentproc.Result{Result: `{"outcome":"finish","summary":"ok"}`}

	out := <-done
	if out.hibernated {
		t.Error("steady activity should reset the idle timer; expected a terminal result, got hibernation")
	}
	if out.result == nil {
		t.Error("expected the terminal result after activity stopped")
	}
}

// TestDriveLiveConversation_NoneClosesAndHandsBack: a turn that ends with no
// conclusion (prose) and nothing queued behind it closes the process and hands
// the result back for processCompletion to park — the driver does not keep the
// process for a message that has not arrived. A second result left on the
// channel proves it: the driver returned on the prose and never read it.
func TestDriveLiveConversation_NoneClosesAndHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	prose := &agentproc.Result{Result: "Some prose with no completion envelope at all."}
	results <- prose
	results <- &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.result != prose {
		t.Errorf("result = %+v, want the no-conclusion turn handed back", out.result)
	}
	if out.hibernated {
		t.Error("a no-conclusion turn is parked by processCompletion, not by the driver")
	}
	if !proc.wasClosed() {
		t.Error("a no-conclusion turn with nothing queued must close the process")
	}
	if len(results) != 1 {
		t.Errorf("results left = %d, want 1 (the driver must not read past the turn it returned)", len(results))
	}
	if proc.sends() != 0 {
		t.Errorf("sends = %d, want 0 (a no-conclusion turn is not re-prompted)", proc.sends())
	}
}

// TestDriveLiveConversation_QueuedTurnOutlivesTheTurnEnd: a message steered in
// while the turn ran is queued by the process and starts the next turn, so a
// no-conclusion end with a turn still owed is not a park — the driver stays
// on the same process, under the same claim, and reads the turn the steer
// produces. Delivering prose first and a valid conclusion second proves it:
// the conclusion is only reachable by staying past the prose.
func TestDriveLiveConversation_QueuedTurnOutlivesTheTurnEnd(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	proc.owe(1)
	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{Result: "Some prose; the steered turn runs next."}
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion the queued turn produced", out.result)
	}
	if out.hibernated {
		t.Error("a turn end with a turn still owed must not park")
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
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.result != want {
		t.Errorf("result = %+v, want the conclusion the queued turn produced", out.result)
	}
	if out.hibernated {
		t.Error("a pause with a turn still owed must not park")
	}
}

// TestDriveLiveConversation_InvalidRepromptsToBoundThenHandsBack: an envelope-shaped but
// invalid conclusion is re-prompted to fix, up to maxCompletionRetries. When
// the bound is exhausted the driver hands the (still invalid) result back —
// keeping the totals the live process folded — rather than dropping them on a
// bare error; processCompletion records the failure (see
// TestProcessCompletion_InvalidEnvelopeFails). Each correction here produces
// another invalid turn, so the bound is exhausted.
func TestDriveLiveConversation_InvalidRepromptsToBoundThenHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	invalid := &agentproc.Result{Result: `{"outcome":"frobnicate"}`}
	proc.onSend = func(int) { results <- invalid } // every correction yields another invalid turn
	results <- invalid                             // the initial invalid turn

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.err != nil {
		t.Fatalf("expected the unfixed result handed back, got err %v", out.err)
	}
	if out.result != invalid {
		t.Errorf("result = %+v, want the unfixed invalid result for processCompletion to fail", out.result)
	}
	if proc.sends() != maxCompletionRetries {
		t.Errorf("sends = %d, want %d (one correction per retry)", proc.sends(), maxCompletionRetries)
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed after exhausting re-prompts")
	}
}

// TestDriveLiveConversation_BoundedResumeRepromptsInvalid: a bounded resume (idleTimeout
// 0) holds a live process too, so it re-prompts an invalid envelope in place
// just like an autonomous run — the fix for the asymmetry where a resume used
// to accept an invalid envelope uncorrected.
func TestDriveLiveConversation_BoundedResumeRepromptsInvalid(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"fixed"}`}
	proc.onSend = func(int) { results <- want } // the correction lands a valid conclusion
	results <- &agentproc.Result{Result: `{"outcome":"abort"}`}

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), 0)

	if out.result != want {
		t.Errorf("result = %+v, want the corrected valid conclusion", out.result)
	}
	if proc.sends() != 1 {
		t.Errorf("sends = %d, want 1 (resume re-prompts invalid once, then accepts)", proc.sends())
	}
}

// TestDriveLiveConversation_BoundedResumeNoneHandsBack: with no idle backstop
// armed (idleTimeout 0, the resume's shape) a no-conclusion turn takes the same
// exit as with one — the process is closed and the result handed back for
// processCompletion to park.
func TestDriveLiveConversation_BoundedResumeNoneHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	none := &agentproc.Result{Result: "prose, no envelope"}
	results <- none

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), 0)

	if out.result != none {
		t.Errorf("result = %+v, want the no-conclusion result handed back", out.result)
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed on the no-conclusion turn")
	}
}

// TestDriveLiveConversation_InvalidThenValidReturns: an invalid conclusion is re-prompted
// once and the corrected turn is a valid conclusion → the driver returns it
// (no failure).
func TestDriveLiveConversation_InvalidThenValidReturns(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"fixed"}`}
	proc.onSend = func(int) { results <- want } // the correction lands a valid conclusion
	results <- &agentproc.Result{Result: `{"outcome":"finish"}`}

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.err != nil {
		t.Fatalf("expected success after one correction, got err %v", out.err)
	}
	if out.result != want {
		t.Errorf("result = %+v, want the corrected valid conclusion", out.result)
	}
	if proc.sends() != 1 {
		t.Errorf("sends = %d, want 1 (corrected on the first re-prompt)", proc.sends())
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
// fix depends on: a no-conclusion turn does NOT park from inside the driver.
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
	none := &agentproc.Result{Result: "prose, no completion envelope"}
	results <- none
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-none", taskID: taskID,
		namespace: "seedbpr-r-none", claudeCwd: "/tmp/wt-none",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}

	out := s.driveLiveConversation(context.Background(), park, proc, results, make(chan struct{}), time.Minute)
	if out.result != none {
		t.Fatalf("result = %+v, want the no-conclusion turn handed back", out.result)
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
	results <- &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrgID, conversationID: "r-pause", taskID: taskID,
		namespace: "seedbpr-r-pause", claudeCwd: "/tmp/wt-pause",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
		claimID: claimID, reason: db.ParkIdle(), runtime: domain.ConversationRuntimeSDK,
	}

	out := s.driveLiveConversation(context.Background(), park, proc, results, make(chan struct{}), time.Minute)
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

	out := s.driveLiveConversation(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrgID, conversationID: "r-err"}, proc, results, make(chan struct{}), time.Minute)
	if out.result != r {
		t.Fatalf("expected the error result back, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("a terminal error should close the process")
	}
}

// TestDriveLiveConversation_InterruptBoundedResumeParksOpen: a pause with no
// idle backstop armed (the resume's shape) takes the same exit — the driver
// closes the process and parks the run open to a durable resume.
func TestDriveLiveConversation_InterruptBoundedResumeParksOpen(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess-bounded")

	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	out := s.driveLiveConversation(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrgID, conversationID: "r-bounded"}, proc, results, make(chan struct{}), 0)
	if !out.hibernated {
		t.Fatalf("bounded-resume pause should park open (hibernated), got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("bounded-resume pause should close the process")
	}
}

// TestFoldAccounting_PauseDoesNotPoisonConclusion pins the merge-fold split
// that keeps a paused run's valid conclusion valid: MergeResult's IsError is
// deliberately sticky across turns, so a benign interrupted (pause) turn
// earlier in the conversation infects the accounting fold — the disposition
// processCompletion acts on must come from the turn the driver classified,
// with only cost/duration/turns taken cumulatively.
func TestFoldAccounting_PauseDoesNotPoisonConclusion(t *testing.T) {
	pause := &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true, CostUSD: 0.25, DurationMs: 1000, NumTurns: 15}
	conclusion := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`, Subtype: "success", CostUSD: 0.5, DurationMs: 500, NumTurns: 5}
	merged := agentproc.MergeResult(pause, conclusion)
	if !merged.IsError || !merged.Interrupted {
		t.Fatal("precondition: MergeResult keeps IsError/Interrupted sticky across turns")
	}

	got := foldAccounting(conclusion, merged)
	if got.IsError || got.Interrupted {
		t.Errorf("disposition must come from the classified turn: IsError=%v Interrupted=%v", got.IsError, got.Interrupted)
	}
	if got.Subtype != "success" || got.Result != conclusion.Result {
		t.Errorf("classified turn's envelope/subtype must survive the fold: %+v", got)
	}
	if got.CostUSD != 0.75 || got.DurationMs != 1500 || got.NumTurns != 20 {
		t.Errorf("accounting must be cumulative: cost=%v dur=%v turns=%v", got.CostUSD, got.DurationMs, got.NumTurns)
	}
	if conclusion.CostUSD != 0.5 {
		t.Error("foldAccounting must not mutate the classified result in place")
	}
}
