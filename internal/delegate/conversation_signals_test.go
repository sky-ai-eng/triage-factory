package delegate

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeConversationSignalStore is an in-memory db.ConversationSignalStore for
// exercising the cross-pod control seam without Postgres. Mirrors the outbox semantics
// the real store's tests already pin: ascending ids, idempotent Ack,
// AckStatus reads "" for an unacked/unknown id.
type fakeConversationSignalStore struct {
	mu      sync.Mutex
	nextID  int64
	signals map[int64]*domain.ConversationSignal
}

func newFakeConversationSignalStore() *fakeConversationSignalStore {
	return &fakeConversationSignalStore{signals: map[int64]*domain.ConversationSignal{}}
}

func (f *fakeConversationSignalStore) Insert(_ context.Context, orgID, conversationID string, kind domain.ConversationSignalKind, payload, target string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	f.signals[id] = &domain.ConversationSignal{ID: id, OrgID: orgID, ConversationID: conversationID, Kind: kind, Payload: payload, Target: target, CreatedAt: time.Now()}
	return id, nil
}

func (f *fakeConversationSignalStore) ListUnackedForTarget(_ context.Context, target string) ([]domain.ConversationSignal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, 0, len(f.signals))
	for id := range f.signals {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []domain.ConversationSignal
	for _, id := range ids {
		s := f.signals[id]
		if s.Target == target && s.AckedAt == nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeConversationSignalStore) Ack(_ context.Context, id int64, result string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.signals[id]
	if !ok || s.AckedAt != nil {
		return nil
	}
	now := time.Now()
	s.AckedAt = &now
	s.AckResult = result
	return nil
}

func (f *fakeConversationSignalStore) AckStatus(_ context.Context, id int64) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.signals[id]
	if !ok || s.AckedAt == nil {
		return false, "", nil
	}
	return true, s.AckResult, nil
}

func (f *fakeConversationSignalStore) PurgeAcked(_ context.Context, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for id, s := range f.signals {
		if s.AckedAt != nil && s.AckedAt.Before(cutoff) {
			delete(f.signals, id)
			n++
		}
	}
	return n, nil
}

// findUnacked polls the fake store until a signal targeting target appears,
// or fails the test after a bounded wait.
func (f *fakeConversationSignalStore) findUnacked(t *testing.T, target string) domain.ConversationSignal {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sigs, _ := f.ListUnackedForTarget(context.Background(), target)
		if len(sigs) > 0 {
			return sigs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no unacked signal appeared for target %q in time", target)
	return domain.ConversationSignal{}
}

// fakeInstanceStore is an in-memory db.InstanceStore for liveness tests.
type fakeInstanceStore struct {
	mu    sync.Mutex
	insts map[string]*domain.Instance
}

func (f *fakeInstanceStore) Register(context.Context, string, string, string, string) (int64, error) {
	return 1, nil
}
func (f *fakeInstanceStore) Heartbeat(context.Context, string, int64, domain.InstanceHeartbeat) (bool, bool, error) {
	return true, false, nil
}
func (f *fakeInstanceStore) Get(_ context.Context, id string) (*domain.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insts[id], nil
}
func (f *fakeInstanceStore) List(context.Context) ([]domain.Instance, error) {
	return nil, nil
}
func (f *fakeInstanceStore) SetDraining(context.Context, string, bool) (bool, error) {
	return false, nil
}

// taskIDForConversation reads the task_id a seedConversation fixture's conversation row belongs to.
func taskIDForConversation(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = ?`, conversationID).Scan(&taskID); err != nil {
		t.Fatalf("resolve task id for run %s: %v", conversationID, err)
	}
	return taskID
}

// entityIDForConversation reads the entity_id of a seedConversation fixture's conversation's task.
func entityIDForConversation(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	var entityID string
	if err := database.QueryRow(
		`SELECT entity_id FROM tasks WHERE id = (SELECT task_id FROM conversations WHERE id = ?)`, conversationID,
	).Scan(&entityID); err != nil {
		t.Fatalf("resolve entity id for run %s: %v", conversationID, err)
	}
	return entityID
}

// seedFakeEvent inserts a bare event row (reusing seedConversation's already-seeded
// event type) so task_events.event_id / pending_firings.triggering_event_id
// FKs are satisfiable.
func seedFakeEvent(t *testing.T, database *sql.DB) string {
	t.Helper()
	id, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("seed fake event: %v", err)
	}
	return id
}

// seedFakeTrigger inserts a bare event_handlers trigger row (pointed at
// conversationID's own already-seeded blueprint — reused rather than minting a
// second one, satisfying the kind='trigger' CHECK's required fields) so
// pending_firings.trigger_id's FK is satisfiable.
func seedFakeTrigger(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	blueprintRunID := blueprintRunIDForConversation(t, database, conversationID)
	var blueprintID string
	if err := database.QueryRow(`SELECT blueprint_id FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&blueprintID); err != nil {
		t.Fatalf("resolve blueprint id: %v", err)
	}
	id := "trig-" + uuid.New().String()
	if _, err := database.Exec(
		`INSERT INTO event_handlers (id, kind, event_type, blueprint_id, breaker_threshold, min_autonomy_suitability, creator_user_id)
		 VALUES (?, 'trigger', ?, ?, 4, 0, ?)`,
		id, domain.EventGitHubPRCICheckFailed, blueprintID, runmode.LocalDefaultUserID,
	); err != nil {
		t.Fatalf("seed fake trigger: %v", err)
	}
	return id
}

// TestCrossPodController_Interrupt_RoutesRemoteAndAwaitsAck: a local getProc
// miss with a live remote owner inserts an interrupt signal and blocks until
// the owner (simulated here by directly acking the fake store) resolves it.
func TestCrossPodController_Interrupt_RoutesRemoteAndAwaitsAck(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-remote", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-remote", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)
	s.SetSignalAckTimeout(2 * time.Second)

	go func() {
		sig := fakeSignals.findUnacked(t, "executor-2")
		if sig.Kind != domain.ConversationSignalInterrupt {
			t.Errorf("signal kind = %q, want interrupt", sig.Kind)
		}
		_ = fakeSignals.Ack(context.Background(), sig.ID, domain.ConversationSignalAckOK)
	}()

	if err := s.Interrupt(context.Background(), "r-remote"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

// TestCrossPodController_Steer_CarriesTextInPayload: the steer signal's
// payload round-trips the text; a "gone" ack surfaces as ErrNoLiveProcess.
func TestCrossPodController_Steer_CarriesTextInPayload(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-remote2", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-remote2", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)
	s.SetSignalAckTimeout(2 * time.Second)

	go func() {
		sig := fakeSignals.findUnacked(t, "executor-2")
		if sig.Payload == "" {
			t.Error("steer signal payload is empty")
		}
		_ = fakeSignals.Ack(context.Background(), sig.ID, domain.ConversationSignalAckGone)
	}()

	err := s.Steer(context.Background(), "r-remote2", "keep going")
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("err = %v, want ErrNoLiveProcess for a gone ack", err)
	}
}

// TestCrossPodController_Steer_StaleAckSurfacesAsNoLiveProcess: a "stale"
// ack (e.g. the owner couldn't apply the payload) must surface as
// ErrNoLiveProcess exactly like "gone" — routeControlSignal only treats an
// explicit "ok" as success, so no ack_result other than ok is ever silently
// reported as a successful steer/interrupt.
func TestCrossPodController_Steer_StaleAckSurfacesAsNoLiveProcess(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-remote-stale", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-remote-stale", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)
	s.SetSignalAckTimeout(2 * time.Second)

	go func() {
		sig := fakeSignals.findUnacked(t, "executor-2")
		_ = fakeSignals.Ack(context.Background(), sig.ID, domain.ConversationSignalAckStale)
	}()

	err := s.Steer(context.Background(), "r-remote-stale", "keep going")
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("err = %v, want ErrNoLiveProcess for a stale ack", err)
	}
}

// TestCrossPodController_Interrupt_TimesOutWhenOwnerNeverAcks: no ack
// arrives before the deadline -> ErrSignalAckTimeout.
func TestCrossPodController_Interrupt_TimesOutWhenOwnerNeverAcks(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-timeout", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-timeout", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(newFakeConversationSignalStore(), nil)
	s.SetSignalAckTimeout(50 * time.Millisecond)

	err := s.Interrupt(context.Background(), "r-timeout")
	if !errors.Is(err, ErrSignalAckTimeout) {
		t.Errorf("err = %v, want ErrSignalAckTimeout", err)
	}
}

// TestCrossPodController_Interrupt_NoLiveOwnerFallsBackToNotLive: an
// executor with a stale heartbeat is not a viable signal target -> the same
// ErrNoLiveProcess a genuinely unowned run gets.
func TestCrossPodController_Interrupt_NoLiveOwnerFallsBackToNotLive(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-stale", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-stale", "executor-dead", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-dead": {ID: "executor-dead", LastHeartbeatAt: time.Now().Add(-time.Hour)},
	}}
	s.SetConversationSignals(newFakeConversationSignalStore(), nil)

	err := s.Interrupt(context.Background(), "r-stale")
	if !errors.Is(err, ErrNoLiveProcess) {
		t.Errorf("err = %v, want ErrNoLiveProcess", err)
	}
}

// TestCrossPodController_LocalHitNeverGoesRemote: a registered local proc
// short-circuits before ever consulting conversationSignals — proven by asserting no
// signal was ever inserted. A bare &agentproc.LiveRun{} has no real
// subprocess wired up, so the local call itself may panic deep inside
// agentproc once it tries to write to the (nil) control pipe; that's
// expected and irrelevant here — recovered so the assertion below still
// runs, since what this test pins is "never reaches the remote branch",
// not "the local call succeeds".
func TestCrossPodController_LocalHitNeverGoesRemote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-local", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.controller = crossPodController{inProcessController: inProcessController{s: s}}
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)
	s.registerProc(runmode.LocalDefaultOrgID, "r-local", &agentproc.LiveRun{})

	func() {
		defer func() { _ = recover() }()
		_ = s.Interrupt(context.Background(), "r-local")
	}()

	sigs, _ := fakeSignals.ListUnackedForTarget(context.Background(), "")
	if len(sigs) != 0 {
		t.Errorf("a local hit must never insert a conversation_signals row, got %d", len(sigs))
	}
}

// TestStop_SignalsRemoteOwnerBestEffort: the stop verb's DB-only write is
// synchronous and unaffected by the cross-pod hastening signal; the signal
// itself lands asynchronously (fire-and-forget, never waited on).
func TestStop_SignalsRemoteOwnerBestEffort(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-cancel", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-cancel", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-cancel", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = 'r-cancel'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open (the DB-only park must be synchronous, unaffected by the async hastening signal)", status)
	}
	sig := fakeSignals.findUnacked(t, "executor-2")
	if sig.Kind != domain.ConversationSignalCancel {
		t.Errorf("hastening signal kind = %q, want cancel", sig.Kind)
	}
}

// TestStop_SignalsRemoteOwnerBestEffort_NilInstancesDoesNotPanic:
// signalCancelBestEffort must not dereference a nil s.instances — a
// deployment can wire conversationSignals without an instance store (e.g. a
// misconfigured role), and the fire-and-forget hastening path must simply
// no-op rather than panic in its own goroutine.
func TestStop_SignalsRemoteOwnerBestEffort_NilInstancesDoesNotPanic(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-cancel-noinst", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-cancel-noinst", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = nil
	s.SetConversationSignals(fakeSignals, nil)

	if err := s.Stop(runmode.LocalDefaultOrgID, "r-cancel-noinst", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = 'r-cancel-noinst'`).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "open" {
		t.Errorf("status = %q, want open", status)
	}
	// Give the fire-and-forget goroutine a beat to run (and not panic); it
	// must never insert a signal without an instance store to confirm
	// liveness against.
	time.Sleep(50 * time.Millisecond)
	sigs, _ := fakeSignals.ListUnackedForTarget(context.Background(), "executor-2")
	if len(sigs) != 0 {
		t.Errorf("expected no hastening signal without an instance store, got %d", len(sigs))
	}
}

// TestResolvePermission_RoutesRemoteWhenNoLocalProcess: no local permPending
// entry AND no local proc -> resolves through the outbox.
func TestResolvePermission_RoutesRemoteWhenNoLocalProcess(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-perm", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-perm", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)

	go func() {
		sig := fakeSignals.findUnacked(t, "executor-2")
		if sig.Kind != domain.ConversationSignalPermission {
			t.Errorf("signal kind = %q, want permission", sig.Kind)
		}
		_ = fakeSignals.Ack(context.Background(), sig.ID, domain.ConversationSignalAckOK)
	}()

	if err := s.ResolvePermission(runmode.LocalDefaultOrgID, "r-perm", "req-1", "", agentproc.PermissionDecision{Behavior: "allow"}); err != nil {
		t.Fatalf("ResolvePermission: %v", err)
	}
}

// TestResolvePermission_LocalProcOwnedButRequestStaleNeverGoesRemote: this
// process DOES own the run's live process (getProc hit), but the specific
// request_id isn't pending — genuinely stale, never a routing question.
func TestResolvePermission_LocalProcOwnedButRequestStaleNeverGoesRemote(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-perm2", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)
	s.registerProc(runmode.LocalDefaultOrgID, "r-perm2", &agentproc.LiveRun{})

	err := s.ResolvePermission(runmode.LocalDefaultOrgID, "r-perm2", "req-ghost", "", agentproc.PermissionDecision{Behavior: "allow"})
	if !errors.Is(err, ErrNoPendingPermission) {
		t.Errorf("err = %v, want ErrNoPendingPermission", err)
	}
	sigs, _ := fakeSignals.ListUnackedForTarget(context.Background(), "")
	if len(sigs) != 0 {
		t.Errorf("a locally-owned run must never route to remote, got %d signals", len(sigs))
	}
}

// TestStageOrDeliverAdditiveEvent_RemoteLiveSignals: no local process, but a
// live remote owner exists -> InjectDeliveredRemote, fire-and-forget (never
// waited on).
func TestStageOrDeliverAdditiveEvent_RemoteLiveSignals(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-inj", "sess", "/tmp/wt")
	dbtest.SeedActiveClaim(t, database, "r-inj", "executor-2", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.instances = &fakeInstanceStore{insts: map[string]*domain.Instance{
		"executor-2": {ID: "executor-2", LastHeartbeatAt: time.Now()},
	}}
	s.SetConversationSignals(fakeSignals, nil)

	outcome := s.StageOrDeliverAdditiveEvent(context.Background(), runmode.LocalDefaultOrgID, "r-inj", "producer", "body", AdditiveFiringRef{
		EntityID: "e1", TaskID: "t1", TriggerID: "tr1", TriggeringEventID: "ev1",
	})
	if outcome != InjectDeliveredRemote {
		t.Fatalf("outcome = %v, want InjectDeliveredRemote", outcome)
	}
	sig := fakeSignals.findUnacked(t, "executor-2")
	if sig.Kind != domain.ConversationSignalInject {
		t.Errorf("signal kind = %q, want inject", sig.Kind)
	}
}

// TestStageOrDeliverAdditiveEvent_NoLiveOwnerFallsBackToStaged: no local
// proc and no live remote owner -> falls back to the durable staged queue,
// exactly like the pre-TFAC-585 behavior when the run is resumable.
func TestStageOrDeliverAdditiveEvent_NoLiveOwnerFallsBackToStaged(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-inj2", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-inj2'`); err != nil {
		t.Fatalf("park run: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.SetConversationSignals(newFakeConversationSignalStore(), nil)

	outcome := s.StageOrDeliverAdditiveEvent(context.Background(), runmode.LocalDefaultOrgID, "r-inj2", "producer", "body", AdditiveFiringRef{})
	if outcome != InjectStagedResumable {
		t.Fatalf("outcome = %v, want InjectStagedResumable", outcome)
	}
}

// TestStageOrDeliverAdditiveEvent_TerminalRunNotDelivered: staged onto a
// run that's already fully terminal (not parked/resumable) -> the staged
// row will never flush, so the caller must fall through to its own
// deferral (InjectNotDelivered), never silently swallowing the event.
//
// TFAC-621 part 4: this is also the direct regression test for the orphaned
// staged-injection row — StageOrDeliverAdditiveEvent stages the durable row
// BEFORE re-checking resumability, so a non-resumable verdict here must
// delete what it just appended rather than leaving a permanent leaked row
// (worse: a double-delivery hazard if the run is ever boot-recovered and
// resumed later, flushing the row into it while the caller's own
// pending_firing deferral also spawns a fresh run).
func TestStageOrDeliverAdditiveEvent_TerminalRunNotDelivered(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-inj3", "sess", "/tmp/wt")
	if _, err := database.Exec(`UPDATE conversations SET status = 'completed', outcome = 'finish' WHERE id = 'r-inj3'`); err != nil {
		t.Fatalf("terminate run: %v", err)
	}
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "m")
	s.SetConversationSignals(newFakeConversationSignalStore(), nil)

	outcome := s.StageOrDeliverAdditiveEvent(context.Background(), runmode.LocalDefaultOrgID, "r-inj3", "producer", "body", AdditiveFiringRef{})
	if outcome != InjectNotDelivered {
		t.Fatalf("outcome = %v, want InjectNotDelivered", outcome)
	}

	// Withdrawn encoding: the row stays undelivered ("never happened") but
	// leaves the active window, so nothing flushable — or visible — remains.
	var flushable int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE conversation_id = ? AND delivered = 0 AND window_state = 'active'
		  AND subtype = 'injection:system-note'
	`, "r-inj3").Scan(&flushable); err != nil {
		t.Fatalf("count flushable staged injection messages: %v", err)
	}
	if flushable != 0 {
		t.Errorf("messages holds %d flushable staged-injection row(s) for a run InjectNotDelivered decided against, want 0", flushable)
	}
	var withdrawn int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE conversation_id = ? AND delivered = 0 AND window_state = 'inactive'
		  AND subtype = 'injection:system-note'
	`, "r-inj3").Scan(&withdrawn); err != nil {
		t.Fatalf("count withdrawn staged injection messages: %v", err)
	}
	if withdrawn != 1 {
		t.Errorf("withdrawn staged-injection rows = %d, want 1 (retired in place, not deleted)", withdrawn)
	}
	msgs, err := stores.Conversations.Messages(context.Background(), runmode.LocalDefaultOrgID, "r-inj3")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	for _, m := range msgs {
		if m.Subtype == "injection:system-note" {
			t.Errorf("withdrawn staged injection renders in the display read: %+v", m)
		}
	}
}

// TestApplySignal_Inject_LiveDeliversAndRecords: the owner's apply loop
// steers a live inject signal and records task_events 'injected'.
//
// TFAC-621 part 2 (owner-side site): production always reaches this path
// with the control pod's own upsertTaskForEvent having ALREADY written a
// "bumped" row for (taskID, evID) — the additive event that triggers the
// cross-pod inject signal is exactly the event that bumped the task. Seed
// that row here (mirroring RecordEventSystem's real INSERT) so the
// assertion below exercises deliverInjectSignal's MarkEventInjectedSystem
// upgrading an existing row in place, not (pre-fix) a masked no-op INSERT
// racing an absent row that happened to succeed anyway.
func TestApplySignal_Inject_LiveDeliversAndRecords(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-owner-inj", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)
	s.registerProc(runmode.LocalDefaultOrgID, "r-owner-inj", &agentproc.LiveRun{})

	taskID := taskIDForConversation(t, database, "r-owner-inj")
	// A fake triggering event row: task_events FKs to events(id).
	evID := seedFakeEvent(t, database)
	if err := s.tasks.RecordEventSystem(context.Background(), runmode.LocalDefaultOrgID, taskID, evID, "bumped"); err != nil {
		t.Fatalf("seed bumped row: %v", err)
	}

	payload := `{"producer":"p","body":"hello","task_id":"` + taskID + `","triggering_event_id":"` + evID + `"}`
	id, err := fakeSignals.Insert(context.Background(), runmode.LocalDefaultOrgID, "r-owner-inj", domain.ConversationSignalInject, payload, "me")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	sig, _ := fakeSignals.ListUnackedForTarget(context.Background(), "me")
	_ = id
	s.applySignal(context.Background(), sig[0])

	acked, result, err := fakeSignals.AckStatus(context.Background(), sig[0].ID)
	if err != nil || !acked {
		t.Fatalf("ack status: acked=%v err=%v", acked, err)
	}
	if result != domain.ConversationSignalAckDelivered {
		t.Errorf("ack result = %q, want delivered", result)
	}
	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, evID).Scan(&kind); err != nil {
		t.Fatalf("read task_events: %v", err)
	}
	if kind != "injected" {
		t.Errorf("task_events.kind = %q, want injected", kind)
	}
}

// TestApplySignal_Inject_GoneCompensatesWithPendingFiring: the run isn't
// live here at apply time -> acks 'gone' AND enqueues the pending_firing
// itself, so the additive event's intent survives.
func TestApplySignal_Inject_GoneCompensatesWithPendingFiring(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-owner-gone", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)
	// Deliberately no registerProc — the run isn't live here.

	entityID := entityIDForConversation(t, database, "r-owner-gone")
	taskID := taskIDForConversation(t, database, "r-owner-gone")
	triggerID := seedFakeTrigger(t, database, "r-owner-gone")
	evID := seedFakeEvent(t, database)

	payload := `{"producer":"p","body":"hello","entity_id":"` + entityID + `","task_id":"` + taskID + `","trigger_id":"` + triggerID + `","triggering_event_id":"` + evID + `"}`
	_, err := fakeSignals.Insert(context.Background(), runmode.LocalDefaultOrgID, "r-owner-gone", domain.ConversationSignalInject, payload, "me")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	sigs, _ := fakeSignals.ListUnackedForTarget(context.Background(), "me")
	s.applySignal(context.Background(), sigs[0])

	acked, result, err := fakeSignals.AckStatus(context.Background(), sigs[0].ID)
	if err != nil || !acked || result != domain.ConversationSignalAckGone {
		t.Fatalf("ack = (%v,%q,%v), want (true, gone, nil)", acked, result, err)
	}
	rows, err := s.pendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 compensating pending_firing, got %d", len(rows))
	}
}

// TestConversationSignalPurgeReaper_RemovesOldAckedRows: the reaper deletes acked
// rows past the age cutoff and leaves fresher ones alone.
func TestConversationSignalPurgeReaper_RemovesOldAckedRows(t *testing.T) {
	fakeSignals := newFakeConversationSignalStore()
	id1, _ := fakeSignals.Insert(context.Background(), "org", "run", domain.ConversationSignalCancel, "", "t")
	id2, _ := fakeSignals.Insert(context.Background(), "org", "run", domain.ConversationSignalCancel, "", "t")
	_ = fakeSignals.Ack(context.Background(), id1, domain.ConversationSignalAckOK)
	_ = fakeSignals.Ack(context.Background(), id2, domain.ConversationSignalAckOK)
	// Backdate id1 so it's past the purge cutoff; id2 stays fresh.
	fakeSignals.mu.Lock()
	old := time.Now().Add(-48 * time.Hour)
	fakeSignals.signals[id1].AckedAt = &old
	fakeSignals.mu.Unlock()

	n, err := fakeSignals.PurgeAcked(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
	if ok, _, _ := fakeSignals.AckStatus(context.Background(), id1); ok {
		t.Error("id1 should have been purged")
	}
	if acked, _, _ := fakeSignals.AckStatus(context.Background(), id2); !acked {
		t.Error("id2 (fresh) should NOT have been purged")
	}
}

// TestParseSignalAckTimeout pins the TF_SIGNAL_ACK_TIMEOUT env parsing
// convention shared with ParseMaxConcurrentClaims et al.
func TestParseSignalAckTimeout(t *testing.T) {
	if d, err := ParseSignalAckTimeout(""); err != nil || d != DefaultSignalAckTimeout {
		t.Errorf("empty: got (%v,%v), want (%v,nil)", d, err, DefaultSignalAckTimeout)
	}
	if d, err := ParseSignalAckTimeout("2s"); err != nil || d != 2*time.Second {
		t.Errorf("2s: got (%v,%v), want (2s,nil)", d, err)
	}
	if d, err := ParseSignalAckTimeout("not-a-duration"); err == nil || d != DefaultSignalAckTimeout {
		t.Errorf("invalid: got (%v,%v), want (%v,err)", d, err, DefaultSignalAckTimeout)
	}
	if d, err := ParseSignalAckTimeout("-1s"); err == nil || d != DefaultSignalAckTimeout {
		t.Errorf("negative: got (%v,%v), want (%v,err)", d, err, DefaultSignalAckTimeout)
	}
}

// TestConversationSignalApplyLoop_AppliesQueuedSignalsAndWakesOnAck: the apply loop
// scans+applies a queued signal for this executor and acks it — proven
// end-to-end via a cancel signal (the simplest to assert, since it only
// touches s.cancels, no LiveRun internals).
func TestConversationSignalApplyLoop_AppliesQueuedSignalsAndWakesOnAck(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-apply", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.SetExecutorID("me", 1)
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)

	// Register a cancel handle so the apply loop's local Cancel() finds it.
	cancelled := make(chan struct{})
	s.mu.Lock()
	s.cancels["r-apply"] = func() { close(cancelled) }
	s.mu.Unlock()

	id, err := fakeSignals.Insert(context.Background(), runmode.LocalDefaultOrgID, "r-apply", domain.ConversationSignalCancel, "", "me")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ConversationSignalApplyLoop(ctx, 50*time.Millisecond)

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("apply loop never applied the cancel signal")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, result, _ := fakeSignals.AckStatus(context.Background(), id); acked {
			if result != domain.ConversationSignalAckOK {
				t.Errorf("ack result = %q, want ok", result)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("signal was never acked")
}
