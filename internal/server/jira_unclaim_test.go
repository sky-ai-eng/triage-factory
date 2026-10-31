package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// jiraMe is the acting user as a Data Center instance names them: the stable
// id (the user key) that both the snapshot's assignee_account_id and the
// user's stored Jira identity carry.
const (
	jiraMeKey  = "JIRAUSER1"
	jiraMeName = "me"
)

// jiraWrite is one write the fake received: "assign" with the user key,
// "unassign", or "transition" with the target status id.
type jiraWrite struct {
	kind  string
	value string
}

// jiraIssueFake stands in for a Data Center instance serving one ticket: the
// claim-state read, /myself, the assignee write and the transitions list and
// write, against a workflow that can move between any two fixture statuses. It
// applies and records every write, so a test sees which writes a claim or a
// requeue made, in order.
type jiraIssueFake struct {
	URL string

	mu       sync.Mutex
	status   fakeJiraStatus
	assignee string // user key, "" when unassigned
	writes   []jiraWrite
	issues   []string // what each /issue/ request named, id or key
	requests int
	// withdrawn is a status the workflow offers no transition into, as when
	// it has left the project's workflow.
	withdrawn string
	// failTransition answers every transition write with a server error.
	failTransition bool
}

func newJiraIssueFake(t *testing.T, statusID, assignee string) *jiraIssueFake {
	t.Helper()
	f := &jiraIssueFake{status: jiraFixtureStatus(t, statusID), assignee: assignee}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func jiraFixtureStatus(t *testing.T, id string) fakeJiraStatus {
	t.Helper()
	for _, st := range jiraFixtureStatuses {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no fixture status %s", id)
	return fakeJiraStatus{}
}

// client is a Jira client acting as jiraMe, pointed at the fake.
func (f *jiraIssueFake) client() *jira.Client {
	return jira.NewClient(jira.DataCenterPAT(f.URL, "user-tok"))
}

func (f *jiraIssueFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	if _, rest, ok := strings.Cut(path, "/issue/"); ok {
		issue, _, _ := strings.Cut(rest, "/")
		f.issues = append(f.issues, issue)
	}
	switch {
	case strings.HasSuffix(path, "/myself"):
		_ = json.NewEncoder(w).Encode(map[string]string{"key": jiraMeKey, "name": jiraMeName})
	case strings.HasSuffix(path, "/assignee") && r.Method == http.MethodPut:
		var body map[string]*string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if name := body["name"]; name == nil {
			f.writes = append(f.writes, jiraWrite{kind: "unassign"})
			f.assignee = ""
		} else {
			f.writes = append(f.writes, jiraWrite{kind: "assign", value: jiraMeKey})
			f.assignee = jiraMeKey
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/transitions") && r.Method == http.MethodGet:
		var transitions []jira.Transition
		for _, st := range jiraFixtureStatuses {
			if st.ID != f.status.ID && st.ID != f.withdrawn {
				transitions = append(transitions, jira.Transition{ID: "t-" + st.ID, Name: st.Name, To: jira.Status{ID: st.ID, Name: st.Name}})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"transitions": transitions})
	case strings.HasSuffix(path, "/transitions") && r.Method == http.MethodPost:
		if f.failTransition {
			http.Error(w, `{"errorMessages":["try again later"]}`, http.StatusInternalServerError)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Transition struct {
				ID string `json:"id"`
			} `json:"transition"`
		}
		_ = json.Unmarshal(raw, &body)
		statusID := strings.TrimPrefix(body.Transition.ID, "t-")
		for _, st := range jiraFixtureStatuses {
			if st.ID == statusID {
				f.status = st
			}
		}
		f.writes = append(f.writes, jiraWrite{kind: "transition", value: statusID})
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(path, "/issue/") && r.Method == http.MethodGet:
		var assignee any
		if f.assignee != "" {
			assignee = map[string]string{"key": f.assignee, "name": f.assignee}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key":    "SKY-5",
			"fields": map[string]any{"status": f.status, "assignee": assignee},
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Requests is how many requests of any kind the fake received.
func (f *jiraIssueFake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *jiraIssueFake) Writes() []jiraWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// assertIssuesNamed fails unless the fake received /issue/ requests and every
// one named one of ids.
func (f *jiraIssueFake) assertIssuesNamed(t *testing.T, ids ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issues) == 0 || slices.ContainsFunc(f.issues, func(issue string) bool { return !slices.Contains(ids, issue) }) {
		t.Errorf("Jira requests named issues %v, want only %v", f.issues, ids)
	}
}

// waitForWrites blocks until the fake has received n writes, for a sync the
// handler ran on a detached goroutine, and returns them.
func (f *jiraIssueFake) waitForWrites(t *testing.T, n int) []jiraWrite {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w := f.Writes(); len(w) >= n {
			return w
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Jira received %v after 5s, want %d writes", f.Writes(), n)
	return nil
}

var jiraUnclaimInProgress = []domain.JiraStatusRef{
	{ID: statusInProgressID, Name: "In Progress"},
	{ID: statusInReviewID, Name: "Code Review"},
}

// TestApplyJiraUnclaim: a requeue reverses only what the claim changed,
// judged against the state the claim recorded, and leaves a ticket someone
// has since moved on alone.
func TestApplyJiraUnclaim(t *testing.T) {
	cases := []struct {
		name     string
		prior    domain.SourceStatus
		status   string // the ticket's status now
		assignee string // its assignee now
		want     []jiraWrite
	}{
		{
			name:     "claimed unassigned from To Do",
			prior:    domain.SourceStatus{StatusID: statusToDoID, StatusName: "To Do"},
			status:   statusInProgressID,
			assignee: jiraMeKey,
			want:     []jiraWrite{{kind: "unassign"}, {kind: "transition", value: statusToDoID}},
		},
		{
			name:     "already mine and in progress",
			prior:    domain.SourceStatus{StatusID: statusInProgressID, StatusName: "In Progress", AssigneeID: jiraMeKey},
			status:   statusInProgressID,
			assignee: jiraMeKey,
			want:     nil,
		},
		{
			name:     "already mine in To Do",
			prior:    domain.SourceStatus{StatusID: statusToDoID, StatusName: "To Do", AssigneeID: jiraMeKey},
			status:   statusInProgressID,
			assignee: jiraMeKey,
			want:     []jiraWrite{{kind: "transition", value: statusToDoID}},
		},
		{
			name:     "unassigned but already in progress",
			prior:    domain.SourceStatus{StatusID: statusInReviewID, StatusName: "Code Review"},
			status:   statusInReviewID,
			assignee: jiraMeKey,
			want:     []jiraWrite{{kind: "unassign"}},
		},
		{
			name:     "reassigned to someone else since",
			prior:    domain.SourceStatus{StatusID: statusToDoID, StatusName: "To Do"},
			status:   statusInProgressID,
			assignee: "JIRAUSER2",
			want:     nil,
		},
		{
			name:     "moved on to done since",
			prior:    domain.SourceStatus{StatusID: statusToDoID, StatusName: "To Do"},
			status:   statusDoneID,
			assignee: jiraMeKey,
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newJiraIssueFake(t, tc.status, tc.assignee)
			applyJiraUnclaim(t.Context(), fake.client(), "10005", "SKY-5", tc.prior, jiraMeKey, jiraUnclaimInProgress)
			if got := fake.Writes(); !slices.Equal(got, tc.want) {
				t.Errorf("writes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaskRequeue_JiraTask_PutsTheTicketBack: claiming a Jira task records
// the ticket's state in the claim, and returning it to the queue puts the
// ticket back in that status, unassigned, as the user who claimed it,
// addressing the issue by its id.
func TestTaskRequeue_JiraTask_PutsTheTicketBack(t *testing.T) {
	fake := newJiraIssueFake(t, statusToDoID, "")
	s := newTestServer(t)
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

	const taskID = "00000000-0000-4000-8000-000000000400"
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, external_id, kind, state, scope, snapshot_json, jira_project_id)
		 VALUES ('e_jira_unclaim', 'jira', 'SKY-5', 'id-SKY-5', 'issue', 'active', 'https://jira.example.com', ?, ?)`,
		`{"key":"SKY-5","project_id":"id-SKY","project_key":"SKY","status":"To Do","status_id":"`+statusToDoID+`","assignee":""}`, project,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_jira_unclaim', 'e_jira_unclaim', ?, '')`,
		domain.EventJiraIssueAvailable,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES (?, 'e_jira_unclaim', ?, 'ev_jira_unclaim', 'queued')`,
		taskID, domain.EventJiraIssueAvailable,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0}); rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	fake.waitForWrites(t, 2)

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("requeue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := []jiraWrite{
		{kind: "assign", value: jiraMeKey}, {kind: "transition", value: statusInProgressID},
		{kind: "unassign"}, {kind: "transition", value: statusToDoID},
	}
	if got := fake.waitForWrites(t, len(want)); !slices.Equal(got, want) {
		t.Errorf("Jira writes = %v, want %v", got, want)
	}
	fake.assertIssuesNamed(t, "id-SKY-5")
}
