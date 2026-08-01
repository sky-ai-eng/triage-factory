package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeController records control-seam calls so the routing in SendMessage and
// Interrupt can be asserted without spawning a live subprocess. It satisfies
// RunController; the real inProcessController is exercised separately.
type fakeController struct {
	mu             sync.Mutex
	steerCalls     int
	steerRunID     string
	steerText      string
	interruptCalls int
	interruptRunID string
}

func (f *fakeController) Interrupt(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	f.interruptRunID = runID
	return nil
}

func (f *fakeController) Steer(_ context.Context, runID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerCalls++
	f.steerRunID = runID
	f.steerText = text
	return nil
}

func (f *fakeController) Cancel(string) bool { return false }

// TestSendMessage_LiveRoutesToSteer: a run with a registered live process is
// steered in place through the control seam — asserted via a fake controller.
func TestSendMessage_LiveRoutesToSteer(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc
	// getProc is SendMessage's liveness gate; register a handle so it routes to
	// the live path. The handle's LiveRun is never touched — the fake controller
	// stands in for the real Steer.
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "run-live", runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 || fc.steerRunID != "run-live" || fc.steerText != "hello" {
		t.Errorf("steer = {calls:%d runID:%q text:%q}, want {1 run-live hello}", fc.steerCalls, fc.steerRunID, fc.steerText)
	}
}

// TestSendMessage_OpenRoutesToResume: a run with no live process but status
// `open` is woken via ResumeOpenRun — resume-by-enqueue, so "woken" means
// the row flips to `queued` and the message lands as durable pending
// input, not that a goroutine ran. Delivery is a separate,
// later concern exercised by TestDispatchResumeClaim_DeliversRecordedInput.
func TestSendMessage_OpenRoutesToResume(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-open", "sess-open", "/tmp/does-not-exist-open")
	if _, err := database.Exec(`UPDATE conversations SET status='open' WHERE id='r-open'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-open", runmode.LocalDefaultUserID, "go on"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if st := storedStatus(t, database, "r-open"); st != "" {
		t.Errorf("stored status = %q, want none — SendMessage did not route to ResumeOpenRun's resume-by-enqueue", st)
	}
	msg, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-open")
	if err != nil || !ok || msg != "go on" {
		t.Errorf("pending input = (ok=%v msg=%q err=%v), want (true, %q, nil)", ok, msg, err, "go on")
	}
}

// TestSendMessage_RunningRemoteRoutesToController pins the multi-mode fix: a
// running run with NO process in THIS pod's registry — the shape a control pod
// always sees, since a running run's process lives on an executor — must route
// through the controller (which delivers over conversation_signals to the owner), NOT be
// misread as unsteerable because the LOCAL registry missed. Before the fix this
// fell through to the resumable check (running isn't resumable) and 409'd.
func TestSendMessage_RunningRemoteRoutesToController(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-run", "sess-run", "/tmp/wt-run")
	if _, err := database.Exec(`UPDATE conversations SET status='running' WHERE id='r-run'`); err != nil {
		t.Fatalf("running: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc
	// Deliberately do NOT registerProc: getProc stays nil, exactly as on a control
	// pod whose running run's process lives on an executor. The controller (real
	// crossPodController in production) is what resolves the remote owner; here the
	// fake stands in to assert the routing decision, not the delivery.

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-run", runmode.LocalDefaultUserID, "steer me"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 || fc.steerRunID != "r-run" || fc.steerText != "steer me" {
		t.Errorf("steer = {calls:%d runID:%q text:%q}, want {1 r-run \"steer me\"} — a running-but-remote run must route to the controller, not 409", fc.steerCalls, fc.steerRunID, fc.steerText)
	}
}

// TestSendMessage_QueuedNotSteerable pins the boundary the status switch draws:
// a queued run is claimed by no one yet, so there is no owner to signal and it is
// not yet parked to resume — it must be ErrRunNotSteerable, NOT routed to the
// controller (which would find no owner). Guards against widening the active
// branch to pre-claim states.
func TestSendMessage_QueuedNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-q", "sess-q", "/tmp/wt-q")
	if _, err := database.Exec(`UPDATE conversations SET status = NULL WHERE id='r-q'`); err != nil {
		t.Fatalf("queued: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fc := &fakeController{}
	s.controller = fc

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-q", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable for a queued run", err)
	}
	if fc.steerCalls != 0 {
		t.Errorf("steer calls = %d, want 0 — a queued run has no owner to steer", fc.steerCalls)
	}
}

// TestSendMessage_TerminalNotSteerable: a terminal finish run (no live process,
// completed without an abort outcome) can take no message — SendMessage returns
// ErrRunNotSteerable for the handler to map to 409.
func TestSendMessage_TerminalNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-done", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status='completed' WHERE id='r-done'`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-done", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable", err)
	}
}

// TestResumableState pins the (status, outcome) wake gate the routing and the
// MarkQueuedForResume CAS both key on: every non-finish parked/terminal state
// is resumable, a finish run is not.
func TestResumableState(t *testing.T) {
	cases := []struct {
		status, outcome string
		want            bool
	}{
		{"open", "", true},
		{"completed", "abort", true},
		{"completed", "finish", false}, // finish is the one terminal excluded
		{"completed", "", false},
		{"running", "", false},
		{"queued", "", false},
		{"failed", "", false},
		{"cancelled", "abort", false}, // status gates first — only completed pairs with abort
	}
	for _, tc := range cases {
		if got := resumableState(tc.status, tc.outcome); got != tc.want {
			t.Errorf("resumableState(%q, %q) = %v, want %v", tc.status, tc.outcome, got, tc.want)
		}
	}
}

// TestSendMessage_CompletedAbortIsResumable: a completed+abort run passes the
// steerable gate and routes to a resume (the agent's voluntary stop can be
// picked back up). This is also the path a terminal run with an unresolved
// artifact resumes through now that pending_approval is gone.
func TestSendMessage_CompletedAbortIsResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-ab", "sess-ab", "/tmp/does-not-exist-ab")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='abort' WHERE id='r-ab'`); err != nil {
		t.Fatalf("completed+abort: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-ab", runmode.LocalDefaultUserID, "pick it back up")
	if errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("completed+abort run rejected at the steerable gate: %v", err)
	}
	if st := storedStatus(t, database, "r-ab"); st != "" {
		t.Errorf("stored status = %q, want none (resume-by-enqueue's un-terminal write)", st)
	}
}

// TestSendMessage_CompletedFinishNotSteerable: a completed+finish run is NOT
// resumable (finish is the one non-finish-state exclusion) → ErrRunNotSteerable.
func TestSendMessage_CompletedFinishNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-fin", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-fin'`); err != nil {
		t.Fatalf("completed+finish: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-fin", runmode.LocalDefaultUserID, "more please")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable (finish runs are excluded from resume)", err)
	}
}

// TestSendMessage_MissingRunNotSteerable: an unknown run id (no process, no
// row) is not steerable.
func TestSendMessage_MissingRunNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "ghost", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable", err)
	}
}

// TestInterrupt_LiveRoutesToController: Spawner.Interrupt drives the live
// process through the control seam — asserted via a fake controller.
func TestInterrupt_LiveRoutesToController(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc

	if err := s.Interrupt(context.Background(), "run-live"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if fc.interruptCalls != 1 || fc.interruptRunID != "run-live" {
		t.Errorf("interrupt = {calls:%d runID:%q}, want {1 run-live}", fc.interruptCalls, fc.interruptRunID)
	}
}
