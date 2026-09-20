package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/tracker"
)

// The close obligation is the half of the close story the queue could not
// reach on its own: an entity whose stored snapshot is terminal but whose row
// never flipped, with no queue row left to retry — the transition diffed but
// never published, a row parked past its budget, a row orphaned by a replaced
// pod. These tests drive the whole loop end to end: a poll cycle against a
// fake GitHub (or Jira) that observes the terminal state again, the
// obligation it enqueues, and the router closing from it under the same
// guarded transaction a real transition takes. They seed the divergence
// directly, because that is the only honest way to model "the close was
// lost."

// prNode renders one pull request the fake GitHub's GraphQL refresh answers
// with. The node id is what Phase 2 keys the refresh on, and it must match
// the stored snapshot's.
func prNode(state string, merged bool) string {
	return `{"id":"PR_owed","number":9,"title":"Owed PR","author":{"login":"bob"},
	 "state":"` + state + `","merged":` + boolJSON(merged) + `,"url":"https://github.com/octo/repo/pull/9",
	 "repository":{"nameWithOwner":"octo/repo"},
	 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-20T00:00:00Z"}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// fakeGitHub serves the refresh for one pull request whose upstream state the
// test flips between cycles. Only the GraphQL refresh is modelled: the
// entity's stored snapshot already carries its node id, so no REST resolve
// is needed, and the tracker is run with no repos to discover.
type fakeGitHub struct {
	srv   *httptest.Server
	state atomic.Value // "OPEN" | "MERGED"
}

func newFakeGitHub(t *testing.T, state string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.state.Store(state)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		state := f.state.Load().(string)
		_, _ = w.Write([]byte(`{"data":{"nodes":[` + prNode(state, state == "MERGED") + `]}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) set(state string) { f.state.Store(state) }

// noopPublisher is the bus the tracker fans out to; these tests read the
// durable queue, which is what the router consumes.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, domain.Event)            {}
func (noopPublisher) PublishPreEnqueued(context.Context, domain.Event) {}

// pollGitHub runs one GitHub poll cycle against the fake.
func pollGitHub(t *testing.T, database *sql.DB, gh *fakeGitHub) {
	t.Helper()
	st := sqlitestore.New(database)
	tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.EventQueue, runmode.LocalDefaultOrgID)
	if _, _, err := tr.RefreshGitHub(context.Background(), ghclient.NewClient(gh.srv.URL, "tok"), "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
}

// prSnapshotJSON is the stored snapshot for the fake's pull request in the
// given state, carrying the node id Phase 2 refreshes by.
func prSnapshotJSON(t *testing.T, state string) string {
	t.Helper()
	b, err := json.Marshal(domain.PRSnapshot{
		NodeID: "PR_owed", Number: 9, Title: "Owed PR", Author: "bob",
		Repo: "octo/repo", URL: "https://github.com/octo/repo/pull/9",
		State: state, Merged: state == "MERGED",
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(b)
}

// seedStrandedPR is the divergence the obligation repairs: the fake's pull
// request as an ACTIVE entity whose stored snapshot already says merged, with
// a live CI task on it and a running blueprint behind that task. Returns the
// entity, the task and the blueprint run.
func seedStrandedPR(t *testing.T, r *Router, database *sql.DB) (entityID, taskID, blueprintRunID string) {
	t.Helper()
	entityID, taskID = seedCIFailedTaskOnEntity(t, r, database, "octo/repo#9")
	if _, err := sqlitestore.New(database).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entityID, prSnapshotJSON(t, "MERGED")); err != nil {
		t.Fatalf("seed terminal snapshot: %v", err)
	}
	blueprintRunID, _ = seedRunOnTask(t, database, taskID, "running", "")
	return entityID, taskID, blueprintRunID
}

// queueRowsOfType lists the entity's queue rows of one event type.
func queueRowsOfType(t *testing.T, database *sql.DB, entityID, eventType string) []domain.QueuedEvent {
	t.Helper()
	rows, err := sqlitestore.New(database).EventQueue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	var out []domain.QueuedEvent
	for _, row := range rows {
		if row.EventType == eventType {
			out = append(out, row)
		}
	}
	return out
}

func entityPollSeq(t *testing.T, database *sql.DB, entityID string) int64 {
	t.Helper()
	var seq int64
	if err := database.QueryRow(`SELECT poll_seq FROM entities WHERE id = ?`, entityID).Scan(&seq); err != nil {
		t.Fatalf("read poll_seq: %v", err)
	}
	return seq
}

// TestCloseOwed_PreExistingInconsistency_RepairedWithinOneCycle is the
// headline: an entity active with a terminal snapshot at test start. One poll
// cycle enqueues exactly one obligation, judged at the version the cycle
// advanced to; routing it closes the tasks — audit reason reconciled, audit
// row naming the obligation, cancel intent stamped, stop and teardown
// reached — and the entity; a second cycle enqueues nothing, because the
// entity is closed and out of the refresh set.
func TestCloseOwed_PreExistingInconsistency_RepairedWithinOneCycle(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{stubDelegator: stubDelegator{db: database}}
	r.spawner = sp
	gh := newFakeGitHub(t, "MERGED")
	entityID, taskID, brID := seedStrandedPR(t, r, database)
	seqBefore := entityPollSeq(t, database, entityID)

	pollGitHub(t, database, gh)

	owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed)
	if len(owed) != 1 {
		t.Fatalf("obligation rows after one cycle = %d, want exactly 1", len(owed))
	}
	if owed[0].EntityPollSeq == nil || *owed[0].EntityPollSeq != seqBefore+1 {
		t.Errorf("obligation judged at %v, want %d — the version the cycle's CAS advanced to", owed[0].EntityPollSeq, seqBefore+1)
	}
	if got := entityState(t, database, entityID); got != "active" {
		t.Fatalf("entity state after the cycle = %q, want active — the poll records the obligation, the router closes", got)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed — a merged PR must not stay in the refresh set forever", got)
	}
	status, reason := taskCloseReason(t, database, taskID)
	if status != "done" || reason != closeReasonReconciled {
		t.Errorf("task = (%q, %q), want (done, %q) — the audit trail should say the close was reconciled, not event-driven", status, reason, closeReasonReconciled)
	}
	if n := closeAuditCount(t, database, taskID); n != 1 {
		t.Errorf("close-audit rows = %d, want 1 — the obligation is the closing event", n)
	}
	if !cancelRequested(t, database, brID) {
		t.Error("cancel_requested = false; the reconciled close must carry the same stop intent as an event-driven one")
	}
	if got := sp.stoppedIDs(); len(got) != 1 {
		t.Errorf("conversations stopped = %v, want the one running on the task", got)
	}
	if got := sp.tornDownCopy(); len(got) != 1 || got[0] != taskID {
		t.Errorf("teardown calls = %v, want exactly [%s]", got, taskID)
	}
	if rows := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); rows[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("obligation row status = %q, want done", rows[0].Status)
	}

	// A second cycle: the entity is closed, out of the refresh set, and owes
	// nothing.
	pollGitHub(t, database, gh)
	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 1 {
		t.Errorf("obligation rows after repair = %d, want still 1 — a repaired entity owes nothing", len(owed))
	}
}

// TestCloseOwed_UnchangedSnapshot_StillOwed pins that the obligation does not
// depend on the snapshot moving: a refresh returning a byte-identical
// terminal snapshot still records the obligation, because the previous
// snapshot was already terminal and the entity is still active.
func TestCloseOwed_UnchangedSnapshot_StillOwed(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	gh := newFakeGitHub(t, "MERGED")
	entityID, _, _ := seedStrandedPR(t, r, database)

	// First cycle: whatever the refresh normalises the snapshot to is now
	// stored, and the obligation lands. Route it away and reopen the
	// divergence by hand — the snapshot bytes the next cycle diffs against
	// are then exactly what the refresh will return.
	pollGitHub(t, database, gh)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	var stored string
	if err := database.QueryRow(`SELECT snapshot_json FROM entities WHERE id = ?`, entityID).Scan(&stored); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if _, err := database.Exec(`UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ?`, entityID); err != nil {
		t.Fatalf("re-strand the entity: %v", err)
	}

	pollGitHub(t, database, gh)

	var after string
	if err := database.QueryRow(`SELECT snapshot_json FROM entities WHERE id = ?`, entityID).Scan(&after); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if after != stored {
		t.Fatalf("test wiring: the refresh changed the snapshot bytes\nbefore %s\nafter  %s", stored, after)
	}
	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 2 {
		t.Errorf("obligation rows = %d, want 2 — an unchanged terminal snapshot on an active entity is still owed a close", len(owed))
	}
}

// TestCloseOwed_InFlightTransition_EmitsOnlyTheTransition is the negative
// space: the cycle that MAKES a snapshot terminal emits the real transition
// and no obligation, and while that transition is pending the next cycle
// emits nothing at all — the router will close from the transition.
func TestCloseOwed_InFlightTransition_EmitsOnlyTheTransition(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	gh := newFakeGitHub(t, "OPEN")
	entityID, _ := seedCIFailedTaskOnEntity(t, r, database, "octo/repo#9")
	if _, err := sqlitestore.New(database).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entityID, prSnapshotJSON(t, "OPEN")); err != nil {
		t.Fatalf("seed open snapshot: %v", err)
	}

	pollGitHub(t, database, gh) // open → open: nothing
	gh.set("MERGED")
	pollGitHub(t, database, gh) // open → merged: the real transition

	if merged := queueRowsOfType(t, database, entityID, domain.EventGitHubPRMerged); len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1", len(merged))
	}
	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 0 {
		t.Errorf("obligation rows = %d, want 0 — the cycle that emits the transition owes nothing", len(owed))
	}

	// The transition is still pending: the entity is active with a terminal
	// snapshot, which is the obligation's shape, but a close is in flight.
	pollGitHub(t, database, gh)
	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 0 {
		t.Errorf("obligation rows while the transition is pending = %d, want 0", len(owed))
	}

	// Once the transition routes, the entity is closed and nothing is owed.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
	pollGitHub(t, database, gh)
	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 0 {
		t.Errorf("obligation rows after the transition routed = %d, want 0", len(owed))
	}
}

// TestCloseOwed_ReopenRace_ObligationIsStale is the race the version guard
// exists for. The obligation is enqueued at version N+1; before it routes,
// the pull request reopens and the next cycle writes the open snapshot
// (version N+2). Routing the obligation must decline: the entity stays
// active, the task stays open, no cancel intent is written, and the row is
// consumed rather than replayed.
func TestCloseOwed_ReopenRace_ObligationIsStale(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	gh := newFakeGitHub(t, "MERGED")
	entityID, taskID, brID := seedStrandedPR(t, r, database)

	pollGitHub(t, database, gh)
	owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed)
	if len(owed) != 1 {
		t.Fatalf("obligation rows = %d, want 1", len(owed))
	}

	gh.set("OPEN")
	pollGitHub(t, database, gh)
	if seq := entityPollSeq(t, database, entityID); seq != *owed[0].EntityPollSeq+1 {
		t.Fatalf("test wiring: poll_seq = %d after the reopen cycle, want %d", seq, *owed[0].EntityPollSeq+1)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := entityState(t, database, entityID); got != "active" {
		t.Errorf("entity state = %q, want active — the obligation was judged at a version that no longer exists", got)
	}
	if status, _ := taskCloseReason(t, database, taskID); status != "queued" {
		t.Errorf("task status = %q, want queued — the stale obligation must touch nothing", status)
	}
	if cancelRequested(t, database, brID) {
		t.Error("cancel_requested = true; a stale close cancelled a run on a reopened pull request")
	}
	if rows := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); rows[0].Status != domain.QueuedEventStatusDone {
		t.Errorf("obligation row status = %q, want done — a stale terminal is consumed, not replayed", rows[0].Status)
	}
}

// TestCloseOwed_ReopenRace_RealTransitionIsStale is the same race on a real
// merged transition: enqueued at version N+1, the pull request reopened
// before it routed. The merged event is consumed with the stale_terminal
// disposition and the entity stays active.
func TestCloseOwed_ReopenRace_RealTransitionIsStale(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	pub := &fakeDispositionPublisher{}
	r.SetEventPublisher(pub)
	gh := newFakeGitHub(t, "OPEN")
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "octo/repo#9")
	if _, err := sqlitestore.New(database).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entityID, prSnapshotJSON(t, "OPEN")); err != nil {
		t.Fatalf("seed open snapshot: %v", err)
	}
	brID, _ := seedRunOnTask(t, database, taskID, "running", "")

	pollGitHub(t, database, gh)
	gh.set("MERGED")
	pollGitHub(t, database, gh)
	merged := queueRowsOfType(t, database, entityID, domain.EventGitHubPRMerged)
	if len(merged) != 1 || merged[0].EntityPollSeq == nil {
		t.Fatalf("merged rows = %+v, want one carrying its version", merged)
	}
	gh.set("OPEN")
	pollGitHub(t, database, gh)

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := entityState(t, database, entityID); got != "active" {
		t.Errorf("entity state = %q, want active", got)
	}
	if status, _ := taskCloseReason(t, database, taskID); status != "queued" {
		t.Errorf("task status = %q, want queued", status)
	}
	if cancelRequested(t, database, brID) {
		t.Error("cancel_requested = true; the stale merge cancelled a run on a reopened pull request")
	}
	var disp string
	for _, evt := range pub.eventsCopy() {
		meta := decodeDisposition(t, evt.MetadataJSON)
		if meta.EventType == domain.EventGitHubPRMerged {
			disp = meta.Disposition
		}
	}
	if disp != events.DispositionStaleTerminal {
		t.Errorf("merged disposition = %q, want %q", disp, events.DispositionStaleTerminal)
	}
}

// closeTerminalOutageStore fails the terminating close — the ONE transaction
// a terminating event closes an entity and its tasks through.
type closeTerminalOutageStore struct {
	dbpkg.EntityStore
	o *outage
}

func (s closeTerminalOutageStore) CloseTerminalSystem(ctx context.Context, orgID, entityID string, expected *int64, closeTypes []string, closeReason, closeEventType, closingEventID string) (dbpkg.TerminalCloseResult, error) {
	if s.o.down() {
		return dbpkg.TerminalCloseResult{}, errOutage
	}
	return s.EntityStore.CloseTerminalSystem(ctx, orgID, entityID, expected, closeTypes, closeReason, closeEventType, closingEventID)
}

// TestCloseOwed_FailedClose_ReplaysWholeAndParkedIsReissued pins the failure
// posture. A close that fails leaves nothing half-done — both tasks open,
// the entity active, no cancel intent — and the row replays; a row that
// parks after its budget is re-owed by the next cycle, and the parked one
// stays visible.
func TestCloseOwed_FailedClose_ReplaysWholeAndParkedIsReissued(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	gh := newFakeGitHub(t, "MERGED")
	entityID, taskID, brID := seedStrandedPR(t, r, database)
	second, _, err := testTaskStore(database).FindOrCreateAtSystem(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultTeamID, entityID, domain.EventGitHubPRReviewChangesRequested, "", queueRowsOfType(t, database, entityID, domain.EventGitHubPRCICheckFailed)[0].EventID, 0.5, time.Now())
	if err != nil {
		t.Fatalf("seed second task: %v", err)
	}

	pollGitHub(t, database, gh)
	o := &outage{remaining: 1}
	r.entities = closeTerminalOutageStore{EntityStore: sqlitestore.New(database).Entities, o: o}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed)
	if owed[0].Status != domain.QueuedEventStatusPending || owed[0].Attempts != 1 {
		t.Errorf("row after a failed close = (%s, attempts %d), want (pending, 1) — the close is owed, not done", owed[0].Status, owed[0].Attempts)
	}
	if got := entityState(t, database, entityID); got != "active" {
		t.Errorf("entity state = %q, want active", got)
	}
	for _, id := range []string{taskID, second.ID} {
		if status, _ := taskCloseReason(t, database, id); status != "queued" {
			t.Errorf("task %s status = %q, want queued — a failed close leaves nothing half-closed", id, status)
		}
	}
	if cancelRequested(t, database, brID) {
		t.Error("cancel_requested = true after a failed close; the intent must roll back with it")
	}

	// The replay finishes the whole job.
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
	for _, id := range []string{taskID, second.ID} {
		if status, reason := taskCloseReason(t, database, id); status != "done" || reason != closeReasonReconciled {
			t.Errorf("task %s = (%q, %q), want (done, %q)", id, status, reason, closeReasonReconciled)
		}
	}

	// The park: strand the entity again, let the obligation burn its budget,
	// and watch the next cycle owe a fresh one beside the parked row.
	if _, err := database.Exec(`UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ?`, entityID); err != nil {
		t.Fatalf("re-strand the entity: %v", err)
	}
	pollGitHub(t, database, gh)
	r.entities = closeTerminalOutageStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: maxEventAttempts}}
	for i := 0; i < maxEventAttempts; i++ {
		if err := r.drainEventQueue(context.Background()); err != nil {
			t.Fatalf("drainEventQueue attempt %d: %v", i+1, err)
		}
	}
	owed = queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed)
	if len(owed) != 2 || owed[1].Status != domain.QueuedEventStatusFailed {
		t.Fatalf("obligation rows = %+v, want the first done and the second parked", owed)
	}
	pollGitHub(t, database, gh)
	owed = queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed)
	if len(owed) != 3 || owed[2].Status != domain.QueuedEventStatusPending {
		t.Fatalf("obligation rows after the park = %d, want a fresh pending one beside the parked row", len(owed))
	}
	if owed[1].Status != domain.QueuedEventStatusFailed {
		t.Error("the parked row was disturbed; it must stay on the failed-events panel")
	}
}

// TestCloseOwed_SourcePause_ClearedSnapshotIsNotTerminal: a paused source's
// cleared snapshot says nothing about whether the work is over, so the first
// cycle after re-enabling seeds quietly — closing a merged PR in the same
// write as its snapshot — and enqueues no obligation; the checker counts
// nothing at any point.
func TestCloseOwed_SourcePause_ClearedSnapshotIsNotTerminal(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, _, _ := seedStrandedPR(t, r, database)
	if _, err := sqlitestore.New(database).Entities.ClearSnapshotsForSourceSystem(context.Background(), runmode.LocalDefaultOrgID, "github"); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}
	if a, b, ok := r.checkOrgTerminalInvariant(context.Background(), runmode.LocalDefaultOrgID); !ok || a != 0 || b != 0 {
		t.Errorf("checker counts with the snapshot cleared = (%d, %d, ok=%v), want zeros", a, b, ok)
	}

	// The re-seed needs the REST node resolve a snapshot-less row takes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[` + prNode("MERGED", true) + `]}}`))
		case strings.Contains(r.URL.Path, "/pulls/9"):
			_, _ = w.Write([]byte(`{"number": 9, "node_id": "PR_owed", "title": "Owed PR", "state": "closed", "merged": true,
				"html_url": "https://github.com/octo/repo/pull/9", "user": {"login": "bob"},
				"head": {"sha": "sha9", "ref": "feat"}, "base": {"ref": "main"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	st := sqlitestore.New(database)
	tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.EventQueue, runmode.LocalDefaultOrgID)
	if _, _, err := tr.RefreshGitHub(context.Background(), ghclient.NewClient(srv.URL, "tok"), "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	if owed := queueRowsOfType(t, database, entityID, domain.EventSystemEntityCloseOwed); len(owed) != 0 {
		t.Errorf("obligation rows after the re-seed = %d, want 0 — a quiet seed announces nothing", len(owed))
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed — the terminal seed closes in the same write as its snapshot", got)
	}
}

// TestCloseOwed_Jira drives the Jira arm of the same loop: an issue active
// with a done snapshot, one cycle enqueues the obligation, routing closes the
// issue's task and entity, and a second cycle owes nothing.
func TestCloseOwed_Jira(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	seedJiraDoneRules(t, database, runmode.LocalDefaultTeamID, domain.JiraProjectStatusRules{
		ProjectKey: "SKY", PickupMembers: jiraRefs("To Do"),
		InProgressMembers: jiraRefs("In Progress"), InProgressCanonical: jiraRef("In Progress"),
		DoneMembers: jiraRefs("Done"), DoneCanonical: jiraRef("Done"),
	})
	const page = `{"issues":[
		{"key":"SKY-1","fields":{
			"summary":"Owed issue","status":{"name":"Done","id":"st-Done"},
			"assignee":{"displayName":"Alice","accountId":"acc-1"},
			"created":"2026-06-01T00:00:00.000+0000","updated":"2026-06-10T10:00:00.000+0000"
		}}
	],"total":1}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("unexpected jira request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "jira", "SKY-1", "issue", "Owed issue", "")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	snap, _ := json.Marshal(domain.JiraSnapshot{Key: "SKY-1", Summary: "Owed issue", Status: "Done", StatusID: "st-Done",
		Assignee: "Alice", AssigneeAccountID: "acc-1", UpdatedAt: "2026-06-10T10:00:00.000+0000"})
	if _, err := st.Entities.UpdateSnapshot(ctx, runmode.LocalDefaultOrgID, entity.ID, string(snap)); err != nil {
		t.Fatalf("seed done snapshot: %v", err)
	}
	evtID, err := st.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
		OrgID: runmode.LocalDefaultOrgID, EntityID: &entity.ID, EventType: domain.EventJiraIssueAssigned, MetadataJSON: "{}",
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := st.Tasks.FindOrCreateAtSystem(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, "", evtID, 0.5, time.Now())
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	poll := func() {
		t.Helper()
		tr := tracker.New(database, noopPublisher{}, st.Tasks, st.Entities, st.Repos, st.EventQueue, runmode.LocalDefaultOrgID)
		rules := tracker.JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}
		if _, err := tr.RefreshJira(ctx, jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat")), srv.URL, rules); err != nil {
			t.Fatalf("RefreshJira: %v", err)
		}
	}

	poll()
	if owed := queueRowsOfType(t, database, entity.ID, domain.EventSystemEntityCloseOwed); len(owed) != 1 {
		t.Fatalf("obligation rows = %d, want 1", len(owed))
	}
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := entityState(t, database, entity.ID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
	if status, reason := taskCloseReason(t, database, task.ID); status != "done" || reason != closeReasonReconciled {
		t.Errorf("task = (%q, %q), want (done, %q)", status, reason, closeReasonReconciled)
	}
	poll()
	if owed := queueRowsOfType(t, database, entity.ID, domain.EventSystemEntityCloseOwed); len(owed) != 1 {
		t.Errorf("obligation rows after repair = %d, want still 1", len(owed))
	}
}

// TestEntityCloseSettlingEventTypes_MatchTheRouterDerivation keeps the two
// spellings of "which queue rows decide an entity's fate" equal: the domain
// list the store layer reads, and the set the router derives from its close
// relations plus the obligation.
func TestEntityCloseSettlingEventTypes_MatchTheRouterDerivation(t *testing.T) {
	want := map[string]bool{}
	for et := range EntityTerminatingEvents {
		want[et] = true
	}
	got := map[string]bool{}
	for _, et := range domain.EntityCloseSettlingEventTypes() {
		got[et] = true
	}
	for et := range want {
		if !got[et] {
			t.Errorf("%q terminates an entity in the router but is missing from domain.EntityCloseSettlingEventTypes", et)
		}
	}
	for et := range got {
		if !want[et] {
			t.Errorf("%q is in domain.EntityCloseSettlingEventTypes but terminates nothing in the router", et)
		}
	}
	if !EntityTerminatingEvents[domain.EventSystemEntityCloseOwed] {
		t.Error("the close obligation is not a terminating event")
	}
}

// TestCloseOwed_IsNotRouterBound pins how the obligation reaches the queue:
// through the tracker's snapshot-CAS enqueue, never through ingest — so the
// router-bound set need not admit the system prefix, and ingest keeps
// treating every system:* event as bus-only.
func TestCloseOwed_IsNotRouterBound(t *testing.T) {
	if RouterBound(domain.EventSystemEntityCloseOwed) {
		t.Error("the obligation is router-bound; ingest would enqueue it, and the system prefix would be open to every sentinel")
	}
}

// TestCloseOwed_CloseSetSparesTheTerminatingEvents pins the obligation's
// close set: every task type a terminating transition would close, and
// never a terminating event's own type, since a task of one of those may be
// the lifecycle task riding the run its transition started.
func TestCloseOwed_CloseSetSparesTheTerminatingEvents(t *testing.T) {
	set := map[string]bool{}
	for _, et := range closeOwedCloseTypes() {
		if set[et] {
			t.Errorf("%q appears twice in the obligation's close set", et)
		}
		set[et] = true
	}
	for _, spared := range []string{domain.EventGitHubPRMerged, domain.EventGitHubPRClosed, domain.EventJiraIssueCompleted, domain.EventJiraIssueUnreachable} {
		if set[spared] {
			t.Errorf("%q is in the obligation's close set; a lifecycle task riding its run would be cancelled", spared)
		}
	}
	for _, covered := range append(append(githubPRTerminalCloseTypes(), jiraIssueTerminalCloseTypes()...), jiraIssueUnreachableCloseTypes()...) {
		if !set[covered] && !domain.TaskMayRideClosedEntity(covered) {
			t.Errorf("%q is closed by a real transition but not by the obligation", covered)
		}
	}
}
