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
	if a.Scope == "" {
		a.Scope = "github.com"
	}
	if a.Target == "" {
		a.Target = "owner/repo#1"
	}
	if a.Credential == "" {
		a.Credential = domain.CredentialGitHubApp
	}
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
// it names another status. Only the newest counts: a run that moved the issue
// to done and then back did not cause its completion. The transitions are
// written back to back, inside one second on SQLite, so the write order is
// what decides which is newest.
func TestCloseDeferral_IssueCompletionMatchesTheRunsLastTransition(t *testing.T) {
	cases := []struct {
		name, eventType string
		toStates        []string // in the order the run made them
		final           string
		want            bool
	}{
		{"jira_same_status_other_case", domain.EventJiraIssueCompleted, []string{"Done"}, "DONE", true},
		{"jira_other_status", domain.EventJiraIssueCompleted, []string{"Done"}, "Closed", false},
		{"jira_done_then_back", domain.EventJiraIssueCompleted, []string{"Done", "In Progress"}, "Done", false},
		{"jira_back_then_done", domain.EventJiraIssueCompleted, []string{"In Progress", "Done"}, "Done", true},
		{"linear_same_status_other_case", domain.EventLinearIssueCompleted, []string{"done"}, "Done", true},
		{"linear_other_status", domain.EventLinearIssueCompleted, []string{"Done"}, "Canceled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#"+tc.name)
			runID, convID := seedRunOnTask(t, database, taskID, "running", "")
			provider, scope := domain.ProviderJira, "example.atlassian.net"
			if tc.eventType == domain.EventLinearIssueCompleted {
				provider, scope = domain.ProviderLinear, "linear-workspace"
			}
			for _, to := range tc.toStates {
				recordAgentAction(t, database, convID, domain.ExternalAction{
					Provider: provider, Scope: scope, Action: domain.ActionIssueTransitioned,
					Target: "SKY-1", EntityID: entityID, ToState: to, ObjectKind: domain.ObjectKindIssue,
				})
			}

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

// TestCloseDeferral_PRTerminationMatchesItsOwnAction pins the pull request
// matcher's two arms: a pr_merged row explains github:pr:merged and a
// pr_closed row github:pr:closed, each on the event's entity, and neither
// explains the other.
func TestCloseDeferral_PRTerminationMatchesItsOwnAction(t *testing.T) {
	cases := []struct {
		name, eventType, action string
		want                    bool
	}{
		{"merged_by_merge", domain.EventGitHubPRMerged, domain.ActionPRMerged, true},
		{"closed_by_close", domain.EventGitHubPRClosed, domain.ActionPRClosed, true},
		{"merged_not_by_close", domain.EventGitHubPRMerged, domain.ActionPRClosed, false},
		{"closed_not_by_merge", domain.EventGitHubPRClosed, domain.ActionPRMerged, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#"+tc.name)
			runID, convID := seedRunOnTask(t, database, taskID, "running", "")
			recordAgentAction(t, database, convID, domain.ExternalAction{Action: tc.action, EntityID: entityID})

			evt := domain.Event{ID: uuid.New().String(), EventType: tc.eventType, MetadataJSON: "{}"}
			got, err := r.causingRun(t.Context(), runmode.LocalDefaultOrgID, evt, entityID, terminatingRelationFor(tc.eventType), taskID)
			if err != nil {
				t.Fatalf("causingRun: %v", err)
			}
			if want := map[bool]string{true: runID, false: ""}[tc.want]; got != want {
				t.Errorf("causingRun = %q, want %q", got, want)
			}
		})
	}
}

// enqueueClosed durably enqueues the terminating github:pr:closed event.
func enqueueClosed(t *testing.T, database *sql.DB, entityID string) {
	t.Helper()
	eid := entityID
	meta, _ := json.Marshal(events.GitHubPRClosedMetadata{Author: "aidan", Repo: "owner/repo", PRNumber: 1, ClosedBy: "aidan"})
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRClosed,
		MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue pr:closed: %v", err)
	}
}

// pollCyclesPass moves every recorded event and ledger row an hour into the
// past. It stands in for the poll cycles between one close and the next,
// which a test otherwise runs inside one second, where SQLite's whole-second
// ledger stamps cannot place a row written after a close later than it.
func pollCyclesPass(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE events SET created_at = datetime(created_at, '-1 hour')`); err != nil {
		t.Fatalf("age events: %v", err)
	}
	if _, err := database.Exec(`UPDATE external_actions SET occurred_at = datetime(occurred_at, '-1 hour')`); err != nil {
		t.Fatalf("age ledger rows: %v", err)
	}
}

// reopenEntity is the tracker's reactivation of a closed entity it saw open
// again, a poll cycle after the close.
func reopenEntity(t *testing.T, database *sql.DB, entityID string) {
	t.Helper()
	pollCyclesPass(t, database)
	if _, err := database.Exec(`UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ?`, entityID); err != nil {
		t.Fatalf("reopen entity: %v", err)
	}
}

// newDeferredCloseThenReopenFixture is a ci_check_failed task whose running
// run closed its pull request, with the task's close held by that close, and
// the pull request reopened since while the run keeps working.
func newDeferredCloseThenReopenFixture(t *testing.T, sourceID string) deferredCIFixture {
	t.Helper()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, sourceID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentAction(t, database, convID, domain.ExternalAction{Action: domain.ActionPRClosed, EntityID: entityID})
	enqueueClosed(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the run's close: %v", err)
	}
	eventID, deferredRun := taskDeferral(t, database, taskID)
	if eventID == "" || deferredRun != runID {
		t.Fatalf("setup: deferral = (%q, %q), want the close and run %s", eventID, deferredRun, runID)
	}
	reopenEntity(t, database, entityID)
	return deferredCIFixture{r: r, database: database, sp: sp, entityID: entityID, taskID: taskID, runID: runID, convID: convID, eventID: eventID}
}

// TestCloseDeferral_PersonsCloseAfterAReopenStopsTheRun pins that a ledger
// row explains one close: the run closed the pull request and a person
// reopened it, so the person's next close is not the run's. It closes the
// task and stops the run the way a close the run did not cause always has.
func TestCloseDeferral_PersonsCloseAfterAReopenStopsTheRun(t *testing.T) {
	f := newDeferredCloseThenReopenFixture(t, "owner/repo#close-reopen-person")

	enqueueClosed(t, f.database, f.entityID)
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the person's close: %v", err)
	}

	if status, reason, eventType := taskRow(t, f.database, f.taskID); status != "done" || reason != "entity_closed" || eventType != domain.EventGitHubPRClosed {
		t.Errorf("task = (%q, %q, %q), want done by the person's close", status, reason, eventType)
	}
	if !slices.Contains(f.sp.stoppedIDs(), f.convID) {
		t.Errorf("stops = %v, want the run's conversation %s", f.sp.stoppedIDs(), f.convID)
	}
	if !cancelRequested(t, f.database, f.runID) {
		t.Error("the run was not called off")
	}
}

// TestCloseDeferral_RunsOwnCloseAfterAReopenDefers pins the other side: the
// run closed the pull request, a person reopened it, and the run closed it
// again. The second close is the run's own, written after the first, so it
// is held like the first.
func TestCloseDeferral_RunsOwnCloseAfterAReopenDefers(t *testing.T) {
	f := newDeferredCloseThenReopenFixture(t, "owner/repo#close-reopen-run")
	recordAgentAction(t, f.database, f.convID, domain.ExternalAction{Action: domain.ActionPRClosed, EntityID: f.entityID})

	enqueueClosed(t, f.database, f.entityID)
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the run's second close: %v", err)
	}

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Fatal("the run's task closed under it")
	}
	eventID, deferredRun := taskDeferral(t, f.database, f.taskID)
	if eventID == f.eventID || deferredRun != f.runID {
		t.Errorf("deferral = (%q, %q), want the second close and run %s", eventID, deferredRun, f.runID)
	}
	if cancelRequested(t, f.database, f.runID) || slices.Contains(f.sp.stoppedIDs(), f.convID) {
		t.Error("the closing run was stopped")
	}
}

// TestCloseDeferral_IssueCompletionAfterAReopenMatchesOnlyTheRunsLaterMove
// pins the transition matcher across a reopen, for Jira and Linear: the run
// moved the issue to Done, it completed, and a person reopened it. A person's
// move back to Done is not the run's; the run's own move after the reopen is.
func TestCloseDeferral_IssueCompletionAfterAReopenMatchesOnlyTheRunsLaterMove(t *testing.T) {
	cases := []struct {
		name, eventType string
		runMovesItAgain bool
		wantCausedByRun bool
	}{
		{"jira_persons_done", domain.EventJiraIssueCompleted, false, false},
		{"jira_runs_done", domain.EventJiraIssueCompleted, true, true},
		{"linear_persons_done", domain.EventLinearIssueCompleted, false, false},
		{"linear_runs_done", domain.EventLinearIssueCompleted, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#"+tc.name)
			runID, convID := seedRunOnTask(t, database, taskID, "running", "")
			provider, scope := domain.ProviderJira, "example.atlassian.net"
			meta, _ := json.Marshal(events.JiraIssueCompletedMetadata{IssueKey: "SKY-1", FinalStatus: "Done"})
			if tc.eventType == domain.EventLinearIssueCompleted {
				provider, scope = domain.ProviderLinear, "linear-workspace"
				meta, _ = json.Marshal(events.LinearIssueCompletedMetadata{FinalStatus: "Done"})
			}
			moveToDone := func() {
				recordAgentAction(t, database, convID, domain.ExternalAction{
					Provider: provider, Scope: scope, Action: domain.ActionIssueTransitioned,
					Target: "SKY-1", EntityID: entityID, ToState: "Done", ObjectKind: domain.ObjectKindIssue,
				})
			}
			complete := func() domain.Event {
				evt := domain.Event{EntityID: &entityID, EventType: tc.eventType, MetadataJSON: string(meta)}
				id, err := sqlitestore.New(database).Events.RecordSystem(t.Context(), runmode.LocalDefaultOrgID, evt)
				if err != nil {
					t.Fatalf("record %s: %v", tc.eventType, err)
				}
				evt.ID = id
				return evt
			}

			moveToDone()
			complete()
			// A person reopens the issue, and the tracker sees it a poll
			// cycle later.
			pollCyclesPass(t, database)
			if tc.runMovesItAgain {
				moveToDone()
			}
			evt := complete()

			got, err := r.causingRun(t.Context(), runmode.LocalDefaultOrgID, evt, entityID, terminatingRelationFor(tc.eventType), taskID)
			if err != nil {
				t.Fatalf("causingRun: %v", err)
			}
			if want := map[bool]string{true: runID, false: ""}[tc.wantCausedByRun]; got != want {
				t.Errorf("causingRun = %q, want %q", got, want)
			}
		})
	}
}

// TestCloseDeferral_CancelRequestedRunGetsNoDeferral pins the stopped check on
// the run itself: a run someone called off is stopped while its conversation
// still shows it working, so the pass closes its task now.
func TestCloseDeferral_CancelRequestedRunGetsNoDeferral(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	r.spawner = &stoppingSpawner{}

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#called-off")
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	if _, err := database.Exec(`UPDATE blueprint_runs SET cancel_requested = 1 WHERE id = ?`, runID); err != nil {
		t.Fatalf("call the run off: %v", err)
	}
	recordAgentPush(t, database, convID, pushedSHA)
	seedPRCheckRuns(t, database, entityID, domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"})

	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}

	if status, _, _ := taskRow(t, database, taskID); status != "done" {
		t.Errorf("task status = %q, want done: a called-off run gets no deferral", status)
	}
	if eventID, _ := taskDeferral(t, database, taskID); eventID != "" {
		t.Errorf("a called-off run's task was deferred on %s", eventID)
	}
}

// TestCloseDeferral_LaterEventFromTheSameRunOverwritesTheDeferral pins the
// overwrite: the run's push turned CI green and its close was held, and then
// the same run merged the pull request. The merge's deferral replaces the
// pass's, stamped in the transaction that closes the entity.
func TestCloseDeferral_LaterEventFromTheSameRunOverwritesTheDeferral(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#pass-then-merge")
	recordAgentAction(t, f.database, f.convID, domain.ExternalAction{Action: domain.ActionPRMerged, EntityID: f.entityID})

	enqueueMerged(t, f.database, f.entityID)
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the merge: %v", err)
	}

	eventID, deferredRun := taskDeferral(t, f.database, f.taskID)
	if eventID == f.eventID || deferredRun != f.runID {
		t.Fatalf("deferral = (%q, %q), want a newer event and run %s", eventID, deferredRun, f.runID)
	}
	var eventType string
	if err := f.database.QueryRow(`SELECT event_type FROM events WHERE id = ?`, eventID).Scan(&eventType); err != nil {
		t.Fatalf("read the deferral's event: %v", err)
	}
	if eventType != domain.EventGitHubPRMerged {
		t.Errorf("deferral names a %s event, want the merge", eventType)
	}
	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the run's task closed under it")
	}
}

// newDeferredMergeFixture is a ci_check_failed task whose running run merged
// its pull request, with the task's close held by that merge and the entity
// closed.
func newDeferredMergeFixture(t *testing.T, sourceID string) deferredCIFixture {
	t.Helper()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, sourceID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	recordAgentAction(t, database, convID, domain.ExternalAction{Action: domain.ActionPRMerged, EntityID: entityID})
	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the merge: %v", err)
	}
	eventID, deferredRun := taskDeferral(t, database, taskID)
	if eventID == "" || deferredRun != runID {
		t.Fatalf("setup: deferral = (%q, %q), want the merge and run %s", eventID, deferredRun, runID)
	}
	return deferredCIFixture{r: r, database: database, sp: sp, entityID: entityID, taskID: taskID, runID: runID, convID: convID, eventID: eventID}
}

// concludeRun marks the run completed with its step conversation parked on
// its verdict, the shape a run that finished cleanly leaves.
func concludeRun(t *testing.T, database *sql.DB, runID, convID string) {
	t.Helper()
	setRunStatus(t, database, runID, "completed")
	if _, err := database.Exec(`UPDATE conversations SET status = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?`, domain.StatusOpen, convID); err != nil {
		t.Fatalf("conclude conversation %s: %v", convID, err)
	}
}

// TestCloseDeferralSweep_CompletedRunAppliesATerminatingClose pins the
// terminating arm of a completed run: the run merged its pull request and
// completed, and its task is still open (a deliverable waiting on a person).
// The entity is closed, so nothing can act on that deliverable any more: the
// task closes with entity_closed and the ordinary teardown.
func TestCloseDeferralSweep_CompletedRunAppliesATerminatingClose(t *testing.T) {
	f := newDeferredMergeFixture(t, "owner/repo#merged-then-completed")
	concludeRun(t, f.database, f.runID, f.convID)

	f.r.sweepCloseDeferrals(t.Context())

	status, reason, eventType := taskRow(t, f.database, f.taskID)
	if status != "done" || reason != "entity_closed" || eventType != domain.EventGitHubPRMerged {
		t.Errorf("task = (%q, %q, %q), want done by the merge's entity_closed", status, reason, eventType)
	}
	if got := f.sp.tornDownCopy(); !slices.Equal(got, []string{f.taskID}) {
		t.Errorf("teardowns = %v, want [%s]", got, f.taskID)
	}
}

// TestCloseDeferralSweep_ReopenedEntityClearsWithoutClosing pins the
// terminating re-check: the run merged and the pull request was reopened
// since, so when the run fails its task is live work again and the held
// close no longer holds.
func TestCloseDeferralSweep_ReopenedEntityClearsWithoutClosing(t *testing.T) {
	f := newDeferredMergeFixture(t, "owner/repo#merged-then-reopened")
	if _, err := f.database.Exec(`UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ?`, f.entityID); err != nil {
		t.Fatalf("reopen entity: %v", err)
	}
	setRunStatus(t, f.database, f.runID, "failed")
	setConversationStatus(t, f.database, f.convID, domain.StatusFailed)

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose pull request was reopened")
	}
	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != "" {
		t.Errorf("deferral still held after its close stopped holding: %s", eventID)
	}
	if len(f.sp.tornDownCopy()) != 0 {
		t.Errorf("teardowns = %v, want none", f.sp.tornDownCopy())
	}
}

// TestCloseDeferralSweep_CompletedRunClearsWithoutClosing pins the sweep's
// hand-off on a live entity: while the run works the deferral waits, and once
// the run completes a task still open is waiting on a person, which still
// works while the pull request is open, so the sweep drops the held CI close
// and closes nothing.
func TestCloseDeferralSweep_CompletedRunClearsWithoutClosing(t *testing.T) {
	f := newDeferredCIFixture(t, "owner/repo#sweep-completed")

	f.r.sweepCloseDeferrals(t.Context())
	if eventID, _ := taskDeferral(t, f.database, f.taskID); eventID != f.eventID {
		t.Fatalf("a working run's deferral changed to %q", eventID)
	}

	concludeRun(t, f.database, f.runID, f.convID)
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
