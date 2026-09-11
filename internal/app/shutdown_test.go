package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func newDrainTestApp() *App {
	return &App{
		plan:    planForRole(runmode.RoleExecutor),
		spawner: delegate.NewSpawner(nil, db.Stores{}, nil, nil, ""),
	}
}

// TestAwaitDispatches_StopsReportingReadyBeforeWaiting pins the ordering the
// shutdown path depends on: by the time the join blocks, this pod already
// answers not-ready and has latched drain. A rolling deploy that learns the
// pod is leaving only after the wait would keep routing at it for the whole
// drain window.
func TestAwaitDispatches_StopsReportingReadyBeforeWaiting(t *testing.T) {
	a := newDrainTestApp()

	readyDuringWait := true
	drainingDuringWait := false
	wait := func(context.Context) bool {
		readyDuringWait = !a.shuttingDown.Load()
		drainingDuringWait = a.spawner.Draining()
		return true
	}

	if !a.awaitDispatches(wait, time.Second) {
		t.Fatal("awaitDispatches reported a timeout on a join that succeeded")
	}
	if readyDuringWait {
		t.Error("the healthz was still reporting ready when the join began")
	}
	if !drainingDuringWait {
		t.Error("draining was not latched before the join began — the wait is not bounded by already-claimed work")
	}
}

// TestAwaitDispatches_DeadlineExpiryLogsAndReturns pins decision (4): the
// expiry path gives up and lets Close proceed. Nothing stronger is possible —
// the writes still running are the un-cancellable ones — so what shutdown owes
// the operator is a bounded wait and a line saying it expired, not a hang.
func TestAwaitDispatches_DeadlineExpiryLogsAndReturns(t *testing.T) {
	a := newDrainTestApp()

	// A dispatch that never finishes: the join can only end on the deadline.
	wait := func(ctx context.Context) bool {
		<-ctx.Done()
		return false
	}

	start := time.Now()
	if a.awaitDispatches(wait, 50*time.Millisecond) {
		t.Fatal("awaitDispatches reported a clean drain while a dispatch was still in flight")
	}
	waited := time.Since(start)
	if waited < 50*time.Millisecond {
		t.Errorf("returned after %s, before the deadline it was given", waited)
	}
	if waited > 10*time.Second {
		t.Errorf("took %s — the drain must be bounded by its deadline, not by the work", waited)
	}
}

// TestDrainDispatches_NoOpWithoutADispatcher: a control pod claims nothing, so
// it has nothing in flight to finish. Latching drain/shutting-down there would
// only mislabel a pod whose real readiness signal is its own HTTP server.
func TestDrainDispatches_NoOpWithoutADispatcher(t *testing.T) {
	a := newDrainTestApp()
	a.plan = planForRole(runmode.RoleControl)

	a.drainDispatches(cancelledContext())

	if a.shuttingDown.Load() || a.spawner.Draining() {
		t.Error("drainDispatches touched the drain state on a role that never dispatches")
	}
}

// TestDrainDispatches_NoOpWhileTheContextIsLive: a listener also returns
// without ever having served — a port already in use — and that is not a
// shutdown. Draining there would stop a dispatcher that is still running, and
// the join would hold the real bind error behind a deadline nobody caused.
func TestDrainDispatches_NoOpWhileTheContextIsLive(t *testing.T) {
	a := newDrainTestApp()

	a.drainDispatches(context.Background())

	if a.shuttingDown.Load() {
		t.Error("a live context latched shutting_down; nothing is shutting down")
	}
	if a.spawner.Draining() {
		t.Error("a live context latched draining; the dispatcher is still meant to claim")
	}
}

// cancelledContext is the shutdown signal the drain gate looks for.
func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestRunExecutorHealthz_DrainsWhileStillAnswering is the ordering end to end,
// over a real socket: on cancellation the drain runs first and the endpoint is
// still reachable while it does — answering 503 with shutting_down set, which
// is what tells a rolling deploy to route away. Only once the drain returns
// does the listener stop.
func TestRunExecutorHealthz_DrainsWhileStillAnswering(t *testing.T) {
	t.Setenv("TF_HEALTHZ_PORT", freePort(t))
	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", executorHealthzPort())

	a := newDrainTestApp()
	ctx, cancel := context.WithCancel(context.Background())

	type probe struct {
		code int
		body executorHealthzResponse
		err  error
	}
	probed := make(chan probe, 1)
	drain := func() {
		a.drainDispatches(ctx)
		// Still inside the shutdown hook: the listener has not been told to
		// stop yet, so this probe is the assertion that a draining pod
		// answers rather than refusing.
		var p probe
		resp, err := http.Get(url)
		if err != nil {
			p.err = err
			probed <- p
			return
		}
		defer resp.Body.Close()
		p.code = resp.StatusCode
		p.err = json.NewDecoder(resp.Body).Decode(&p.body)
		probed <- p
	}

	served := make(chan error, 1)
	go func() { served <- a.runExecutorHealthz(ctx, drain) }()
	waitForHealthz(t, url)

	cancel()

	p := <-probed
	if p.err != nil {
		t.Fatalf("healthz was unreachable during the drain: %v", p.err)
	}
	if p.code != http.StatusServiceUnavailable {
		t.Errorf("code during drain = %d, want 503 — a pod on its way out must not read as ready", p.code)
	}
	if !p.body.ShuttingDown {
		t.Error("shutting_down = false during the drain; the 503 is unattributable")
	}
	if !p.body.Draining {
		t.Error("draining = false during the drain")
	}

	if err := <-served; err != nil {
		t.Fatalf("runExecutorHealthz: %v", err)
	}
	if resp, err := http.Get(url); err == nil {
		resp.Body.Close()
		t.Error("the listener is still answering after the drain returned; shutdown never completed")
	}
}

// freePort reserves and releases a loopback port, so the healthz listener
// under test binds somewhere nothing else on the machine is using.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return port
}

// waitForHealthz blocks until the listener under test accepts a request, so a
// probe can't race the bind.
func waitForHealthz(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("executor healthz never came up")
}

// TestRunExecutorHealthz_DoesNotDrainOnBindFailure is the executor's half of
// the same rule: a healthz listener that cannot bind returns its error
// straight away, never having served, so the shutdown hook must not fire. The
// dispatcher is still running on a live context at that point.
func TestRunExecutorHealthz_DoesNotDrainOnBindFailure(t *testing.T) {
	// Hold the port so the listener under test cannot have it.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer held.Close()
	_, port, err := net.SplitHostPort(held.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	t.Setenv("TF_HEALTHZ_PORT", port)

	a := newDrainTestApp()
	// Atomic: the hook runs on the healthz goroutine, and this test's whole
	// point is that it might.
	var drained atomic.Bool

	served := make(chan error, 1)
	go func() {
		served <- a.runExecutorHealthz(context.Background(), func() { drained.Store(true) })
	}()

	select {
	case err := <-served:
		if err == nil {
			t.Fatal("runExecutorHealthz returned nil on a port it could not bind")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runExecutorHealthz did not return the bind error")
	}
	if drained.Load() {
		t.Error("the shutdown hook ran on a bind failure; nothing was shutting down")
	}
	if a.shuttingDown.Load() || a.spawner.Draining() {
		t.Error("a bind failure latched the drain state")
	}
}
