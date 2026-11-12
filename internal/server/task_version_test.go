package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// raceAroundWriteTx lands a write by someone else around the request's first
// write transaction, each hook once. *before runs just before it opens: a
// change between the handler's read of the task and the route's own first
// write, which only the version predicate on that write can refuse. *after
// runs just after it commits: a change the route's response must not report
// as its own write's.
type raceAroundWriteTx struct {
	db.TxRunner
	before, after *func()
}

func (w raceAroundWriteTx) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	if race := *w.before; race != nil {
		*w.before = nil
		race()
	}
	err := w.TxRunner.WithTx(ctx, orgID, userID, fn)
	if race := *w.after; err == nil && race != nil {
		*w.after = nil
		race()
	}
	return err
}

// moveTaskOn is another writer's change to the task: one column a person acts
// on that none of the routes' other checks read, written the way every task
// writer writes, so the version rises with it. It flips the column, so every
// call is a change.
func moveTaskOn(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	flip := "CASE WHEN close_event_type IS NULL THEN '" + domain.EventGitHubPRMerged + "' END"
	execSQL(t, database, `UPDATE tasks SET `+db.TaskSetSQL(db.TaskAssign{Col: "close_event_type", Expr: flip})+` WHERE id = ?`, taskID)
}

// taskWorld is everything a task write could change or start, read off the
// rows: the task itself, its conversations and their boundary stamps, its
// deliverables, and its blueprint runs. Two equal readings mean the request
// in between wrote nothing, stopped and ended nothing, retired nothing, and
// spawned nothing.
func taskWorld(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT * FROM tasks WHERE id = ?`,
		`SELECT id, status, ended_at, ended_reason FROM conversations WHERE task_id = ? ORDER BY id`,
		`SELECT d.id, d.state FROM deliverables d JOIN conversations c ON c.id = d.conversation_id WHERE c.task_id = ? ORDER BY d.id`,
		`SELECT id, status, cancel_requested FROM blueprint_runs WHERE task_id = ? ORDER BY id`,
	} {
		rows, err := database.Query(q, taskID)
		if err != nil {
			t.Fatalf("read %q: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			fmt.Fprintf(&b, "%v\n", vals)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("read %q: %v", q, err)
		}
	}
	return b.String()
}

// taskVersionCase is one task write: the state the task needs for the write
// to land, and the request that makes it.
type taskVersionCase struct {
	name   string
	setup  string // run against the fixture's task first; "" leaves it agent-claimed and in progress
	method string
	suffix string
	body   map[string]any
}

func taskVersionCases(otherUserID string) []taskVersionCase {
	const unclaimed = `UPDATE tasks SET claimed_by_agent_id = NULL, status = 'queued' WHERE id = ?`
	const mine = `UPDATE tasks SET claimed_by_agent_id = NULL, claimed_by_user_id = '` + runmode.LocalDefaultUserID + `', status = 'in_progress' WHERE id = ?`
	return []taskVersionCase{
		{name: "patch close", method: http.MethodPatch, body: map[string]any{"status": "dismissed"}},
		{name: "patch snooze", setup: unclaimed, method: http.MethodPatch,
			body: map[string]any{"snooze_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}},
		{name: "patch wake",
			setup:  `UPDATE tasks SET claimed_by_agent_id = NULL, status = 'snoozed', snooze_until = datetime('now', '+1 hour') WHERE id = ?`,
			method: http.MethodPatch, body: map[string]any{"snooze_until": nil}},
		{name: "patch stage", setup: mine, method: http.MethodPatch, body: map[string]any{"status": "in_progress"}},
		{name: "claim from the queue", setup: unclaimed, method: http.MethodPost, suffix: "/claim", body: map[string]any{}},
		{name: "claim from the agent", method: http.MethodPost, suffix: "/claim", body: map[string]any{}},
		{name: "claim it already holds", setup: mine, method: http.MethodPost, suffix: "/claim", body: map[string]any{}},
		{name: "claim's reassign arm", setup: mine, method: http.MethodPost, suffix: "/claim",
			body: map[string]any{"target_user_id": otherUserID}},
		{name: "delegate", setup: unclaimed, method: http.MethodPost, suffix: "/delegate", body: map[string]any{}},
		{name: "re-delegate to the agent that holds it", method: http.MethodPost, suffix: "/delegate", body: map[string]any{}},
		{name: "requeue", method: http.MethodPost, suffix: "/requeue", body: map[string]any{}},
	}
}

func withVersion(body map[string]any, version int64) map[string]any {
	out := map[string]any{"version": version}
	for k, v := range body {
		out[k] = v
	}
	return out
}

func wantVersionRefusal(t *testing.T, rec *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, status, rec.Body.String())
	}
	var env struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode errors: %v; body=%s", err, rec.Body.String())
	}
	if !slices.ContainsFunc(env.Errors, func(e httpx.ErrorItem) bool { return e.Reason == reason && e.Field == "version" }) {
		t.Errorf("errors = %+v, want %s on version", env.Errors, reason)
	}
}

// TestTaskWrites_RequireTheVersionTheCallerRead: every task write route, and
// every arm of PATCH and of the claim route, refuses a body with no version
// (400 MISSING_FIELD), a version the task has moved past by the time the
// handler reads it (409 VERSION_CONFLICT), and a version the task moves past
// between the handler's read and its first write (409 VERSION_CONFLICT, from
// that write's own predicate). Each refusal writes nothing and starts nothing:
// no claim stamped, no run stopped, ended or spawned, no deliverable retired.
// At the version the task holds, the write lands and answers the version it
// left, even when another write lands before the route's response is read.
func TestTaskWrites_RequireTheVersionTheCallerRead(t *testing.T) {
	const otherUserID = "00000000-0000-0000-0000-0000000007aa"
	for _, c := range taskVersionCases(otherUserID) {
		t.Run(c.name, func(t *testing.T) {
			var race, after func()
			s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner {
				return raceAroundWriteTx{TxRunner: tx, before: &race, after: &after}
			})
			// The reassign arm's target, on the task's team, so the handoff
			// lands once the version is right.
			execSQL(t, s.db, `INSERT INTO users (id, display_name) VALUES (?, 'Other User')`, otherUserID)
			execSQL(t, s.db, `INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`,
				otherUserID, runmode.LocalDefaultTeamID)
			taskID, _, _ := pendingApprovalFixture(t, s.db)
			if c.setup != "" {
				execSQL(t, s.db, c.setup, taskID)
			}
			path := "/api/tasks/" + taskID + c.suffix

			before := taskWorld(t, s.db, taskID)
			rec := doJSON(t, s, c.method, path, c.body)
			wantVersionRefusal(t, rec, http.StatusBadRequest, httpx.ReasonMissingField)
			if after := taskWorld(t, s.db, taskID); after != before {
				t.Errorf("a body with no version changed the task\nbefore %s\nafter  %s", before, after)
			}

			read := taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)
			moveTaskOn(t, s.db, taskID)
			before = taskWorld(t, s.db, taskID)
			rec = doJSON(t, s, c.method, path, withVersion(c.body, read))
			wantVersionRefusal(t, rec, http.StatusConflict, httpx.ReasonVersionConflict)
			if after := taskWorld(t, s.db, taskID); after != before {
				t.Errorf("a stale version changed the task\nbefore %s\nafter  %s", before, after)
			}

			read = taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)
			var raced string
			race = func() {
				moveTaskOn(t, s.db, taskID)
				raced = taskWorld(t, s.db, taskID)
			}
			rec = doJSON(t, s, c.method, path, withVersion(c.body, read))
			if race != nil {
				t.Fatal("the route opened no write transaction for the race to land before")
			}
			wantVersionRefusal(t, rec, http.StatusConflict, httpx.ReasonVersionConflict)
			if after := taskWorld(t, s.db, taskID); after != raced {
				t.Errorf("a write that lost the race changed the task\nbefore %s\nafter  %s", raced, after)
			}

			// At the version the task holds, the same request lands. Its answer
			// carries the version its own write left, which the next write
			// names, and not the version of a write landing after it.
			var left int64
			after = func() {
				left = taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)
				moveTaskOn(t, s.db, taskID)
			}
			rec = doJSON(t, s, c.method, path, withVersion(c.body, taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)))
			if rec.Code != http.StatusOK {
				t.Fatalf("at the current version: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if after != nil {
				t.Fatal("the write transaction never committed for the later write to land after")
			}
			var landed struct {
				Version *int64 `json:"version"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &landed); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if landed.Version == nil {
				t.Errorf("response carries no version, want %d, the version the write left", left)
			} else if *landed.Version != left {
				t.Errorf("response version = %d, want %d, the version the write left", *landed.Version, left)
			}
			if now := taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID); now != left+1 {
				t.Errorf("task version = %d, want %d: the later write should have moved it past the write's", now, left+1)
			}
		})
	}
}

// TestTaskReads_CarryTheVersion: the detail read and each row of the list
// carry the version a write names.
func TestTaskReads_CarryTheVersion(t *testing.T) {
	s := newTestServer(t)
	taskID, _, _ := pendingApprovalFixture(t, s.db)
	moveTaskOn(t, s.db, taskID)
	want := taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)
	if want != 2 {
		t.Fatalf("seeded task moved on to version %d, want 2", want)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/tasks/"+taskID, nil)
	var got struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Version != want {
		t.Errorf("GET version = %d (err %v), want %d; body=%s", got.Version, err, want, rec.Body.String())
	}

	rec = doJSON(t, s, http.MethodPost, "/api/tasks/list", map[string]any{"statuses": []string{"in_progress"}})
	var page struct {
		Items []struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	i := slices.IndexFunc(page.Items, func(it struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}) bool {
		return it.ID == taskID
	})
	if i < 0 || page.Items[i].Version != want {
		t.Errorf("list rows = %+v, want task %s at version %d", page.Items, taskID, want)
	}
}

// TestTaskWrites_VersionRefusalAsksJiraNothing: a Jira-backed task's claim
// that loses its version to a write landing after the handler resolved the
// caller's Jira client, and a requeue naming a version the task has moved
// past, leave the ticket alone: no assignment, no transition.
func TestTaskWrites_VersionRefusalAsksJiraNothing(t *testing.T) {
	fake := newJiraIssueFake(t, statusToDoID, "")
	var race, after func()
	s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner {
		return raceAroundWriteTx{TxRunner: tx, before: &race, after: &after}
	})
	s.jiraResolver = &recordingJiraResolver{userClient: fake.client()}
	ctx := t.Context()
	seedSourceHost(t, s, eventsource.KindJira, "https://jira.example.com")
	if err := s.users.UpsertJiraIdentity(ctx, runmode.LocalDefaultUserID, "https://jira.example.com", jiraMeKey, "Me", "pat"); err != nil {
		t.Fatalf("seed jira identity: %v", err)
	}
	project := dbseed.JiraProject(t, s.db, runmode.LocalDefaultOrgID, "https://jira.example.com", "SKY")
	if err := s.jiraRules.ReplaceForTeam(ctx, runmode.LocalDefaultTeamID, "https://jira.example.com", []domain.JiraProjectStatusRules{{
		JiraProjectID:       project,
		PickupMembers:       []domain.JiraStatusRef{{ID: statusToDoID, Name: "To Do"}},
		InProgressMembers:   jiraUnclaimInProgress,
		InProgressCanonical: domain.JiraStatusRef{ID: statusInProgressID, Name: "In Progress"},
		DoneMembers:         []domain.JiraStatusRef{{ID: statusDoneID, Name: "Done"}},
		DoneCanonical:       domain.JiraStatusRef{ID: statusDoneID, Name: "Done"},
	}}); err != nil {
		t.Fatalf("seed jira rule: %v", err)
	}
	const taskID = "00000000-0000-4000-8000-000000000410"
	execSQL(t, s.db,
		`INSERT INTO entities (id, source, source_id, external_id, kind, state, scope, snapshot_json, jira_project_id)
		 VALUES ('e_jira_ver', 'jira', 'SKY-6', 'id-SKY-6', 'issue', 'active', 'https://jira.example.com', ?, ?)`,
		`{"key":"SKY-6","project_id":"id-SKY","project_key":"SKY","status":"To Do","status_id":"`+statusToDoID+`","assignee":""}`, project)
	execSQL(t, s.db, `INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_jira_ver', 'e_jira_ver', ?, '')`,
		domain.EventJiraIssueAvailable)
	execSQL(t, s.db, `INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES (?, 'e_jira_ver', ?, 'ev_jira_ver', 'queued')`,
		taskID, domain.EventJiraIssueAvailable)

	race = func() { moveTaskOn(t, s.db, taskID) }
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, nil))
	wantVersionRefusal(t, rec, http.StatusConflict, httpx.ReasonVersionConflict)

	// A claim at the current version lands and syncs, so the requeue below
	// would have a claim to put back in Jira.
	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, nil)); rec.Code != http.StatusOK {
		t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	claimWrites := fake.waitForWrites(t, 2)

	stale := taskVersionNow(t, s, runmode.LocalDefaultOrgID, taskID)
	moveTaskOn(t, s.db, taskID)
	rec = doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", map[string]any{"version": stale})
	wantVersionRefusal(t, rec, http.StatusConflict, httpx.ReasonVersionConflict)
	if who := claimedBy(t, s.db, taskID); who != runmode.LocalDefaultUserID {
		t.Errorf("claimed by %q after the refused requeue, want the claim kept", who)
	}

	// The writes the one claim that landed made are the only ones there are.
	// A sync the refusals had started would land on its own goroutine, so it
	// is given a moment to.
	time.Sleep(100 * time.Millisecond)
	if got := fake.Writes(); !slices.Equal(got, claimWrites) {
		t.Errorf("Jira writes = %v, want only the landed claim's %v", got, claimWrites)
	}
}
