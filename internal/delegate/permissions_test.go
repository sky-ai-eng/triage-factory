package delegate

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// waitForPending blocks until the broker has registered (runID, requestID). The
// handler registers synchronously on entry and then parks, so a test can resolve
// it without racing the handler goroutine. Only safe when permTimeout is far
// longer than a poll interval — the entry must still be pending when the poll
// runs. Tests that shrink permTimeout near the poll interval must use
// waitForPendingOrDone instead.
func waitForPending(t *testing.T, s *Spawner, runID, requestID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.permPending[permKey(runID, requestID)]
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("permission request %q (run %q) never registered", requestID, runID)
}

// waitForPendingOrDone polls until the prompt is observably pending (false) or
// the handler has already returned its decision into got (true, decision in
// *d). With a tiny permTimeout the whole pending window can open and close
// between two polls on a starved runner — registration, timeout, deregistration
// all inside one oversleep — so "handler already finished" must be a navigable
// outcome, not a missed observation that fails the test.
func waitForPendingOrDone(t *testing.T, s *Spawner, runID, requestID string, got <-chan agentproc.PermissionDecision, d *agentproc.PermissionDecision) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, pending := s.permPending[permKey(runID, requestID)]
		s.mu.Unlock()
		if pending {
			return false
		}
		select {
		case *d = <-got:
			return true
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("permission request %q (run %q) neither registered nor returned", requestID, runID)
	return false
}

// TestBrowserPermissionHandler_ResolveAllow: a prompt answered via
// ResolvePermission returns the user's decision to the parked handler.
func TestBrowserPermissionHandler_ResolveAllow(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-1")

	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-1", ToolName: "Bash", Input: map[string]any{"command": "ls"}})
	}()

	waitForPending(t, s, "run-1", "req-1")
	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-1", "req-1", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	select {
	case d := <-got:
		if d.Behavior != "allow" {
			t.Errorf("behavior = %q, want allow", d.Behavior)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not return after resolve")
	}
}

// TestBrowserPermissionHandler_TimeoutDenies: with no answer, the prompt denies
// once permTimeout elapses (made tiny here via a short idle window).
func TestBrowserPermissionHandler_TimeoutDenies(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	s.SetIdleHibernateTimeout(10 * time.Millisecond) // permTimeout = 5ms
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-1")

	d := h(agentproc.PermissionRequest{RequestID: "req-timeout", ToolName: "Bash"})
	if d.Behavior != "deny" {
		t.Errorf("behavior = %q, want deny on timeout", d.Behavior)
	}
	// The handler deregisters on return, resolved or timed out.
	s.mu.Lock()
	_, stillPending := s.permPending[permKey("run-1", "req-timeout")]
	s.mu.Unlock()
	if stillPending {
		t.Error("expected the timed-out request to be deregistered")
	}
}

// TestPermTimeoutBelowIdle pins the load-bearing ordering: the per-prompt wait
// is strictly below the idle-hibernate window, so a prompt never blocks past
// hibernation. Holds for the default and an injected idle.
func TestPermTimeoutBelowIdle(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	if s.permTimeout() >= s.idleTimeout() {
		t.Errorf("permTimeout %v must be < idleTimeout %v (default)", s.permTimeout(), s.idleTimeout())
	}
	s.SetIdleHibernateTimeout(2 * time.Second)
	if s.permTimeout() >= s.idleTimeout() {
		t.Errorf("permTimeout %v must be < idleTimeout %v (injected)", s.permTimeout(), s.idleTimeout())
	}
}

// TestResolvePermission_AcknowledgedResolveNeverDropped stress-pins the
// 200⟹honored invariant at the timeout boundary: whenever ResolvePermission
// returns nil (the endpoint's 200), the parked handler must return that
// decision — even when the resolve lands at the same instant the timer fires
// (select picks randomly between two ready cases) or in the gap before the
// handler deregisters. The staggered sleep walks attempts across the
// deadline so iterations land before, at, and after it; the after-deadline
// ones assert the inverse (no ack ⟹ timeout deny, resolve 404s). An iteration
// whose prompt already timed out before the test could observe it pending
// (loaded runner) is the same after-deadline case, asserted inline.
func TestResolvePermission_AcknowledgedResolveNeverDropped(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	s.SetIdleHibernateTimeout(4 * time.Millisecond) // permTimeout = 2ms
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-race")

	for i := 0; i < 50; i++ {
		reqID := fmt.Sprintf("req-%d", i)
		got := make(chan agentproc.PermissionDecision, 1)
		go func() {
			got <- h(agentproc.PermissionRequest{RequestID: reqID})
		}()

		// On a starved runner the 2ms pending window can elapse entirely
		// between two polls; the handler then already timed out — that
		// iteration is an after-deadline sample, asserted directly here.
		var d agentproc.PermissionDecision
		if done := waitForPendingOrDone(t, s, "run-race", reqID, got, &d); done {
			if d.Behavior != "deny" {
				t.Fatalf("iteration %d: unanswered prompt returned %q, want timeout deny", i, d.Behavior)
			}
			if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-race", reqID, agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
				t.Fatalf("iteration %d: resolve after handler returned: err = %v, want ErrNoPendingPermission", i, err)
			}
			continue
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)

		err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-race", reqID, agentproc.PermissionDecision{Behavior: "allow"})
		if err != nil && !errors.Is(err, ErrNoPendingPermission) {
			t.Fatalf("iteration %d: ResolvePermission: %v", i, err)
		}
		acked := err == nil

		d = <-got
		if acked && d.Behavior != "allow" {
			t.Fatalf("iteration %d: resolve was acknowledged (nil) but handler returned %q (%s) — acknowledged decision dropped", i, d.Behavior, d.Message)
		}
		if !acked && d.Behavior != "deny" {
			t.Fatalf("iteration %d: no resolve acknowledged but handler returned %q", i, d.Behavior)
		}
	}
}

// TestResolvePermission_NoPending: resolving an unknown request id errors so the
// endpoint can 404.
func TestResolvePermission_NoPending(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-1", "ghost", agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission", err)
	}
}

// TestResolvePermission_WrongRun: a resolve addressed to a different run can't
// satisfy a pending prompt — the broker keys by request id but verifies the
// run/tenant the entry was registered for.
func TestResolvePermission_WrongRun(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	s.SetIdleHibernateTimeout(2 * time.Second) // bound the goroutine if cleanup is missed
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-A")
	done := make(chan agentproc.PermissionDecision, 1)
	go func() { done <- h(agentproc.PermissionRequest{RequestID: "req-x"}) }()
	waitForPending(t, s, "run-A", "req-x")

	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-B", "req-x", agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission for a wrong-run resolve", err)
	}

	// The correct run resolves it, freeing the handler goroutine.
	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-A", "req-x", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
		t.Fatalf("correct resolve: %v", err)
	}
	<-done
}

// TestBrowserPermissionHandler_ConcurrentRunsSameRequestID guards the broker
// key: the wrapper's request_id is only unique within a run process, so two
// concurrent runs can both raise "perm-1". Keying by (runID, requestID) keeps
// them isolated — each registers without clobbering the other and each resolves
// to its own decision.
func TestBrowserPermissionHandler_ConcurrentRunsSameRequestID(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	hA := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-A")
	hB := s.BrowserPermissionHandler(runmode.LocalDefaultOrg, "run-B")

	gotA := make(chan agentproc.PermissionDecision, 1)
	gotB := make(chan agentproc.PermissionDecision, 1)
	go func() { gotA <- hA(agentproc.PermissionRequest{RequestID: "perm-1"}) }()
	go func() { gotB <- hB(agentproc.PermissionRequest{RequestID: "perm-1"}) }()
	waitForPending(t, s, "run-A", "perm-1")
	waitForPending(t, s, "run-B", "perm-1")

	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-A", "perm-1", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("resolve run-A: %v", err)
	}
	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-B", "perm-1", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
		t.Fatalf("resolve run-B: %v", err)
	}
	if d := <-gotA; d.Behavior != "allow" {
		t.Errorf("run-A decision = %q, want allow", d.Behavior)
	}
	if d := <-gotB; d.Behavior != "deny" {
		t.Errorf("run-B decision = %q, want deny", d.Behavior)
	}
}
