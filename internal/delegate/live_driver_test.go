package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeLiveProc stands in for *agentproc.LiveRun in the driver tests so
// driveLiveRun can be exercised without spawning a subprocess. Close()
// closes done (idempotent) so a subsequent select on Done() doesn't block.
type fakeLiveProc struct {
	mu        sync.Mutex
	done      chan struct{}
	closed    bool
	sessionID string
	result    *agentproc.Result
	err       error
	sendCount int
	// onSend fires (with the 1-based send count) after each Send so a test can
	// feed the next turn's result onto the results channel — the way the driver
	// drives the invalid-envelope re-prompt loop.
	onSend func(count int)
}

func newFakeLiveProc(sessionID string) *fakeLiveProc {
	return &fakeLiveProc{done: make(chan struct{}), sessionID: sessionID}
}

func (f *fakeLiveProc) Done() <-chan struct{}     { return f.done }
func (f *fakeLiveProc) SessionID() string         { return f.sessionID }
func (f *fakeLiveProc) Stderr() string            { return "" }
func (f *fakeLiveProc) Result() *agentproc.Result { f.mu.Lock(); defer f.mu.Unlock(); return f.result }
func (f *fakeLiveProc) Err() error                { f.mu.Lock(); defer f.mu.Unlock(); return f.err }

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

// TestDriveLiveRun_TerminalResultClosesAndReturns: a turn-terminal result
// closes the process (freeing the session for the gate's resume) and is
// handed back for processCompletion — no hibernation.
func TestDriveLiveRun_TerminalResultClosesAndReturns(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

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

// TestDriveLiveRun_CtxCancelReturnsErr: a cancelled ctx (the hard-kill path)
// closes the process and surfaces the ctx error so the caller routes through
// its cancelled handling.
func TestDriveLiveRun_CtxCancelReturnsErr(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := s.driveLiveRun(ctx, liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

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

// TestDriveLiveRun_ProcessExitCarriesResult covers the proc.Done() branch:
// when the process exits on its own (rather than us closing it off the
// results channel), the driver hands back the folded result for
// processCompletion — and does not hibernate.
func TestDriveLiveRun_ProcessExitCarriesResult(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"exited"}`}
	proc.exit(want, nil) // process already gone, Done() closed, with a result set

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

	if out.result != want {
		t.Errorf("result = %+v, want %+v", out.result, want)
	}
	if out.hibernated {
		t.Error("did not expect hibernation on a process exit")
	}
}

// TestDriveLiveRun_ProcessExitCarriesErr is the crash half of the proc.Done()
// branch: an exit with no result surfaces the process error so the caller
// routes through failRun.
func TestDriveLiveRun_ProcessExitCarriesErr(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	wantErr := errors.New("agent runtime exited with error")
	proc.exit(nil, wantErr)

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), make(chan struct{}), time.Minute)

	if out.result != nil {
		t.Errorf("result = %+v, want nil on a crash exit", out.result)
	}
	if !errors.Is(out.err, wantErr) {
		t.Errorf("err = %v, want %v", out.err, wantErr)
	}
}

// TestDriveLiveRun_IdleHibernates is the acceptance check for idle
// hibernation: a live run quiet past the (injected short) threshold closes
// its process and parks to `open`, keeping the worktree.
func TestDriveLiveRun_IdleHibernates(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-idle", "sess-idle", "/tmp/wt-idle")
	if _, err := database.Exec(`UPDATE runs SET status='running' WHERE id='r-idle'`); err != nil {
		t.Fatalf("set running: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM runs WHERE id='r-idle'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-idle")
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrg, runID: "r-idle", taskID: taskID,
		namespace: "seedbpr-r-idle", claudeCwd: "/tmp/wt-idle",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
	}

	out := s.driveLiveRun(context.Background(), park, proc, make(chan *agentproc.Result), make(chan struct{}), 20*time.Millisecond)

	if !out.hibernated {
		t.Fatalf("expected hibernation, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("expected the idle hibernation to close the process")
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-idle'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (idle hibernation flips to open)", status)
	}
}

// TestDriveLiveRun_ActivityDefersHibernation pins the activity-reset: a
// slow-but-working agent (steady stream activity) must NOT hibernate. We pump
// activity well past the idle window, then deliver a terminal result — if the
// timer reset correctly the run returns the result rather than hibernating.
func TestDriveLiveRun_ActivityDefersHibernation(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	activity := make(chan struct{}, 8)

	done := make(chan liveOutcome, 1)
	go func() {
		done <- s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, activity, 100*time.Millisecond)
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

// TestDriveLiveRun_NoneKeepsProcOpenAndLoops: a turn that ends with no
// conclusion (prose) must NOT close the process or hibernate — the run is
// open, so the driver keeps the process warm and loops. We prove the loop by
// delivering prose first and a valid conclusion second: the driver returns the
// valid result, which it could only reach by looping past the prose.
func TestDriveLiveRun_NoneKeepsProcOpenAndLoops(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{Result: "Some prose with no completion envelope at all."}
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

	if out.result != want {
		t.Errorf("result = %+v, want the valid conclusion reached after looping past prose", out.result)
	}
	if out.hibernated {
		t.Error("a no-conclusion turn must not hibernate while results keep arriving")
	}
	if proc.sends() != 0 {
		t.Errorf("sends = %d, want 0 (a no-conclusion turn is not re-prompted)", proc.sends())
	}
}

// TestDriveLiveRun_InvalidRepromptsToBoundThenHandsBack: an envelope-shaped but
// invalid conclusion is re-prompted to fix, up to maxCompletionRetries. When
// the bound is exhausted the driver hands the (still invalid) result back —
// keeping the totals the live process folded — rather than dropping them on a
// bare error; processCompletion records the failure (see
// TestProcessCompletion_InvalidEnvelopeFails). Each correction here produces
// another invalid turn, so the bound is exhausted.
func TestDriveLiveRun_InvalidRepromptsToBoundThenHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	invalid := &agentproc.Result{Result: `{"outcome":"frobnicate"}`}
	proc.onSend = func(int) { results <- invalid } // every correction yields another invalid turn
	results <- invalid                             // the initial invalid turn

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

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

// TestDriveLiveRun_BoundedResumeRepromptsInvalid: a bounded resume (idleTimeout
// 0) holds a live process too, so it re-prompts an invalid envelope in place
// just like an autonomous run — the fix for the asymmetry where a resume used
// to accept an invalid envelope uncorrected.
func TestDriveLiveRun_BoundedResumeRepromptsInvalid(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"fixed"}`}
	proc.onSend = func(int) { results <- want } // the correction lands a valid conclusion
	results <- &agentproc.Result{Result: `{"outcome":"abort"}`}

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), 0)

	if out.result != want {
		t.Errorf("result = %+v, want the corrected valid conclusion", out.result)
	}
	if proc.sends() != 1 {
		t.Errorf("sends = %d, want 1 (resume re-prompts invalid once, then accepts)", proc.sends())
	}
}

// TestDriveLiveRun_BoundedResumeNoneHandsBack: a bounded resume (idleTimeout 0)
// has no idle timer to close a warm process, so a no-conclusion turn is handed
// straight back (not looped) for processCompletion to mark open.
func TestDriveLiveRun_BoundedResumeNoneHandsBack(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 1)
	none := &agentproc.Result{Result: "prose, no envelope"}
	results <- none

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), 0)

	if out.result != none {
		t.Errorf("result = %+v, want the no-conclusion result handed back", out.result)
	}
	if !proc.wasClosed() {
		t.Error("expected the process closed (a bounded resume does not keep it warm)")
	}
}

// TestDriveLiveRun_InvalidThenValidReturns: an invalid conclusion is re-prompted
// once and the corrected turn is a valid conclusion → the driver returns it
// (no failure).
func TestDriveLiveRun_InvalidThenValidReturns(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess")
	results := make(chan *agentproc.Result, 8)
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"fixed"}`}
	proc.onSend = func(int) { results <- want } // the correction lands a valid conclusion
	results <- &agentproc.Result{Result: `{"outcome":"finish"}`}

	out := s.driveLiveRun(context.Background(), liveParkContext{}, proc, results, make(chan struct{}), time.Minute)

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

// TestDriveLiveRun_NoneFlipsStatusOpen: an autonomous turn that ends without a
// conclusion flips the run to `open` in the DB while the process stays warm —
// so a crash in the warm window leaves an open run (which the boot reconcile
// leaves alone) rather than a running one (which it would re-queue). We deliver
// a no-conclusion turn followed by a valid one: the driver returns the valid
// result, and the row is left `open` from the no-conclusion turn (driveLiveRun
// records the conclusion via processCompletion, not here).
func TestDriveLiveRun_NoneFlipsStatusOpen(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-none", "sess-none", "/tmp/wt-none")
	if _, err := database.Exec(`UPDATE runs SET status='running' WHERE id='r-none'`); err != nil {
		t.Fatalf("set running: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM runs WHERE id='r-none'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-none")
	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{Result: "prose, no completion envelope"}
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrg, runID: "r-none", taskID: taskID,
		namespace: "seedbpr-r-none", claudeCwd: "/tmp/wt-none",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
	}

	out := s.driveLiveRun(context.Background(), park, proc, results, make(chan struct{}), time.Minute)
	if out.result != want {
		t.Fatalf("result = %+v, want the valid conclusion after the open turn", out.result)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-none'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (a no-conclusion turn flips the run open)", status)
	}
}

// TestDriveLiveRun_InterruptParksOpenNotTerminal pins the pause semantics: a
// result the reader marked Interrupted (our own interrupt() ended the turn)
// is a pause, not a failure — the run flips open, the process stays warm, and
// the driver keeps consuming turns. The follow-up valid conclusion proves the
// loop survived the pause.
func TestDriveLiveRun_InterruptParksOpenNotTerminal(t *testing.T) {
	database := newTakeoverTestDB(t)
	seedRun(t, database, "r-pause", "sess-pause", "/tmp/wt-pause")
	if _, err := database.Exec(`UPDATE runs SET status='running' WHERE id='r-pause'`); err != nil {
		t.Fatalf("set running: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6", "")

	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM runs WHERE id='r-pause'`).Scan(&taskID); err != nil {
		t.Fatalf("read task_id: %v", err)
	}
	proc := newFakeLiveProc("sess-pause")

	results := make(chan *agentproc.Result, 8)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	want := &agentproc.Result{Result: `{"outcome":"finish","summary":"done"}`}
	results <- want
	park := liveParkContext{
		orgID: runmode.LocalDefaultOrg, runID: "r-pause", taskID: taskID,
		namespace: "seedbpr-r-pause", claudeCwd: "/tmp/wt-pause",
		triggerType: "manual", creatorUserID: runmode.LocalDefaultUserID,
	}

	out := s.driveLiveRun(context.Background(), park, proc, results, make(chan struct{}), time.Minute)
	if out.result != want {
		t.Fatalf("result = %+v, want the valid conclusion after the pause (pause must not be terminal)", out.result)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-pause'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (the paused turn flips the run open)", status)
	}
}

// TestDriveLiveRun_ErrorWithoutInterruptStaysTerminal pins the inverse: the
// same is_error/error_during_execution shape with NO pause in flight is a
// genuine failure and stays terminal.
func TestDriveLiveRun_ErrorWithoutInterruptStaysTerminal(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess-err")
	results := make(chan *agentproc.Result, 1)
	r := &agentproc.Result{IsError: true, Subtype: "error_during_execution"}
	results <- r

	out := s.driveLiveRun(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrg, runID: "r-err"}, proc, results, make(chan struct{}), time.Minute)
	if out.result != r {
		t.Fatalf("expected the error result back, got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("a terminal error should close the process")
	}
}

// TestDriveLiveRun_InterruptBoundedResumeParksOpen: pausing during a bounded
// resume (no idle timer) can't loop — the driver closes the process and parks
// the run open to a durable resume.
func TestDriveLiveRun_InterruptBoundedResumeParksOpen(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	proc := newFakeLiveProc("sess-bounded")

	results := make(chan *agentproc.Result, 1)
	results <- &agentproc.Result{IsError: true, Subtype: "error_during_execution", Interrupted: true}
	out := s.driveLiveRun(context.Background(), liveParkContext{orgID: runmode.LocalDefaultOrg, runID: "r-bounded"}, proc, results, make(chan struct{}), 0)
	if !out.hibernated {
		t.Fatalf("bounded-resume pause should park open (hibernated), got %+v", out)
	}
	if !proc.wasClosed() {
		t.Error("bounded-resume pause should close the process")
	}
}
