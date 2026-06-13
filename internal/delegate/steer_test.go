package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
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
	s.registerProc(runmode.LocalDefaultOrg, "run-live", &agentproc.LiveRun{})

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "run-live", runmode.LocalDefaultUserID, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if fc.steerCalls != 1 || fc.steerRunID != "run-live" || fc.steerText != "hello" {
		t.Errorf("steer = {calls:%d runID:%q text:%q}, want {1 run-live hello}", fc.steerCalls, fc.steerRunID, fc.steerText)
	}
}

// TestSendMessage_OpenRoutesToResume: a run with no live process but status
// `open` is woken via ResumeOpenRun. Proven by the run leaving `open` — a
// durable snapshot satisfies the workspace pre-flight, then the resume goroutine
// fails fast at the cold rehydrate of the stub blob (no subprocess), exactly as
// in TestResumeOpenRun_InitiatesResume; what matters is that resume was entered.
func TestSendMessage_OpenRoutesToResume(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-open", "sess-open", "/tmp/does-not-exist-open")
	if _, err := database.Exec(`UPDATE runs SET status='open' WHERE id='r-open'`); err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	wireBlobStore(t, s)
	putTestSnapshot(t, s, blueprintRunIDForRun(t, database, "r-open"))

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "r-open", runmode.LocalDefaultUserID, "go on"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Join the resume goroutine: it clears its s.cancels entry in the terminal
	// defer after all DB writes, so waiting on that avoids racing the DB close.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, active := s.cancels["r-open"]
		s.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runs WHERE id='r-open'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "open" {
		t.Error("run stayed open — SendMessage did not route to ResumeOpenRun")
	}
	t.Logf("final run status after open-resume: %s", status)
}

// TestSendMessage_TerminalNotSteerable: a terminal finish run (no live process,
// completed without an abort outcome) can take no message — SendMessage returns
// ErrRunNotSteerable for the handler to map to 409.
func TestSendMessage_TerminalNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-done", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE runs SET status='completed' WHERE id='r-done'`); err != nil {
		t.Fatalf("complete: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "r-done", runmode.LocalDefaultUserID, "hi")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable", err)
	}
}

// TestResumableState pins the (status, outcome) wake gate the routing and the
// MarkResuming CAS both key on: every non-finish parked/terminal state is
// resumable, a finish run is not.
func TestResumableState(t *testing.T) {
	cases := []struct {
		status, outcome string
		want            bool
	}{
		{"open", "", true},
		{"pending_approval", "", true},
		{"pending_approval", "finish", true}, // pending is resumable whatever the coerced outcome
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

// TestSendMessage_PendingApprovalIsResumable: a pending_approval run (no live
// process) passes the steerable gate and routes to a resume rather than
// returning ErrRunNotSteerable — the conversation is independent of the queued
// artifact's approval.
func TestSendMessage_PendingApprovalIsResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-pa", "sess-pa", "/tmp/does-not-exist-pa")
	if _, err := database.Exec(`UPDATE runs SET status='pending_approval' WHERE id='r-pa'`); err != nil {
		t.Fatalf("pending_approval: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "r-pa", runmode.LocalDefaultUserID, "carry on")
	if errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("pending_approval run rejected at the steerable gate: %v", err)
	}
	awaitResumeGoroutine(t, s, "r-pa")
}

// TestSendMessage_CompletedAbortIsResumable: a completed+abort run passes the
// steerable gate and routes to a resume (the agent's voluntary stop can be
// picked back up).
func TestSendMessage_CompletedAbortIsResumable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-ab", "sess-ab", "/tmp/does-not-exist-ab")
	if _, err := database.Exec(`UPDATE runs SET status='completed', outcome='abort' WHERE id='r-ab'`); err != nil {
		t.Fatalf("completed+abort: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "r-ab", runmode.LocalDefaultUserID, "pick it back up")
	if errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("completed+abort run rejected at the steerable gate: %v", err)
	}
	awaitResumeGoroutine(t, s, "r-ab")
}

// TestSendMessage_CompletedFinishNotSteerable: a completed+finish run is NOT
// resumable (finish is the one non-finish-state exclusion) → ErrRunNotSteerable.
func TestSendMessage_CompletedFinishNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-fin", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE runs SET status='completed', outcome='finish' WHERE id='r-fin'`); err != nil {
		t.Fatalf("completed+finish: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "r-fin", runmode.LocalDefaultUserID, "more please")
	if !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("err = %v, want ErrRunNotSteerable (finish runs are excluded from resume)", err)
	}
}

// awaitResumeGoroutine joins a resume goroutine deterministically: ResumeOpenRun
// registers s.cancels[runID] before spawning and clears it in the terminal defer
// (after all DB writes), so waiting for that entry to clear avoids racing the
// in-memory DB close on cleanup. A no-op when no goroutine was spawned.
func awaitResumeGoroutine(t *testing.T, s *Spawner, runID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, active := s.cancels[runID]
		s.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resume goroutine for %s did not finish in time", runID)
}

// TestSendMessage_MissingRunNotSteerable: an unknown run id (no process, no
// row) is not steerable.
func TestSendMessage_MissingRunNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrg, "ghost", runmode.LocalDefaultUserID, "hi")
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
