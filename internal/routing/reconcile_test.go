package routing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// The reconciliation sweep is the half of the close story the queue cannot
// reach: an entity whose stored snapshot is terminal but whose row never
// flipped, with NO queue row left to retry — the transition was diffed but
// never published, or its row parked after five attempts, or a replaced pod
// took it to the grave. These tests seed that divergence directly, because
// that is the only honest way to model "there is nothing to replay."

// newReconcilerRouter builds a router wired with the Jira status-rules store,
// which the sweep needs to know which Jira statuses count as done.
func newReconcilerRouter(t *testing.T, database *sql.DB) *Router {
	t.Helper()
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database),
		nil, nil, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, st.JiraStatusRules, nil, nil, noopScorer{}, websocket.NewHub())
}

// seedDivergentEntity is the state this sweep exists to repair: an entity
// whose stored snapshot says the work finished, whose row still says 'active',
// and whose tasks are still sitting in the team's queue. It writes the rows
// directly — going through the router would close them, which is the very
// thing that failed to happen in every scenario this covers.
func seedDivergentEntity(t *testing.T, database *sql.DB, source, sourceID, snapshotJSON string, taskTypes ...string) (entityID string, taskIDs []string) {
	t.Helper()
	ctx := t.Context()
	st := sqlitestore.New(database)
	kind := "pr"
	if source == "jira" {
		kind = "issue"
	}
	entity, _, err := st.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, source, sourceID, kind, sourceID, "")
	if err != nil {
		t.Fatalf("create entity %s: %v", sourceID, err)
	}
	if err := st.Entities.UpdateSnapshot(ctx, runmode.LocalDefaultOrgID, entity.ID, snapshotJSON); err != nil {
		t.Fatalf("seed snapshot for %s: %v", sourceID, err)
	}
	for _, eventType := range taskTypes {
		evtID, err := st.Events.RecordSystem(ctx, runmode.LocalDefaultOrgID, domain.Event{
			OrgID: runmode.LocalDefaultOrgID, EntityID: &entity.ID, EventType: eventType, MetadataJSON: "{}",
		})
		if err != nil {
			t.Fatalf("record event %s: %v", eventType, err)
		}
		task, _, err := testTaskStore(database).FindOrCreateAtSystem(ctx, runmode.LocalDefaultOrgID,
			runmode.LocalDefaultTeamID, entity.ID, eventType, "", evtID, 0.5, time.Now())
		if err != nil {
			t.Fatalf("create task %s: %v", eventType, err)
		}
		taskIDs = append(taskIDs, task.ID)
	}
	return entity.ID, taskIDs
}

func taskCloseReason(t *testing.T, database *sql.DB, taskID string) (status, reason string) {
	t.Helper()
	if err := database.QueryRow(`SELECT status, COALESCE(close_reason, '') FROM tasks WHERE id = ?`, taskID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return status, reason
}

func entityClosedAt(t *testing.T, database *sql.DB, entityID string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	if err := database.QueryRow(`SELECT closed_at FROM entities WHERE id = ?`, entityID).Scan(&at); err != nil {
		t.Fatalf("read entity %s: %v", entityID, err)
	}
	return at
}

// seedJiraDoneRules configures a team's per-project done statuses — the
// vocabulary that decides whether a Jira snapshot reads terminal.
func seedJiraDoneRules(t *testing.T, database *sql.DB, teamID string, rules ...domain.JiraProjectStatusRules) {
	t.Helper()
	if err := sqlitestore.New(database).JiraStatusRules.ReplaceForTeam(t.Context(), teamID, rules); err != nil {
		t.Fatalf("seed jira status rules: %v", err)
	}
}

// TestReconcile_TerminalSnapshotActiveEntity_ClosesEntityAndTasks is the
// invariant this sweep enforces: a stored snapshot that reads terminal means
// the entity is closed and its tasks are closed, no matter how the divergence
// arose. There is no queue row here at all — that is the point.
func TestReconcile_TerminalSnapshotActiveEntity_ClosesEntityAndTasks(t *testing.T) {
	database := newTestDB(t)
	r := newReconcilerRouter(t, database)

	entityID, taskIDs := seedDivergentEntity(t, database, "github", "owner/repo#stranded",
		`{"state":"MERGED","merged":true}`,
		domain.EventGitHubPRCICheckFailed, domain.EventGitHubPRReviewChangesRequested)

	r.reconcileOrgTerminalEntities(context.Background(), runmode.LocalDefaultOrgID)

	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state = %q, want closed — a merged PR must not stay in the refresh set forever", got)
	}
	for _, id := range taskIDs {
		status, reason := taskCloseReason(t, database, id)
		if status != "done" {
			t.Errorf("task %s status = %q, want done — a card for shipped work has nothing left to clear it", id, status)
		}
		if reason != closeReasonReconciled {
			t.Errorf("task %s close_reason = %q, want %q — a sweep close names no event, and the audit trail should say so",
				id, reason, closeReasonReconciled)
		}
	}
	if n := closeAuditCount(t, database, taskIDs[0]); n != 0 {
		t.Errorf("close-audit rows = %d, want 0 — there is no closing event to record", n)
	}
}

// TestReconcile_SecondPassIsANoOp pins that the sweep converges. A repaired
// entity is no longer a candidate, so a re-run neither re-closes anything nor
// re-stamps a closed_at that would make the audit trail lie about when the
// work actually ended.
func TestReconcile_SecondPassIsANoOp(t *testing.T) {
	database := newTestDB(t)
	r := newReconcilerRouter(t, database)
	ctx := context.Background()

	entityID, taskIDs := seedDivergentEntity(t, database, "github", "owner/repo#converge",
		`{"state":"CLOSED","merged":false}`, domain.EventGitHubPRCICheckFailed)

	r.reconcileOrgTerminalEntities(ctx, runmode.LocalDefaultOrgID)
	firstClose := entityClosedAt(t, database, entityID)
	if !firstClose.Valid {
		t.Fatalf("setup: entity was not closed on the first pass")
	}

	r.reconcileOrgTerminalEntities(ctx, runmode.LocalDefaultOrgID)

	if second := entityClosedAt(t, database, entityID); !second.Time.Equal(firstClose.Time) {
		t.Errorf("closed_at moved from %v to %v on a second pass; the entity close must be a guarded transition", firstClose.Time, second.Time)
	}
	if n := closeAuditCount(t, database, taskIDs[0]); n != 0 {
		t.Errorf("close-audit rows = %d, want 0 on a repeated pass", n)
	}
	// The candidate read itself must be empty now — otherwise every pass
	// forever re-reads a repaired entity's tasks.
	rest, err := sqlitestore.New(database).Entities.ListActiveTerminalCandidatesSystem(ctx, runmode.LocalDefaultOrgID, nil, 0)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("candidates after repair = %d, want 0", len(rest))
	}
}

// TestReconcile_LeavesLiveWorkAlone is the negative space, and the one that
// matters most: this sweep writes terminal state from a snapshot, so a false
// positive closes work someone is still doing. An open PR, and a Jira issue
// whose status is "done" only in a DIFFERENT project's vocabulary, must both
// survive.
func TestReconcile_LeavesLiveWorkAlone(t *testing.T) {
	database := newTestDB(t)
	r := newReconcilerRouter(t, database)
	// SKY calls "Shipped" done; OPS does not, and calls "Done" done.
	seedJiraDoneRules(t, database, runmode.LocalDefaultTeamID,
		domain.JiraProjectStatusRules{
			ProjectKey: "SKY", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Shipped"}, DoneCanonical: "Shipped",
		},
		domain.JiraProjectStatusRules{
			ProjectKey: "OPS", PickupMembers: []string{"To Do"},
			InProgressMembers: []string{"In Progress"}, InProgressCanonical: "In Progress",
			DoneMembers: []string{"Done"}, DoneCanonical: "Done",
		},
	)

	openPR, openTasks := seedDivergentEntity(t, database, "github", "owner/repo#live",
		`{"state":"OPEN","merged":false}`, domain.EventGitHubPRCICheckFailed)
	// "Shipped" is done in SKY but not in OPS — the flat union the store
	// filters on surfaces this row, and the per-project recheck is what
	// keeps it open.
	crossProject, crossTasks := seedDivergentEntity(t, database, "jira", "OPS-7",
		`{"key":"OPS-7","status":"Shipped"}`, domain.EventJiraIssueAssigned)
	realDone, doneTasks := seedDivergentEntity(t, database, "jira", "SKY-7",
		`{"key":"SKY-7","status":"Shipped"}`, domain.EventJiraIssueAssigned)

	r.reconcileOrgTerminalEntities(context.Background(), runmode.LocalDefaultOrgID)

	for _, live := range []struct {
		entityID, taskID, why string
	}{
		{openPR, openTasks[0], "an open PR"},
		{crossProject, crossTasks[0], "a Jira issue whose status is done only in another project"},
	} {
		if got := entityState(t, database, live.entityID); got != "active" {
			t.Errorf("%s: entity state = %q, want active", live.why, got)
		}
		if status, _ := taskCloseReason(t, database, live.taskID); status == "done" {
			t.Errorf("%s: its task was closed; the sweep must never close live work", live.why)
		}
	}
	if got := entityState(t, database, realDone); got != "closed" {
		t.Errorf("a SKY issue in SKY's own done status: entity state = %q, want closed", got)
	}
	if status, _ := taskCloseReason(t, database, doneTasks[0]); status != "done" {
		t.Errorf("a SKY issue in SKY's own done status: task status = %q, want done", status)
	}
}

// TestReconcile_TaskCloseFails_LeavesEntityActiveForTheNextPass pins the same
// ordering rule the close phase follows, for the same reason: the candidate
// read finds ACTIVE entities, so flipping the entity while a task close was
// still failing would hide the survivor from the sweep's own next pass.
func TestReconcile_TaskCloseFails_LeavesEntityActiveForTheNextPass(t *testing.T) {
	database := newTestDB(t)
	r := newReconcilerRouter(t, database)
	ctx := context.Background()

	entityID, taskIDs := seedDivergentEntity(t, database, "github", "owner/repo#half-repaired",
		`{"state":"MERGED","merged":true}`,
		domain.EventGitHubPRCICheckFailed, domain.EventGitHubPRReviewChangesRequested)
	doomed := taskIDs[0]
	r.tasks = &taskCloseOutageStore{TaskStore: testTaskStore(database), failTaskID: doomed, o: &outage{remaining: 1}}

	r.reconcileOrgTerminalEntities(ctx, runmode.LocalDefaultOrgID)

	if got := entityState(t, database, entityID); got != "active" {
		t.Errorf("entity state = %q, want active — closing it here would strand the task whose close failed", got)
	}
	if status, _ := taskCloseReason(t, database, taskIDs[1]); status != "done" {
		t.Errorf("sibling task status = %q, want done — one bad row must not stop the others", status)
	}

	r.reconcileOrgTerminalEntities(ctx, runmode.LocalDefaultOrgID)

	if status, _ := taskCloseReason(t, database, doomed); status != "done" {
		t.Errorf("task status after the next pass = %q, want done", status)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Errorf("entity state after the next pass = %q, want closed", got)
	}
}

// TestRunTerminalReconciler_RepairsOnItsOwnTicker covers the loop itself —
// the per-org iteration and the ticker — since a sweep nothing drives is not
// a backstop.
func TestRunTerminalReconciler_RepairsOnItsOwnTicker(t *testing.T) {
	database := newTestDB(t)
	r := newReconcilerRouter(t, database)
	entityID, taskIDs := seedDivergentEntity(t, database, "github", "owner/repo#ticked",
		`{"state":"MERGED","merged":true}`, domain.EventGitHubPRCICheckFailed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.RunTerminalReconciler(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entityState(t, database, entityID) == "closed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := entityState(t, database, entityID); got != "closed" {
		t.Fatalf("entity state = %q after several ticks, want closed", got)
	}
	if status, _ := taskCloseReason(t, database, taskIDs[0]); status != "done" {
		t.Errorf("task status = %q, want done", status)
	}
}
