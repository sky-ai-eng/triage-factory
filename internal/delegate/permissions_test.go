package delegate

import (
	"errors"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// waitForPending blocks until the broker has registered requestID. The handler
// registers synchronously on entry and then parks, so a test can resolve it
// without racing the handler goroutine.
func waitForPending(t *testing.T, s *Spawner, requestID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.permPending[requestID]
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("permission request %q never registered", requestID)
}

// TestBrowserPermissionHandler_ResolveAllow: a prompt answered via
// ResolvePermission returns the user's decision to the parked handler.
func TestBrowserPermissionHandler_ResolveAllow(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "", "")
	h := s.browserPermissionHandler(runmode.LocalDefaultOrg, "run-1")

	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-1", ToolName: "Bash", Input: map[string]any{"command": "ls"}})
	}()

	waitForPending(t, s, "req-1")
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
	h := s.browserPermissionHandler(runmode.LocalDefaultOrg, "run-1")

	d := h(agentproc.PermissionRequest{RequestID: "req-timeout", ToolName: "Bash"})
	if d.Behavior != "deny" {
		t.Errorf("behavior = %q, want deny on timeout", d.Behavior)
	}
	// The handler deregisters on return, resolved or timed out.
	s.mu.Lock()
	_, stillPending := s.permPending["req-timeout"]
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
	h := s.browserPermissionHandler(runmode.LocalDefaultOrg, "run-A")
	done := make(chan agentproc.PermissionDecision, 1)
	go func() { done <- h(agentproc.PermissionRequest{RequestID: "req-x"}) }()
	waitForPending(t, s, "req-x")

	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-B", "req-x", agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission for a wrong-run resolve", err)
	}

	// The correct run resolves it, freeing the handler goroutine.
	if err := s.ResolvePermission(runmode.LocalDefaultOrg, "run-A", "req-x", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
		t.Fatalf("correct resolve: %v", err)
	}
	<-done
}
