package app

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/lease"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The App-installation grant mirror is refreshed by pull at the head of every
// GitHub poll cycle, so its leader gate is the poller's: in multi mode the poll
// scheduler runs only while this pod holds the background-brain lease, and in
// local mode (role=all) it always runs. This pins that gate at the level where
// the decision is actually made, so the pass cannot acquire a second, weaker one.
//
// The complementary halves live where the other two facts do: internal/poller
// proves the reconcile runs inside the cycle and in both modes, and
// roleplan_test's exclusion guard proves an executor builds no poller at all.
func TestGrantMirrorReconcile_LeaderGate(t *testing.T) {
	t.Run("LocalAlwaysRuns", func(t *testing.T) {
		// role=all is local's shape (multi refuses it at boot). It never elects,
		// never does any lease I/O, and always self-holds — so the reconcile
		// runs on every cycle, which is what local mode needs: no webhook ever
		// arrives there, making the pull the only convergence path that exists.
		a := &App{plan: planForRole(runmode.RoleAll)}
		if !a.plan.brain {
			t.Fatal("role=all must be brain-capable; the poll scheduler is what drives the reconcile")
		}
		if !a.isBrainHolder() {
			t.Error("role=all must always hold the brain, so the mirror always reconciles by pull")
		}
	})

	t.Run("MultiControlOnlyRunsWhileHolding", func(t *testing.T) {
		// A standby control pod builds every brain object but starts no poll
		// loop, so it reconciles nothing — otherwise two pods would race the
		// same org's mirror against the same rate budget.
		store := &fakeLeaseStore{}
		mgr, err := lease.NewManager(store, lease.BackgroundBrainName, "pod-a", time.Hour, 20*time.Second, 15*time.Second, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		a := &App{plan: planForRole(runmode.RoleControl), leaseElector: mgr}
		if !a.plan.brain {
			t.Fatal("control must be brain-capable")
		}
		if a.isBrainHolder() {
			t.Fatal("a control pod that has not acquired the lease must not run the brain")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go mgr.Run(ctx)
		deadline := time.Now().Add(2 * time.Second)
		for !a.isBrainHolder() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !a.isBrainHolder() {
			t.Fatal("control pod never acquired the lease")
		}
	})

	t.Run("ExecutorNeverRuns", func(t *testing.T) {
		// An executor takes no inbound traffic and runs no timer-driven work;
		// it never even constructs a poller, so there is no call site on it.
		a := &App{plan: planForRole(runmode.RoleExecutor)}
		if a.plan.brain {
			t.Fatal("executor must not be brain-capable")
		}
		if a.isBrainHolder() {
			t.Error("executor must never report holding the brain")
		}
	})
}
