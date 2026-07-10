package app

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/lease"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeLeaseStore is a minimal in-memory lease.Store for driving a real
// *lease.Manager without a database, mirroring internal/lease's own test
// fake (kept separate — a test-only type in this package, not exported
// from internal/lease).
type fakeLeaseStore struct {
	holderID string
	term     int64
}

func (s *fakeLeaseStore) EnsureRow(ctx context.Context, name string) error { return nil }

func (s *fakeLeaseStore) TryAcquire(ctx context.Context, name, holderID string, ttl time.Duration) (int64, bool, error) {
	if s.holderID != "" {
		return 0, false, nil
	}
	s.term++
	s.holderID = holderID
	return s.term, true, nil
}

func (s *fakeLeaseStore) Renew(ctx context.Context, name, holderID string, term int64) (bool, error) {
	return s.holderID == holderID && s.term == term, nil
}

func (s *fakeLeaseStore) Read(ctx context.Context, name string) (string, int64, error) {
	return s.holderID, s.term, nil
}

var _ lease.Store = (*fakeLeaseStore)(nil)

// TestApp_IsBrainHolder_AllAlwaysTrue pins that role=all never consults
// the lease elector at all — a nil leaseElector must not panic, and the
// result is always true.
func TestApp_IsBrainHolder_AllAlwaysTrue(t *testing.T) {
	a := &App{plan: subsystemPlan{role: runmode.RoleAll}}
	if !a.isBrainHolder() {
		t.Error("role=all must always report isBrainHolder() = true")
	}
}

// TestApp_IsBrainHolder_ExecutorAlwaysFalse pins that an executor never
// runs the brain, regardless of anything else — it never even builds a
// leaseElector.
func TestApp_IsBrainHolder_ExecutorAlwaysFalse(t *testing.T) {
	a := &App{plan: subsystemPlan{role: runmode.RoleExecutor}}
	if a.isBrainHolder() {
		t.Error("role=executor must always report isBrainHolder() = false")
	}
}

// TestApp_IsBrainHolder_ControlTracksElector pins that a control pod's
// isBrainHolder() is exactly the elector's IsHolder() — false before
// acquisition, true after.
func TestApp_IsBrainHolder_ControlTracksElector(t *testing.T) {
	store := &fakeLeaseStore{}
	mgr, err := lease.NewManager(store, lease.BackgroundBrainName, "pod-a", time.Hour, 20*time.Second, 15*time.Second, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{plan: subsystemPlan{role: runmode.RoleControl}, leaseElector: mgr}

	if a.isBrainHolder() {
		t.Fatal("expected isBrainHolder() = false before any acquisition attempt")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	for !mgr.IsHolder() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !a.isBrainHolder() {
		t.Error("expected isBrainHolder() = true after acquisition")
	}
}

// TestApp_IsBrainHolder_ControlNilElectorIsFalse pins the defensive
// nil-elector case for a control pod (shouldn't happen in production —
// buildLease always wires one — but must not panic).
func TestApp_IsBrainHolder_ControlNilElectorIsFalse(t *testing.T) {
	a := &App{plan: subsystemPlan{role: runmode.RoleControl}}
	if a.isBrainHolder() {
		t.Error("a control pod with no leaseElector wired must report isBrainHolder() = false, not true")
	}
}

// TestApp_TriggerManager_EmptyOrgIsNoop pins that an empty orgID never
// reaches dispatch or the tf_ctl relay — every real caller has an orgID by
// construction (see relay.go), so this is a silent drop, not a panic on a
// nil a.database from publishCtl.
func TestApp_TriggerManager_EmptyOrgIsNoop(t *testing.T) {
	a := &App{plan: subsystemPlan{role: runmode.RoleAll}}
	a.triggerManager("scorer", "", false) // must not panic despite a.database == nil
}

// TestApp_DispatchManagerTrigger_NilManagersAreSafe pins that every
// Manager branch nil-checks before calling Trigger — a role that hasn't
// built brain objects yet (or ever, on an executor reached via a caller
// bug) must not panic.
func TestApp_DispatchManagerTrigger_NilManagersAreSafe(t *testing.T) {
	a := &App{}
	for _, m := range []string{"scorer", "classifier", "profiler", "reconciler", "unknown-manager"} {
		a.dispatchManagerTrigger(m, "org-1", false)
	}
}
