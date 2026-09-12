package app

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// spawnerForHookTest builds the smallest real Spawner the hook wiring takes:
// the hook is a method value on it, so a nil would not exercise the call.
func spawnerForHookTest(t *testing.T) *delegate.Spawner {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return delegate.NewSpawner(conn, sqlitestore.New(conn), nil, nil, "")
}

// TestHookReconcilerToSpawner_ExecutorHasNoReconciler pins the boot shape of a
// multi-mode executor: it builds a spawner and no brain, so there is no
// reconciler to hook, and the wiring must say so rather than dereference one.
func TestHookReconcilerToSpawner_ExecutorHasNoReconciler(t *testing.T) {
	a := &App{plan: planForRole(runmode.RoleExecutor), spawner: spawnerForHookTest(t)}
	if a.plan.brain {
		t.Fatal("an executor's plan must not carry the brain, or this test is not the executor shape")
	}
	a.hookReconcilerToSpawner() // must not panic
}

// TestHookReconcilerToSpawner_BrainWiresTheClosure is the contrast: where the
// brain built a reconciler, the spawner's closure is what it runs.
func TestHookReconcilerToSpawner_BrainWiresTheClosure(t *testing.T) {
	a := &App{
		plan:           planForRole(runmode.RoleControl),
		spawner:        spawnerForHookTest(t),
		reconcilerCore: reconcile.NewReconciler(nil, nil, nil),
	}
	a.hookReconcilerToSpawner()
	if !a.reconcilerCore.HasPullRequestResolvedHook() {
		t.Error("the brain's reconciler was left without the task-closure hook")
	}
}
