package curator

import (
	"context"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/placement"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// --- fakes -----------------------------------------------------------------

type fakeInstanceLister struct{ rows []domain.Instance }

func (f *fakeInstanceLister) List(context.Context) ([]domain.Instance, error) { return f.rows, nil }

type fakeOverrideReader struct {
	m map[string]*domain.PlacementOverride
}

func (f *fakeOverrideReader) Get(_ context.Context, _, kind, value string) (*domain.PlacementOverride, error) {
	return f.m[kind+"|"+value], nil
}

type fakeHomeStore struct {
	rows    map[string]*domain.CuratorHome // projectID -> row
	upserts int
	clears  int
}

func newFakeHomeStore() *fakeHomeStore {
	return &fakeHomeStore{rows: map[string]*domain.CuratorHome{}}
}

func (f *fakeHomeStore) Get(_ context.Context, _, projectID string) (*domain.CuratorHome, error) {
	return f.rows[projectID], nil
}

func (f *fakeHomeStore) Upsert(_ context.Context, orgID, projectID, instanceID string, bootEpoch int64) error {
	f.upserts++
	existing := f.rows[projectID]
	homedAt := time.Now()
	if existing != nil {
		homedAt = existing.HomedAt
	}
	f.rows[projectID] = &domain.CuratorHome{
		OrgID: orgID, ProjectID: projectID, HomeInstanceID: instanceID,
		HomeBootEpoch: bootEpoch, HomedAt: homedAt, UpdatedAt: time.Now(),
	}
	return nil
}

func (f *fakeHomeStore) Clear(_ context.Context, _, projectID string) error {
	f.clears++
	delete(f.rows, projectID)
	return nil
}

// execInstance builds an eligible executor registry row heartbeating now.
func execInstance(id string, maxRuns int) domain.Instance {
	m := maxRuns
	return domain.Instance{ID: id, Role: domain.InstanceRoleExecutor, MaxRuns: &m, BootEpoch: 7, LastHeartbeatAt: time.Now()}
}

func newHomer(t *testing.T, self string, rows []domain.Instance, overrides map[string]*domain.PlacementOverride, homes *fakeHomeStore) *Homer {
	t.Helper()
	cfg := placement.Config{Enabled: true, Aging: 20 * time.Second, Liveness: 12 * time.Second}
	resolver := placement.NewResolver(&fakeInstanceLister{rows: rows}, &fakeOverrideReader{m: overrides}, cfg)
	return NewHomer(homes, resolver, self)
}

// --- tests -----------------------------------------------------------------

func TestHomer_MintsOnFirstTurn(t *testing.T) {
	homes := newFakeHomeStore()
	h := newHomer(t, "c1", []domain.Instance{execInstance("e1", 8)}, nil, homes)

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e1" || !forward {
		t.Fatalf("first turn should home to the sole executor and forward: home=%q forward=%v ok=%v", home, forward, ok)
	}
	if homes.upserts != 1 {
		t.Fatalf("first turn should mint one home row, upserts=%d", homes.upserts)
	}
	if got := homes.rows["proj"]; got == nil || got.HomeInstanceID != "e1" || got.HomeBootEpoch != 7 {
		t.Fatalf("home row not persisted correctly: %+v", got)
	}
}

func TestHomer_HomesEvenWhenPlacementDisabled(t *testing.T) {
	// TF_PLACEMENT off must NOT disable homing: control is capless, so the turn
	// still has to reach an executor. Explain evaluates the candidate set
	// regardless of the Enabled flag.
	homes := newFakeHomeStore()
	cfg := placement.Config{Enabled: false}
	resolver := placement.NewResolver(&fakeInstanceLister{rows: []domain.Instance{execInstance("e1", 8)}}, &fakeOverrideReader{}, cfg)
	h := NewHomer(homes, resolver, "c1")

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e1" || !forward {
		t.Fatalf("homing must work with placement disabled: home=%q forward=%v ok=%v", home, forward, ok)
	}
}

func TestHomer_StickyWhileHomeAlive(t *testing.T) {
	homes := newFakeHomeStore()
	// Pre-seed a home to e1; both e1 and e2 are alive.
	_ = homes.Upsert(context.Background(), "org", "proj", "e1", 7)
	homes.upserts = 0 // reset the counter after seeding
	h := newHomer(t, "c1", []domain.Instance{execInstance("e1", 8), execInstance("e2", 8)}, nil, homes)

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e1" || !forward {
		t.Fatalf("live home should be reused: home=%q forward=%v ok=%v", home, forward, ok)
	}
	if homes.upserts != 0 {
		t.Fatalf("sticky reuse must not re-mint, upserts=%d", homes.upserts)
	}
}

func TestHomer_ReHomesOnDeath(t *testing.T) {
	homes := newFakeHomeStore()
	_ = homes.Upsert(context.Background(), "org", "proj", "e1", 7)
	homes.upserts = 0
	// e1 is now heartbeat-stale (dead); e2 is alive.
	dead := execInstance("e1", 8)
	dead.LastHeartbeatAt = time.Now().Add(-time.Hour)
	h := newHomer(t, "c1", []domain.Instance{dead, execInstance("e2", 8)}, nil, homes)

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e2" || !forward {
		t.Fatalf("dead home should re-home to the live executor: home=%q forward=%v ok=%v", home, forward, ok)
	}
	if homes.upserts != 1 {
		t.Fatalf("re-home should persist the new home, upserts=%d", homes.upserts)
	}
	if got := homes.rows["proj"]; got == nil || got.HomeInstanceID != "e2" {
		t.Fatalf("home row not re-pointed: %+v", got)
	}
}

func TestHomer_NoEligibleExecutor_Unavailable(t *testing.T) {
	homes := newFakeHomeStore()
	// Only a control-role instance exists — ineligible as a curator home.
	control := domain.Instance{ID: "c9", Role: domain.InstanceRoleControl, BootEpoch: 1, LastHeartbeatAt: time.Now()}
	h := newHomer(t, "c1", []domain.Instance{control}, nil, homes)

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if ok || home != "" || forward {
		t.Fatalf("no eligible executor must report unavailable (control is capless): home=%q forward=%v ok=%v", home, forward, ok)
	}
	if homes.upserts != 0 {
		t.Fatalf("unavailable must not persist a home, upserts=%d", homes.upserts)
	}
}

func TestHomer_HonorsEligiblePin(t *testing.T) {
	homes := newFakeHomeStore()
	overrides := map[string]*domain.PlacementOverride{
		domain.PlacementKindProject + "|proj": {
			OrgID: "org", KeyKind: domain.PlacementKindProject, KeyValue: "proj", PinnedInstanceID: "e2",
		},
	}
	h := newHomer(t, "c1", []domain.Instance{execInstance("e1", 8), execInstance("e2", 8)}, overrides, homes)

	home, _, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e2" {
		t.Fatalf("an eligible pin should win the home: home=%q ok=%v", home, ok)
	}
}

// TestSendMessage_ForwardsToHomeExecutor pins the control-pod forward path: a
// wired homer that resolves a remote executor makes SendMessage stamp the row's
// home, ring the "curator_new" doorbell, and NOT start a local session — the
// singularity property (the session lives only at the home).
func TestSendMessage_ForwardsToHomeExecutor(t *testing.T) {
	database := newTestDB(t)
	stores := sqlitestore.New(database)
	projectID := seedProject(t, database, "p")

	c := New(stores, nil, "")
	resolver := placement.NewResolver(
		&fakeInstanceLister{rows: []domain.Instance{execInstance("e1", 8)}},
		&fakeOverrideReader{}, placement.Config{Enabled: true, Liveness: 12 * time.Second})
	c.SetHoming(NewHomer(stores.CuratorHomes, resolver, "c1"), "c1")
	var doorbells []string
	c.SetDoorbell(func(kind, _, _ string) { doorbells = append(doorbells, kind) })

	reqID, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// No local session on the control pod — the turn runs on the home executor.
	c.mu.Lock()
	_, hasSession := c.sessions[projectID]
	c.mu.Unlock()
	if hasSession {
		t.Fatal("control pod must NOT start a local session for a forwarded turn")
	}
	// The doorbell nudged the home executor's claim loop.
	if len(doorbells) != 1 || doorbells[0] != "curator_new" {
		t.Fatalf("expected one curator_new doorbell, got %v", doorbells)
	}
	// The durable delivery exists: an undelivered user message on the
	// sender's conversation, and the project is homed to the executor —
	// exactly what the home's claim-loop scan keys on.
	var delivered bool
	if err := database.QueryRow(`SELECT delivered FROM messages WHERE id = ?`, reqID).Scan(&delivered); err != nil {
		t.Fatalf("read back turn: %v", err)
	}
	if delivered {
		t.Fatal("forwarded turn must stay undelivered until the home executor claims it")
	}
	home, err := stores.CuratorHomes.Get(t.Context(), runmode.LocalDefaultOrgID, projectID)
	if err != nil || home == nil {
		t.Fatalf("read curator home: %v (home=%v)", err, home)
	}
	if home.HomeInstanceID != "e1" {
		t.Fatalf("curator home = %q, want e1", home.HomeInstanceID)
	}
}

// TestSendMessage_NoExecutorFailsFast pins the capless-control degraded path: a
// wired homer with no eligible executor makes SendMessage fail with
// ErrNoCuratorExecutor rather than sandboxing the turn in-process on control.
func TestSendMessage_NoExecutorFailsFast(t *testing.T) {
	database := newTestDB(t)
	stores := sqlitestore.New(database)
	projectID := seedProject(t, database, "p")

	c := New(stores, nil, "")
	// No executors in the fleet — only the control pod itself (ineligible).
	resolver := placement.NewResolver(
		&fakeInstanceLister{rows: nil}, &fakeOverrideReader{},
		placement.Config{Enabled: true, Liveness: 12 * time.Second})
	c.SetHoming(NewHomer(stores.CuratorHomes, resolver, "c1"), "c1")

	_, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi")
	if err == nil {
		t.Fatal("expected ErrNoCuratorExecutor, got nil")
	}
	if err != ErrNoCuratorExecutor {
		t.Fatalf("err = %v, want ErrNoCuratorExecutor", err)
	}
	// No turn recorded — nothing can run it, so nothing to show or sweep.
	var n int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.project_id = ?
	`, projectID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("no turn should be recorded when no executor is available, got %d", n)
	}
}

func TestHomer_IgnoresDeadPin(t *testing.T) {
	homes := newFakeHomeStore()
	// Pin to e9, which is not a live registry row → must not strand; fall to the
	// eligible rendezvous winner among {e1}.
	overrides := map[string]*domain.PlacementOverride{
		domain.PlacementKindProject + "|proj": {
			OrgID: "org", KeyKind: domain.PlacementKindProject, KeyValue: "proj", PinnedInstanceID: "e9",
		},
	}
	h := newHomer(t, "c1", []domain.Instance{execInstance("e1", 8)}, overrides, homes)

	home, forward, ok := h.Resolve(context.Background(), "org", "proj")
	if !ok || home != "e1" || !forward {
		t.Fatalf("a dead pin must fall back to the eligible winner: home=%q forward=%v ok=%v", home, forward, ok)
	}
}
