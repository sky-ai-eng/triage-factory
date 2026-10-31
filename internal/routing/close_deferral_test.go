package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A run whose own action produced the event that closes its task finishes
// before the task closes: the close is held on the task and the sweep applies
// it when the run ends. Every other task the event closes closes at once.

// pushedSHA is the commit enqueueCIPassed reports CI passing on.
const pushedSHA = "abc123"

// recordAgentAction writes a ledger row the run's agent performed on
// conversationID, the way the agent's recording funnel does.
func recordAgentAction(t *testing.T, database *sql.DB, conversationID string, a domain.ExternalAction) {
	t.Helper()
	a.ConversationID = conversationID
	a.PerformedBy = domain.PerformedByAgent
	if a.Provider == "" {
		a.Provider = domain.ProviderGitHub
	}
	a.Scope = "github.com"
	a.Target = "owner/repo#1"
	a.Credential = domain.CredentialGitHubApp
	if err := sqlitestore.New(database).ExternalActions.RecordSystem(t.Context(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("record %s: %v", a.Action, err)
	}
}

func recordAgentPush(t *testing.T, database *sql.DB, conversationID, sha string) {
	t.Helper()
	recordAgentAction(t, database, conversationID, domain.ExternalAction{
		Action: domain.ActionBranchPushed, ExternalID: "refs/heads/tf/fix",
		DetailJSON: `{"sha":"` + sha + `","new":false}`,
	})
}

// taskDeferral reads a task's two deferral columns, "" for NULL.
func taskDeferral(t *testing.T, database *sql.DB, taskID string) (eventID, blueprintRunID string) {
	t.Helper()
	var ev, run sql.NullString
	if err := database.QueryRow(`SELECT close_deferred_event_id, close_deferred_blueprint_run_id FROM tasks WHERE id = ?`, taskID).Scan(&ev, &run); err != nil {
		t.Fatalf("read task %s's deferral: %v", taskID, err)
	}
	return ev.String, run.String
}

func taskRow(t *testing.T, database *sql.DB, taskID string) (status, closeReason, closeEventType string) {
	t.Helper()
	var reason, eventType sql.NullString
	if err := database.QueryRow(`SELECT status, close_reason, close_event_type FROM tasks WHERE id = ?`, taskID).Scan(&status, &reason, &eventType); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return status, reason.String, eventType.String
}

// secondCIFailedTask mints a ci_check_failed task for another check on the
// entity: a task the same pass closes that no run is working on.
func secondCIFailedTask(t *testing.T, r *Router, database *sql.DB, entityID, firstTaskID string) string {
	t.Helper()
	enqueueCIFailedCheck(t, database, entityID, "lint")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second ci event: %v", err)
	}
	for _, task := range activeCIFailedTasks(t, database, entityID) {
		if task.ID != firstTaskID {
			return task.ID
		}
	}
	t.Fatal("setup: no second ci_check_failed task")
	return ""
}

// deferredCIFixture is a ci_check_failed task whose running run pushed the
// commit CI then passed on, with its close already deferred by that pass.
type deferredCIFixture struct {
	r                *Router
	database         *sql.DB
	sp               *stoppingSpawner
	entityID, taskID string
	runID, convID    string
	eventID          string
}

func newDeferredCIFixture(t *testing.T, sourceID string) deferredCIFixture {
	t.Helper()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, sourceID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentPush(t, database, convID, pushedSHA)
	seedPRCheckRuns(t, database, entityID, domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"})
	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}
	eventID, deferredRun := taskDeferral(t, database, taskID)
	if eventID == "" || deferredRun != runID {
		t.Fatalf("setup: deferral = (%q, %q), want the pass and run %s", eventID, deferredRun, runID)
	}
	return deferredCIFixture{r: r, database: database, sp: sp, entityID: entityID, taskID: taskID, runID: runID, convID: convID, eventID: eventID}
}

func setRunStatus(t *testing.T, database *sql.DB, runID, status string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = ? WHERE id = ?`, status, runID); err != nil {
		t.Fatalf("set run %s status %s: %v", runID, status, err)
	}
}

func setConversationStatus(t *testing.T, database *sql.DB, convID, status string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, status, convID); err != nil {
		t.Fatalf("set conversation %s status %s: %v", convID, status, err)
	}
}

// TestCloseDeferral_CIPassFromTheRunsPushDefersItsTask is the headline. The
// run pushed the fix CI passed on: its task stays open with the close held,
// and nothing stops the run. The pass's other ci_check_failed task, which no
// run is working on, closes as it always has.
func TestCloseDeferral_CIPassFromTheRunsPushDefersItsTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#own-push")
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentPush(t, database, convID, pushedSHA)
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		domain.CheckRun{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"},
	)

	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Fatal("the run's task closed under it")
	}
	if eventID, deferredRun := taskDeferral(t, database, taskID); eventID == "" || deferredRun != runID {
		t.Errorf("deferral = (%q, %q), want the pass and run %s", eventID, deferredRun, runID)
	}
	if got := sp.stoppedIDs(); slices.Contains(got, convID) {
		t.Errorf("the run's conversation was stopped: %v", got)
	}
	if cancelRequested(t, database, runID) {
		t.Error("the run was called off")
	}
	if n := closeAuditCount(t, database, taskID); n != 0 {
		t.Errorf("close audit rows on the deferred task = %d, want 0", n)
	}
	if slices.Contains(sp.tornDownCopy(), taskID) {
		t.Error("the deferred task's deliverables were torn down")
	}
	if status, reason, _ := taskRow(t, database, otherID); status != "done" || reason != "auto_closed_by_event" {
		t.Errorf("other task = (%q, %q), want closed by the event", status, reason)
	}
}

// TestCloseDeferral_StoppedRunGetsNoDeferral pins the first condition: a run
// whose newest conversation is parked open without its step's conclusion is
// stopped, so the pass closes its task now, exactly as it would have before.
func TestCloseDeferral_StoppedRunGetsNoDeferral(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	r.spawner = &stoppingSpawner{}

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#stopped-run")
	_, convID := seedRunOnTask(t, database, taskID, "running", domain.StatusOpen)
	recordAgentPush(t, database, convID, pushedSHA)
	seedPRCheckRuns(t, database, entityID, domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"})

	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status != "done" {
		t.Errorf("task status = %q, want done: a stopped run gets no deferral", status)
	}
	if eventID, _ := taskDeferral(t, database, taskID); eventID != "" {
		t.Errorf("a stopped run's task was deferred on %s", eventID)
	}
}

// TestCloseDeferral_TerminatingCloseDefersTheCausingTask pins the terminating
// arm: the run merged its pull request, so its task is held while the merge
// closes the entity's other task and the entity itself, in one transaction.
func TestCloseDeferral_TerminatingCloseDefersTheCausingTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#own-merge")
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentAction(t, database, convID, domain.ExternalAction{Action: domain.ActionPRMerged, EntityID: entityID})

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the merge: %v", err)
	}

	var state string
	if err := database.QueryRow(`SELECT state FROM entities WHERE id = ?`, entityID).Scan(&state); err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if state != "closed" {
		t.Errorf("entity state = %q, want closed: the entity closes whether or not a task deferred", state)
	}
	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Error("the merging run's task closed under it")
	}
	if eventID, deferredRun := taskDeferral(t, database, taskID); eventID == "" || deferredRun != runID {
		t.Errorf("deferral = (%q, %q), want the merge and run %s", eventID, deferredRun, runID)
	}
	if cancelRequested(t, database, runID) || slices.Contains(sp.stoppedIDs(), convID) {
		t.Error("the merging run was stopped")
	}
	if status, reason, _ := taskRow(t, database, otherID); status != "done" || reason != "entity_closed" {
		t.Errorf("other task = (%q, %q), want closed with entity_closed", status, reason)
	}
}

// TestCloseDeferral_IssueCompletionMatchesTheRunsLastTransition pins the
// transition matcher on the Jira and Linear completions: the run's newest
// issue_transitioned row on the entity explains the completion when its
// to_state is the status the issue finished in, ignoring case, and not when
// it names another status.
func TestCloseDeferral_IssueCompletionMatchesTheRunsLastTransition(t *testing.T) {
	cases := []struct {
		name, eventType, toState, final string
		want                            bool
	}{
		{"jira_same_status_other_case", domain.EventJiraIssueCompleted, "Done", "DONE", true},
		{"jira_other_status", domain.EventJiraIssueCompleted, "Done", "Closed", false},
		{"linear_same_status_other_case", domain.EventLinearIssueCompleted, "done", "Done", true},
		{"linear_other_status", domain.EventLinearIssueCompleted, "Done", "Canceled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#"+tc.name)
			runID, convID := seedRunOnTask(t, database, taskID, "running", "")
			recordAgentAction(t, database, convID, domain.ExternalAction{
				Provider: domain.ProviderJira, Action: domain.ActionIssueTransitioned,
				EntityID: entityID, ToState: tc.toState,
			})

			var meta []byte
			if tc.eventType == domain.EventJiraIssueCompleted {
				meta, _ = json.Marshal(events.JiraIssueCompletedMetadata{IssueKey: "SKY-1", FinalStatus: tc.final})
			} else {
				meta, _ = json.Marshal(events.LinearIssueCompletedMetadata{FinalStatus: tc.final})
			}
			evt := domain.Event{ID: uuid.New().String(), EventType: tc.eventType, MetadataJSON: string(meta)}

			rel := terminatingRelationFor(tc.eventType)
			if rel == nil {
				t.Fatalf("no terminating relation for %s", tc.eventType)
			}
			got, err := r.causingRun(t.Context(), runmode.LocalDefaultOrgID, evt, entityID, rel, taskID)
			if err != nil {
				t.Fatalf("causingRun: %v", err)
			}
			if want := map[bool]string{true: runID, false: ""}[tc.want]; got != want {
				t.Errorf("causingRun = %q, want %q", got, want)
			}
		})
	}
}

// TestCloseDeferralSweep_CompletedRunClearsWithoutClosing pins the sweep's
// hand-off: while the run works the deferral waits, and once the run
// completes its own ending owns the task, so the sweep drops the deferral and
// closes nothing.
func TestCloseDeferralSweep_CompletedRunClearsWithoutClosing(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-completed")

	f.r.sweepCloseDeferrals(t.Context())
	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != f.eventID {
		t.Fatalf("a working run's deferral changed to %q", eventID)
	}

	setRunStatus(t, f.database, f.runID, "completed")
	setConversationStatus(t, f.database, f.convID, domain.StatusOpen)
	if _, err := f.database.Exec(`UPDATE conversations SET completed_at = CURRENT_TIMESTAMP WHERE id = ?`, f.convID); err != nil {
		t.Fatalf("conclude conversation: %v", err)
	}
	f.r.sweepCloseDeferrals(t.Context())

	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != "" {
		t.Errorf("deferral still held after the run completed: %s", eventID)
	}
	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose run completed: that close is the run's own")
	}
	if n := closeAuditCount(t, f.database, f.taskID); n != 0 {
		t.Errorf("close audit rows = %d, want 0", n)
	}
	if len(f.sp.tornDownCopy()) != 0 {
		t.Errorf("teardowns = %v, want none", f.sp.tornDownCopy())
	}
}

// TestCloseDeferralSweep_FailedRunAppliesTheOrdinaryClose pins the apply: a
// run that failed gets the close the event owed its task, through the
// ordinary path — the event's reason and type, its audit row, and the
// deliverable teardown.
func TestCloseDeferralSweep_FailedRunAppliesTheOrdinaryClose(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-failed")
	setRunStatus(t, f.database, f.runID, "failed")
	setConversationStatus(t, f.database, f.convID, domain.StatusFailed)

	f.r.sweepCloseDeferrals(t.Context())

	status, reason, eventType := taskRow(t, f.database, f.taskID)
	if status != "done" || reason != "auto_closed_by_event" || eventType != domain.EventGitHubPRCICheckPassed {
		t.Errorf("task = (%q, %q, %q), want done by the ci_check_passed close", status, reason, eventType)
	}
	var auditEvent string
	if err := f.database.QueryRow(`SELECT event_id FROM task_events WHERE task_id = ? AND kind = 'closed'`, f.taskID).Scan(&auditEvent); err != nil {
		t.Fatalf("read close audit row: %v", err)
	}
	if auditEvent != f.eventID {
		t.Errorf("close audit row names event %s, want the deferred %s", auditEvent, f.eventID)
	}
	if got := f.sp.tornDownCopy(); !slices.Equal(got, []string{f.taskID}) {
		t.Errorf("teardowns = %v, want [%s]", got, f.taskID)
	}
}

// TestCloseDeferralSweep_StoppedRunningRunAppliesTheClose pins the other way
// a run ends for the sweep: still marked running, with its newest
// conversation parked without a conclusion. The close then stops it, as an
// undeferred close would have.
func TestCloseDeferralSweep_StoppedRunningRunAppliesTheClose(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-stalled")
	setConversationStatus(t, f.database, f.convID, domain.StatusOpen)

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status != "done" {
		t.Errorf("task status = %q, want done", status)
	}
	if !slices.Contains(f.sp.stoppedIDs(), f.convID) {
		t.Errorf("stops = %v, want the parked conversation %s", f.sp.stoppedIDs(), f.convID)
	}
	if !cancelRequested(t, f.database, f.runID) {
		t.Error("the run's cancel intent was not set")
	}
}

// TestCloseDeferralSweep_RefusingPrepareClearsWithoutClosing pins the re-check:
// by the time the run failed, CI is red again, so the close the pass owed no
// longer holds, and the sweep drops it rather than closing a task whose
// failure is back.
func TestCloseDeferralSweep_RefusingPrepareClearsWithoutClosing(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-red-again")
	setRunStatus(t, f.database, f.runID, "failed")
	setConversationStatus(t, f.database, f.convID, domain.StatusFailed)
	seedPRCheckRuns(t, f.database, f.entityID, domain.CheckRun{ID: 5, Name: "build", Status: "completed", Conclusion: "failure"})

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose CI is failing again")
	}
	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != "" {
		t.Errorf("deferral still held after its close stopped holding: %s", eventID)
	}
	if n := closeAuditCount(t, f.database, f.taskID); n != 0 {
		t.Errorf("close audit rows = %d, want 0", n)
	}
}

// racingDeferralStore stands in for the close phase landing between the
// sweep's read and its clear: right after the list read, it overwrites the
// task's deferral with a newer event the same run caused.
type racingDeferralStore struct {
	dbpkg.TaskStore
	taskID, newerEventID, runID string
}

func (s racingDeferralStore) ListActiveCloseDeferralsSystem(ctx context.Context) ([]domain.TaskCloseDeferral, error) {
	out, err := s.TaskStore.ListActiveCloseDeferralsSystem(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.DeferCloseSystem(ctx, runmode.LocalDefaultOrgID, s.taskID, s.newerEventID, s.runID); err != nil {
		return nil, err
	}
	return out, nil
}

// TestCloseDeferralSweep_OverwrittenDeferralSurvivesTheClear pins the
// compare-and-clear: the run completed, so the sweep clears the deferral it
// read, but a newer event replaced it in between, and that one survives for
// the next pass.
func TestCloseDeferralSweep_OverwrittenDeferralSurvivesTheClear(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-overwritten")
	newer := uuid.New().String()
	if _, err := f.database.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, ?, '', '{}', CURRENT_TIMESTAMP)
	`, newer, f.entityID, domain.EventGitHubPRCICheckPassed); err != nil {
		t.Fatalf("seed newer event: %v", err)
	}
	setRunStatus(t, f.database, f.runID, "completed")
	f.r.tasks = racingDeferralStore{TaskStore: f.r.tasks, taskID: f.taskID, newerEventID: newer, runID: f.runID}

	f.r.sweepCloseDeferrals(t.Context())

	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != newer {
		t.Errorf("deferral = %q, want the newer event %s to survive the stale clear", eventID, newer)
	}
}
