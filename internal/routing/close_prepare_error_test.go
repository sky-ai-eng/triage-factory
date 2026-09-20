package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A close relation's prepare hook reads the store to decide whether to
// close. A store error there used to read as "gate closed": the typed close
// was skipped for good on an event the queue then consumed. These tests pin
// the promotion: the error joins the close phase's routing obligation, the
// event requeues under the transient outcome with no task closed, and the
// replay closes the task exactly once.
//
// The failing store is targeted at the hook rather than at the first read
// of the pass: entity resolution reads the entity too, and a failure there
// is a different stage's contract (obligation_test.go). calledFrom inspects
// the stack for the hook's frame, which is what makes the failure land on
// the read the gate makes rather than on whichever read comes first.

// calledFrom reports whether any caller on the stack is the named function.
func calledFrom(fn string) bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}

// prepareOutageEntityStore fails the entity read only when the close phase's
// snapshot gate makes it, for the first `remaining` such reads.
type prepareOutageEntityStore struct {
	dbpkg.EntityStore
	o *outage
}

func (s prepareOutageEntityStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if calledFrom("prSnapshotForEntity") && s.o.down() {
		return nil, errOutage
	}
	return s.EntityStore.GetSystem(ctx, orgID, id)
}

// prepareOutageOrgsStore fails the org-settings read only when the Jira
// reassign gate makes it.
type prepareOutageOrgsStore struct {
	dbpkg.OrgsStore
	o *outage
}

func (s prepareOutageOrgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	if calledFrom("prepareJiraReassign") && s.o.down() {
		return domain.OrgSettings{}, errOutage
	}
	return s.OrgsStore.GetSettingsSystem(ctx, orgID)
}

// enqueueReviewEvent durably enqueues a review event by the named reviewer.
func enqueueReviewEvent(t *testing.T, database *sql.DB, entityID, eventType, reviewer string) {
	t.Helper()
	eid := entityID
	meta, _ := json.Marshal(map[string]any{"reviewer": reviewer, "author": "aidan", "repo": "owner/repo", "pr_number": 1})
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID: runmode.LocalDefaultOrgID, EntityID: &eid, EventType: eventType, MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue %s: %v", eventType, err)
	}
}

// seedTaskOfType mints an active task of eventType on the entity, the way a
// prior routed event would have.
func seedTaskOfType(t *testing.T, database *sql.DB, entityID, eventType, dedupKey string) string {
	t.Helper()
	eid := entityID
	evtID, err := sqlitestore.New(database).Events.RecordSystem(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID: runmode.LocalDefaultOrgID, EntityID: &eid, EventType: eventType, DedupKey: dedupKey, MetadataJSON: "{}",
	})
	if err != nil {
		t.Fatalf("record seed event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreateAtSystem(context.Background(), runmode.LocalDefaultOrgID,
		runmode.LocalDefaultTeamID, entityID, eventType, dedupKey, evtID, 0.5, time.Now())
	if err != nil {
		t.Fatalf("seed %s task: %v", eventType, err)
	}
	return task.ID
}

// assertRequeuedTransient reads the one queue row of eventType and asserts
// it requeued under the transient outcome with the cause on it.
func assertRequeuedTransient(t *testing.T, database *sql.DB, eventType string) {
	t.Helper()
	var status, outcome, lastErr string
	if err := database.QueryRow(`SELECT status, COALESCE(last_outcome,''), COALESCE(last_error,'') FROM event_queue WHERE event_type = ?`, eventType).
		Scan(&status, &outcome, &lastErr); err != nil {
		t.Fatalf("read the %s row: %v", eventType, err)
	}
	if status != domain.QueuedEventStatusReady || outcome != string(workitem.OutcomeTransient) {
		t.Errorf("row after the failed prepare = (%s, %s), want (ready, transient)", status, outcome)
	}
	if !strings.Contains(lastErr, errOutage.Error()) {
		t.Errorf("last_error = %q, want the store failure the gate hit", lastErr)
	}
}

func assertDone(t *testing.T, database *sql.DB, eventType string) {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM event_queue WHERE event_type = ?`, eventType).Scan(&status); err != nil {
		t.Fatalf("read the %s row: %v", eventType, err)
	}
	if status != domain.QueuedEventStatusDone {
		t.Errorf("row after the replay = %q, want done", status)
	}
}

func TestClosePrepare_CIPassedSnapshotReadFails_RequeuesThenClosesOnce(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#prepare-ci")
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
	)
	r.entities = prepareOutageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: 1}}

	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	assertRequeuedTransient(t, database, domain.EventGitHubPRCICheckPassed)
	if live := activeCIFailedTasks(t, database, entityID); len(live) != 1 {
		t.Fatalf("active ci_check_failed tasks after the failed gate = %d, want 1 — nothing closed", len(live))
	}

	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	assertDone(t, database, domain.EventGitHubPRCICheckPassed)
	if live := activeCIFailedTasks(t, database, entityID); len(live) != 0 {
		t.Errorf("active ci_check_failed tasks after the replay = %d, want 0", len(live))
	}
	if n := closeAuditCount(t, database, taskID); n != 1 {
		t.Errorf("close audit rows on the task = %d, want exactly 1", n)
	}
}

func TestClosePrepare_ReviewResolvedSnapshotReadFails_RequeuesThenClosesOnce(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	entityID := newEntity(t, database, "owner/repo#prepare-review")
	taskID := seedTaskOfType(t, database, entityID, domain.EventGitHubPRReviewChangesRequested, "")
	snap, _ := json.Marshal(domain.PRSnapshot{Reviews: []domain.ReviewState{{Author: "bob", State: "CHANGES_REQUESTED"}}})
	if _, err := database.Exec(`UPDATE entities SET snapshot_json = ? WHERE id = ?`, string(snap), entityID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	r.entities = prepareOutageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: 1}}

	enqueueReviewEvent(t, database, entityID, domain.EventGitHubPRReviewApproved, "bob")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	assertRequeuedTransient(t, database, domain.EventGitHubPRReviewApproved)
	if status, _ := taskCloseReason(t, database, taskID); status != "queued" {
		t.Fatalf("task after the failed gate = %q, want still queued", status)
	}

	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	assertDone(t, database, domain.EventGitHubPRReviewApproved)
	if status, _ := taskCloseReason(t, database, taskID); status != "done" {
		t.Errorf("task after the replay = %q, want done", status)
	}
	if n := closeAuditCount(t, database, taskID); n != 1 {
		t.Errorf("close audit rows on the task = %d, want exactly 1", n)
	}
}

func TestClosePrepare_JiraReassignSettingsReadFails_RequeuesThenClosesOnce(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	r.users = st.Users
	entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "PROJ-77", "issue", "Issue", "https://jira.example.com/PROJ-77")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	taskID := seedTaskOfType(t, database, entity.ID, domain.EventJiraIssueAssigned, "")
	r.orgs = prepareOutageOrgsStore{OrgsStore: st.Orgs, o: &outage{remaining: 1}}

	meta, _ := json.Marshal(events.JiraIssueAssignedMetadata{Assignee: "Carol", AssigneeAccountID: "acc-carol"})
	eid := entity.ID
	if _, err := st.EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID: runmode.LocalDefaultOrgID, EntityID: &eid, EventType: domain.EventJiraIssueAssigned, MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue jira assigned: %v", err)
	}
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	assertRequeuedTransient(t, database, domain.EventJiraIssueAssigned)
	if status, _ := taskCloseReason(t, database, taskID); status != "queued" {
		t.Fatalf("task after the failed gate = %q, want still queued — an unreadable assignee must not retire it", status)
	}

	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	assertDone(t, database, domain.EventJiraIssueAssigned)
	if status, _ := taskCloseReason(t, database, taskID); status != "done" {
		t.Errorf("task after the replay = %q, want done — reassigned away from its team", status)
	}
	if n := closeAuditCount(t, database, taskID); n != 1 {
		t.Errorf("close audit rows on the task = %d, want exactly 1", n)
	}
}

// TestPRSnapshotForEntity_DistinguishesErrorFromDecline pins the hook's
// three answers at the unit level: a store error is an error, a missing
// entity declines without one, and an unparsable snapshot declines too.
func TestPRSnapshotForEntity_DistinguishesErrorFromDecline(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	entityID := newEntity(t, database, "owner/repo#snapshot-shapes")

	r.entities = outageEntityStore{EntityStore: st.Entities, o: &outage{remaining: 1}}
	if _, ok, err := r.prSnapshotForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID); !errors.Is(err, errOutage) || ok {
		t.Errorf("store error: ok=%v err=%v, want the error surfaced", ok, err)
	}
	if _, ok, err := r.prSnapshotForEntity(context.Background(), runmode.LocalDefaultOrgID, "no-such-entity"); err != nil || ok {
		t.Errorf("missing entity: ok=%v err=%v, want a plain decline", ok, err)
	}
	if _, err := database.Exec(`UPDATE entities SET snapshot_json = 'not json' WHERE id = ?`, entityID); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	if _, ok, err := r.prSnapshotForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID); err != nil || ok {
		t.Errorf("corrupt snapshot: ok=%v err=%v, want a plain decline", ok, err)
	}
}
