package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-1", AbsentAutoDeny{})

	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-1", ToolName: "Bash", Input: map[string]any{"command": "ls"}})
	}()

	waitForPending(t, s, "run-1", "req-1")
	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-1", "req-1", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetIdleHibernateTimeout(10 * time.Millisecond) // permTimeout = 5ms
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-1", AbsentAutoDeny{})

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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetIdleHibernateTimeout(4 * time.Millisecond) // permTimeout = 2ms
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-race", AbsentAutoDeny{})

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
			if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-race", reqID, agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
				t.Fatalf("iteration %d: resolve after handler returned: err = %v, want ErrNoPendingPermission", i, err)
			}
			continue
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)

		err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-race", reqID, agentproc.PermissionDecision{Behavior: "allow"})
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-1", "ghost", agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission", err)
	}
}

// TestResolvePermission_WrongRun: a resolve addressed to a different run can't
// satisfy a pending prompt — the broker keys by request id but verifies the
// run/tenant the entry was registered for.
func TestResolvePermission_WrongRun(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetIdleHibernateTimeout(2 * time.Second) // bound the goroutine if cleanup is missed
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-A", AbsentAutoDeny{})
	done := make(chan agentproc.PermissionDecision, 1)
	go func() { done <- h(agentproc.PermissionRequest{RequestID: "req-x"}) }()
	waitForPending(t, s, "run-A", "req-x")

	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-B", "req-x", agentproc.PermissionDecision{Behavior: "allow"}); !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission for a wrong-run resolve", err)
	}

	// The correct run resolves it, freeing the handler goroutine.
	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-A", "req-x", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
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
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	hA := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-A", AbsentAutoDeny{})
	hB := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-B", AbsentAutoDeny{})

	gotA := make(chan agentproc.PermissionDecision, 1)
	gotB := make(chan agentproc.PermissionDecision, 1)
	go func() { gotA <- hA(agentproc.PermissionRequest{RequestID: "perm-1"}) }()
	go func() { gotB <- hB(agentproc.PermissionRequest{RequestID: "perm-1"}) }()
	waitForPending(t, s, "run-A", "perm-1")
	waitForPending(t, s, "run-B", "perm-1")

	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-A", "perm-1", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("resolve run-A: %v", err)
	}
	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-B", "perm-1", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
		t.Fatalf("resolve run-B: %v", err)
	}
	if d := <-gotA; d.Behavior != "allow" {
		t.Errorf("run-A decision = %q, want allow", d.Behavior)
	}
	if d := <-gotB; d.Behavior != "deny" {
		t.Errorf("run-B decision = %q, want deny", d.Behavior)
	}
}

// dialHubClient stands up an httptest server that registers the dialing
// connection with the hub (unscoped, so it receives every event) and returns the
// live client conn. Used by the broadcast tests below to read frames off the
// wire — the only seam into a concrete *websocket.Hub from outside its package.
func dialHubClient(t *testing.T, h *websocket.Hub) (*ws.Conn, func()) {
	t.Helper()
	return dialHubClientOrg(t, h, "")
}

// dialHubClientOrg is dialHubClient with an explicit org on the registered
// connection, for the presence tests that need a client scoped to a tenant.
func dialHubClientOrg(t *testing.T, h *websocket.Hub, orgID string) (*ws.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleWS(w, r, "user", orgID, "")
	}))
	url := strings.Replace(srv.URL, "http://", "ws://", 1)
	conn, _, err := ws.Dial(context.Background(), url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial hub: %v", err)
	}
	return conn, func() {
		_ = conn.Close(ws.StatusNormalClosure, "")
		srv.Close()
	}
}

// sendPresence writes a presence control frame the way the frontend does, then
// blocks until the hub observes the expected presence for (orgID, runID) — so a
// test that depends on a client being present (or absent) doesn't race the
// async readPump apply.
func sendPresence(t *testing.T, conn *ws.Conn, h *websocket.Hub, orgID, runID, viewing string, visible bool, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame := fmt.Sprintf(`{"type":"presence","viewing":%q,"visible":%t}`, viewing, visible)
	if err := conn.Write(ctx, ws.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write presence frame: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.PresentFor(orgID, runID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("hub presence for (%s,%s) never became %v", orgID, runID, want)
}

const presenceTestOrg = "00000000-0000-0000-0000-0000000000aa"

// TestBrowserPermissionHandler_AbsentDeniesAfterGrace: with absent-auto-deny on
// and no answer-capable, focused tab present, an off-allowlist prompt denies
// after ~the grace with the distinct "no operator available" reason — not after
// the full permTimeout().
func TestBrowserPermissionHandler_AbsentDeniesAfterGrace(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetIdleHibernateTimeout(30 * time.Second) // permTimeout = 15s (the full ceiling)
	s.SetPresencePollInterval(5 * time.Millisecond)
	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{enabled: true, grace: 40 * time.Millisecond})

	start := time.Now()
	d := h(agentproc.PermissionRequest{RequestID: "req-absent", ToolName: "Bash"})
	elapsed := time.Since(start)

	if d.Behavior != "deny" {
		t.Fatalf("behavior = %q, want deny", d.Behavior)
	}
	if d.Message != msgNoOperator {
		t.Errorf("message = %q, want %q", d.Message, msgNoOperator)
	}
	// Denied on the grace clock, nowhere near the 15s full window.
	if elapsed > 2*time.Second {
		t.Errorf("absent deny took %v, want ~grace (well under the full permTimeout)", elapsed)
	}
}

// TestBrowserPermissionHandler_PresentWaitsFullTimeout: with a focused board tab
// present in the run's org, the prompt is NOT fast-denied — it waits the full
// window and remains answerable.
func TestBrowserPermissionHandler_PresentWaitsFullTimeout(t *testing.T) {
	hub := websocket.NewHub()
	conn, cleanup := dialHubClientOrg(t, hub, presenceTestOrg)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	s.SetIdleHibernateTimeout(20 * time.Second) // permTimeout = 10s
	s.SetPresencePollInterval(5 * time.Millisecond)

	// Become present BEFORE the prompt is raised so the handler's prompt-time
	// reading is "present" and only the full deadline applies.
	sendPresence(t, conn, hub, presenceTestOrg, "run-1", "board", true, true)

	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{enabled: true, grace: 40 * time.Millisecond})
	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-present", ToolName: "Bash"})
	}()

	// Well past the grace, the prompt must still be pending (not fast-denied).
	time.Sleep(300 * time.Millisecond)
	select {
	case d := <-got:
		t.Fatalf("prompt resolved early (%q/%q) — a present tab must extend it to the full timeout", d.Behavior, d.Message)
	default:
	}

	// It stays answerable: resolving returns the user's decision.
	if err := s.ResolvePermission(presenceTestOrg, "run-1", "req-present", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
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

// TestBrowserPermissionHandler_PresenceDuringGraceExtends: a prompt that arrives
// unattended but gains a focused tab during the grace is NOT fast-denied — the
// wait re-arms to the full timeout.
func TestBrowserPermissionHandler_PresenceDuringGraceExtends(t *testing.T) {
	hub := websocket.NewHub()
	conn, cleanup := dialHubClientOrg(t, hub, presenceTestOrg)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	s.SetIdleHibernateTimeout(20 * time.Second) // permTimeout = 10s
	s.SetPresencePollInterval(5 * time.Millisecond)

	// Absent at prompt time (the dialed client hasn't reported presence yet).
	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{enabled: true, grace: 250 * time.Millisecond})
	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-extend", ToolName: "Bash"})
	}()

	// Focus a board tab partway through the grace.
	time.Sleep(30 * time.Millisecond)
	sendPresence(t, conn, hub, presenceTestOrg, "run-1", "board", true, true)

	// Past where the grace would have fired (250ms) the prompt is still pending,
	// because presence re-armed it to the full window.
	time.Sleep(400 * time.Millisecond)
	select {
	case d := <-got:
		t.Fatalf("prompt fast-denied (%q/%q) despite presence appearing during the grace", d.Behavior, d.Message)
	default:
	}

	// Clean up the parked goroutine.
	if err := s.ResolvePermission(presenceTestOrg, "run-1", "req-extend", agentproc.PermissionDecision{Behavior: "deny"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	<-got
}

// TestBrowserPermissionHandler_PresentThenAbsentDeniesAfterGrace covers the
// third critical transition: a prompt that arrives attended but loses its tab
// mid-wait (present→absent) restarts the grace clock from the moment of
// departure and then denies once it elapses.
func TestBrowserPermissionHandler_PresentThenAbsentDeniesAfterGrace(t *testing.T) {
	hub := websocket.NewHub()
	conn, cleanup := dialHubClientOrg(t, hub, presenceTestOrg)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	s.SetIdleHibernateTimeout(30 * time.Second) // permTimeout = 15s (far above grace)
	s.SetPresencePollInterval(5 * time.Millisecond)

	// Present at prompt time, so only the full window is armed.
	sendPresence(t, conn, hub, presenceTestOrg, "run-1", "board", true, true)

	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{enabled: true, grace: 100 * time.Millisecond})
	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-leave", ToolName: "Bash"})
	}()

	// The operator leaves: present→absent restarts the grace clock from now.
	time.Sleep(30 * time.Millisecond)
	sendPresence(t, conn, hub, presenceTestOrg, "run-1", "board", false, false)

	// The grace fires and denies — nowhere near the 15s full window.
	select {
	case d := <-got:
		if d.Behavior != "deny" || d.Message != msgNoOperator {
			t.Errorf("decision = %q/%q, want deny/%q", d.Behavior, d.Message, msgNoOperator)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt not denied after presence left and the grace elapsed")
	}
}

// TestBrowserPermissionHandler_PresentOtherOrgDenies pins org scoping: a focused
// board tab in a DIFFERENT org does not keep this run's prompt alive — it still
// fast-denies as unattended.
func TestBrowserPermissionHandler_PresentOtherOrgDenies(t *testing.T) {
	const otherOrg = "00000000-0000-0000-0000-0000000000bb"
	hub := websocket.NewHub()
	conn, cleanup := dialHubClientOrg(t, hub, otherOrg)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	s.SetIdleHibernateTimeout(30 * time.Second) // permTimeout = 15s
	s.SetPresencePollInterval(5 * time.Millisecond)

	// A present, focused board tab — but in the wrong org.
	sendPresence(t, conn, hub, otherOrg, "run-1", "board", true, true)
	// It must not register as present for our run's org.
	if hub.PresentFor(presenceTestOrg, "run-1") {
		t.Fatal("a present tab in another org satisfied PresentFor for this org")
	}

	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{enabled: true, grace: 40 * time.Millisecond})
	start := time.Now()
	d := h(agentproc.PermissionRequest{RequestID: "req-otherorg", ToolName: "Bash"})
	if d.Behavior != "deny" || d.Message != msgNoOperator {
		t.Errorf("decision = %q/%q, want deny/%q", d.Behavior, d.Message, msgNoOperator)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a cross-org present tab delayed the absent deny to the full window")
	}
}

// TestBrowserPermissionHandler_ToggleOffIgnoresPresence: with absent-auto-deny
// off the wait is the legacy full-permTimeout() select regardless of presence —
// even with nobody present it does not fast-deny, and when the full window
// elapses it denies with the legacy "permission request timed out" reason.
func TestBrowserPermissionHandler_ToggleOffIgnoresPresence(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetIdleHibernateTimeout(120 * time.Millisecond) // permTimeout = 60ms
	s.SetPresencePollInterval(5 * time.Millisecond)
	h := s.BrowserPermissionHandler(presenceTestOrg, "run-1", AbsentAutoDeny{}) // disabled

	start := time.Now()
	d := h(agentproc.PermissionRequest{RequestID: "req-off", ToolName: "Bash"})
	elapsed := time.Since(start)

	if d.Behavior != "deny" || d.Message != msgPermTimedOut {
		t.Errorf("decision = %q/%q, want deny/%q", d.Behavior, d.Message, msgPermTimedOut)
	}
	// It waited the full ~60ms window, not an instant absent deny.
	if elapsed < 50*time.Millisecond {
		t.Errorf("toggle-off deny took %v, want ~full permTimeout (no fast-deny)", elapsed)
	}
}

// TestClampGrace pins the invariant guard: grace lands in [1s, full) so the
// absent path can never collapse to an instant deny or exceed the full window.
func TestClampGrace(t *testing.T) {
	full := 10 * time.Second
	if g := clampGrace(0, full); g < time.Second || g >= full {
		t.Errorf("clampGrace(0) = %v, want [1s, %v)", g, full)
	}
	if g := clampGrace(15*time.Second, full); g >= full {
		t.Errorf("clampGrace(over-full) = %v, want < %v", g, full)
	}
	if g := clampGrace(5*time.Second, full); g != 5*time.Second {
		t.Errorf("clampGrace(in-range) = %v, want 5s (unchanged)", g)
	}
	// A pathologically short full still yields a positive, strictly-smaller grace.
	if g := clampGrace(time.Second, 4*time.Millisecond); g <= 0 || g >= 4*time.Millisecond {
		t.Errorf("clampGrace(short full) = %v, want (0, 4ms)", g)
	}
}

// readPermissionResolved reads frames until a permission_resolved event arrives
// (skipping the permission_request the handler emits on registration), and
// returns its run_id + request_id. A bounded read deadline turns a missing
// broadcast into a clean failure instead of a hang.
func readPermissionResolved(t *testing.T, conn *ws.Conn) (runID, requestID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read ws frame: %v", err)
		}
		var evt struct {
			Type  string `json:"type"`
			RunID string `json:"run_id"`
			Data  struct {
				RequestID string `json:"request_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &evt); err != nil {
			t.Fatalf("decode frame: %v (raw=%q)", err, data)
		}
		if evt.Type == "permission_resolved" {
			return evt.RunID, evt.Data.RequestID
		}
	}
}

// TestBrowserPermissionHandler_ResolveBroadcastsResolved pins the client
// contract: a user-answered prompt broadcasts permission_resolved(run, request)
// so every other surface showing it drops it without waiting out its TTL.
func TestBrowserPermissionHandler_ResolveBroadcastsResolved(t *testing.T) {
	hub := websocket.NewHub()
	conn, cleanup := dialHubClient(t, hub)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-1", AbsentAutoDeny{})

	got := make(chan agentproc.PermissionDecision, 1)
	go func() {
		got <- h(agentproc.PermissionRequest{RequestID: "req-1", ToolName: "Bash", Input: map[string]any{"command": "ls"}})
	}()

	waitForPending(t, s, "run-1", "req-1")
	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "run-1", "req-1", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
	<-got // handler unblocked

	runID, requestID := readPermissionResolved(t, conn)
	if runID != "run-1" || requestID != "req-1" {
		t.Errorf("permission_resolved = (run %q, req %q), want (run-1, req-1)", runID, requestID)
	}
}

// TestBrowserPermissionHandler_TimeoutBroadcastsResolved pins the same contract
// for the other terminal path: a prompt nobody answered times out and still
// broadcasts permission_resolved so a stale card clears.
func TestBrowserPermissionHandler_TimeoutBroadcastsResolved(t *testing.T) {
	hub := websocket.NewHub()
	conn, cleanup := dialHubClient(t, hub)
	defer cleanup()

	s := NewSpawner(nil, db.Stores{}, nil, hub, "")
	s.SetIdleHibernateTimeout(100 * time.Millisecond) // permTimeout = 50ms

	h := s.BrowserPermissionHandler(runmode.LocalDefaultOrgID, "run-1", AbsentAutoDeny{})
	d := h(agentproc.PermissionRequest{RequestID: "req-timeout", ToolName: "Bash"})
	if d.Behavior != "deny" {
		t.Fatalf("behavior = %q, want deny on timeout", d.Behavior)
	}

	runID, requestID := readPermissionResolved(t, conn)
	if runID != "run-1" || requestID != "req-timeout" {
		t.Errorf("permission_resolved = (run %q, req %q), want (run-1, req-timeout)", runID, requestID)
	}
}
