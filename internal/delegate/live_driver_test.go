package delegate

import (
	"context"
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
}

func newFakeLiveProc(sessionID string) *fakeLiveProc {
	return &fakeLiveProc{done: make(chan struct{}), sessionID: sessionID}
}

func (f *fakeLiveProc) Done() <-chan struct{}     { return f.done }
func (f *fakeLiveProc) SessionID() string         { return f.sessionID }
func (f *fakeLiveProc) Stderr() string            { return "" }
func (f *fakeLiveProc) Result() *agentproc.Result { f.mu.Lock(); defer f.mu.Unlock(); return f.result }
func (f *fakeLiveProc) Err() error                { f.mu.Lock(); defer f.mu.Unlock(); return f.err }

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

// TestDriveLiveRun_IdleHibernates is the acceptance check for idle
// hibernation: a live run quiet past the (injected short) threshold closes
// its process and parks to awaiting_input, keeping the worktree.
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
	if status != "awaiting_input" {
		t.Errorf("status = %q, want awaiting_input (idle hibernation reuses it, no new status)", status)
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
	results <- &agentproc.Result{Result: "ok"}

	out := <-done
	if out.hibernated {
		t.Error("steady activity should reset the idle timer; expected a terminal result, got hibernation")
	}
	if out.result == nil {
		t.Error("expected the terminal result after activity stopped")
	}
}
