package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// recordingLinearResolver is a test double for linear.Resolver that records
// the (orgID, userID) ForUser was routed with and every reach for the org's
// own credential. It pins the write-actor contract: a claim on a Linear issue
// resolves the ACTING user's credential, and the handler surfaces
// ErrNoLinearUserCredential rather than falling back to the org's identity.
type recordingLinearResolver struct {
	userClient *linear.Client
	userErr    error

	mu             sync.Mutex
	forUserCalls   int
	forSystemCalls int
	gotOrgID       string
	gotUserID      string
}

func (r *recordingLinearResolver) ForSystem(context.Context, string) (*linear.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forSystemCalls++
	return nil, linear.ErrNoLinearSystemCredential
}

func (r *recordingLinearResolver) ResolveSystemCredential(context.Context, string) (linear.SystemCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forSystemCalls++
	return linear.SystemCredential{}, linear.ErrNoLinearSystemCredential
}

func (r *recordingLinearResolver) ForUser(_ context.Context, orgID, userID string) (*linear.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forUserCalls++
	r.gotOrgID, r.gotUserID = orgID, userID
	return r.userClient, r.userErr
}

func (r *recordingLinearResolver) snapshot() (userCalls, systemCalls int, orgID, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forUserCalls, r.forSystemCalls, r.gotOrgID, r.gotUserID
}

// linearWrite is one issueUpdate the fake received: "assign" with the
// assignee id, "unassign", or "transition" with the state id.
type linearWrite struct {
	kind  string
	value string
}

// linearIssueFake stands in for Linear's GraphQL endpoint for one issue: it
// answers the Issue read from its current assignee and state, and applies and
// records every IssueUpdate, so a test sees which writes a sync made, in
// order, and a later read sees what they changed.
type linearIssueFake struct {
	URL string

	mu         sync.Mutex
	issue      linear.Issue
	writes     []linearWrite
	requests   int
	failAssign bool
}

func newLinearIssueFake(t *testing.T, assignee *linear.User, state linear.WorkflowState) *linearIssueFake {
	t.Helper()
	f := &linearIssueFake{issue: linear.Issue{
		ID:         linearIssueUUID,
		Identifier: linearIssueIdentifier,
		Title:      "Fix the thing",
		Assignee:   assignee,
		State:      state,
		Team:       linearFixtureEng,
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// client is a Linear client acting as the user, pointed at the fake.
func (f *linearIssueFake) client() *linear.Client {
	cfg := linear.APIKey("lin_user_test")
	cfg.Endpoint = f.URL
	return linear.NewClient(cfg)
}

func (f *linearIssueFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	w.Header().Set("Content-Type", "application/json")

	id := stringVar(body.Variables, "id")
	if id != f.issue.ID && id != f.issue.Identifier {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INVALID_INPUT"}}]}`)
		return
	}
	switch {
	case strings.Contains(body.Query, "mutation IssueUpdate("):
		input, _ := body.Variables["input"].(map[string]any)
		if assignee, ok := input["assigneeId"]; ok {
			if assignee == nil {
				f.writes = append(f.writes, linearWrite{kind: "unassign"})
				f.issue.Assignee = nil
			} else {
				if f.failAssign {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"errors":[{"message":"assignee is not a member of the team","extensions":{"code":"INVALID_INPUT"}}]}`)
					return
				}
				userID, _ := assignee.(string)
				f.writes = append(f.writes, linearWrite{kind: "assign", value: userID})
				f.issue.Assignee = &linear.User{ID: userID}
			}
		}
		if stateID, ok := input["stateId"].(string); ok {
			f.writes = append(f.writes, linearWrite{kind: "transition", value: stateID})
			for _, st := range linearFixtureStates {
				if st.ID == stateID {
					f.issue.State = st
				}
			}
		}
		writeGraphQLData(w, map[string]any{"issueUpdate": map[string]any{"success": true, "issue": f.issueNode()}})
	case strings.Contains(body.Query, "query Issue("):
		writeGraphQLData(w, map[string]any{"issue": f.issueNode()})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"unexpected document"}]}`)
	}
}

// issueNode renders the issue the way the IssueFields fragment selects it.
// The caller holds f.mu.
func (f *linearIssueFake) issueNode() map[string]any {
	emptyPage := map[string]any{"hasNextPage": false, "endCursor": ""}
	var assignee any
	if f.issue.Assignee != nil {
		assignee = f.issue.Assignee
	}
	return map[string]any{
		"id":            f.issue.ID,
		"identifier":    f.issue.Identifier,
		"title":         f.issue.Title,
		"description":   "",
		"url":           "https://linear.app/test/issue/" + f.issue.Identifier,
		"priority":      0,
		"priorityLabel": "No priority",
		"createdAt":     "2026-10-01T00:00:00.000Z",
		"updatedAt":     "2026-10-01T00:00:00.000Z",
		"state":         f.issue.State,
		"assignee":      assignee,
		"team":          f.issue.Team,
		"labels":        map[string]any{"nodes": []any{}, "pageInfo": emptyPage},
		"comments":      map[string]any{"nodes": []any{}},
		"children":      map[string]any{"nodes": []any{}, "pageInfo": emptyPage},
	}
}

func (f *linearIssueFake) Writes() []linearWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *linearIssueFake) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// waitForWrites blocks until the fake has received n writes, for a sync the
// handler ran on a detached goroutine, and returns them.
func (f *linearIssueFake) waitForWrites(t *testing.T, n int) []linearWrite {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w := f.Writes(); len(w) >= n {
			return w
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Linear received %v after 5s, want %d writes", f.Writes(), n)
	return nil
}

const (
	linearIssueIdentifier = "ENG-7"
	linearMe              = "lin-me"
)

var (
	linearIssueUUID = fixtureUUID("linear-issue-eng-7")

	linearStateBacklogRef = domain.LinearStateRef{ID: linearStateBacklog, Name: "Backlog", Type: "backlog"}
	linearStateTodoRef    = domain.LinearStateRef{ID: linearStateTodo, Name: "Todo", Type: "unstarted"}
	linearStateDoingRef   = domain.LinearStateRef{ID: linearStateDoing, Name: "In Progress", Type: "started"}
	linearStateReviewRef  = domain.LinearStateRef{ID: linearStateReview, Name: "In Review", Type: "started"}

	// linearClaimRule is ENG's armed rule.
	linearClaimRule = domain.LinearTeamRules{
		LinearTeamID:        linearTeamEng,
		LinearTeamKey:       "ENG",
		LinearTeamName:      "Engineering",
		PickupMembers:       []domain.LinearStateRef{linearStateTodoRef, linearStateBacklogRef},
		InProgressMembers:   []domain.LinearStateRef{linearStateDoingRef, linearStateReviewRef},
		InProgressCanonical: linearStateDoingRef,
		DoneMembers:         []domain.LinearStateRef{{ID: linearStateDone, Name: "Done", Type: "completed"}},
		DoneCanonical:       domain.LinearStateRef{ID: linearStateDone, Name: "Done", Type: "completed"},
	}
)

// linearFixtureState is a fixture workflow state by id.
func linearFixtureState(t *testing.T, id string) linear.WorkflowState {
	t.Helper()
	for _, st := range linearFixtureStates {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no fixture state %s", id)
	return linear.WorkflowState{}
}

// newLinearClaimServer is a test server whose org has a Linear workspace
// bound and whose claims resolve the acting user's Linear client through res.
// withRule arms ENG for the local team; the acting user's Linear identity is
// always stored, as the connect flow stores it with the credential.
func newLinearClaimServer(t *testing.T, res *recordingLinearResolver, withRule bool) *Server {
	t.Helper()
	s := newTestServer(t)
	s.linearResolver = res
	bindLinearWorkspaceForTest(t, s, linearTestWorkspaceID)
	ctx := t.Context()
	if err := s.users.UpsertLinearIdentity(ctx, runmode.LocalDefaultUserID, linearTestWorkspaceID, linearMe, "Me", "api_key"); err != nil {
		t.Fatalf("seed linear identity: %v", err)
	}
	if withRule {
		if _, err := s.linearRules.ReplaceForTeam(ctx, runmode.LocalDefaultTeamID, linearTestWorkspaceID, []domain.LinearTeamRules{linearClaimRule}); err != nil {
			t.Fatalf("seed linear rule: %v", err)
		}
	}
	return s
}

// seedQueuedLinearTask installs the FK chain for an unclaimed, queued task on
// ENG-7, whose snapshot puts it in ENG, unassigned, in state.
func seedQueuedLinearTask(t *testing.T, db *sql.DB, taskID string, state domain.LinearStateRef) {
	t.Helper()
	snap, err := json.Marshal(domain.LinearSnapshot{
		ID: linearIssueUUID, Identifier: linearIssueIdentifier, TeamID: linearTeamEng, TeamKey: "ENG",
		State: state,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	entityID := "e_" + taskID
	if _, err := db.Exec(
		`INSERT INTO entities (id, source, source_id, external_id, kind, state, scope, snapshot_json, linear_team_id)
		 VALUES (?, 'linear', ?, ?, 'issue', 'active', ?, ?, ?)`,
		entityID, linearIssueIdentifier, linearIssueUUID, linearTestWorkspaceID, string(snap), linearTeamEng,
	); err != nil {
		t.Fatalf("seed linear entity: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, '')`,
		"ev_"+taskID, entityID, domain.EventLinearIssueAvailable,
	); err != nil {
		t.Fatalf("seed linear event: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status) VALUES (?, ?, ?, ?, 'queued')`,
		taskID, entityID, domain.EventLinearIssueAvailable, "ev_"+taskID,
	); err != nil {
		t.Fatalf("seed linear task: %v", err)
	}
}

func claimedBy(t *testing.T, db *sql.DB, taskID string) string {
	t.Helper()
	var claim sql.NullString
	if err := db.QueryRow(`SELECT claimed_by_user_id FROM tasks WHERE id = ?`, taskID).Scan(&claim); err != nil {
		t.Fatalf("scan task: %v", err)
	}
	return claim.String
}

func swipeActions(t *testing.T, db *sql.DB, taskID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action FROM swipe_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("query swipe_events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan swipe_events: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// TestTaskClaim_LinearTask_NoUserCredential_Returns409: a user with no Linear
// credential of their own is refused before the claim lands, and the org's
// credential is never reached for in its place. The SPA's gate is onboarding;
// this refusal is what holds for a headless caller.
func TestTaskClaim_LinearTask_NoUserCredential_Returns409(t *testing.T) {
	res := &recordingLinearResolver{userErr: linear.ErrNoLinearUserCredential}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000300"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Errors) != 1 {
		t.Fatalf("body = %s, want one error item (%v)", rec.Body.String(), err)
	}
	if body.Errors[0].Reason != httpx.ReasonNotConfigured || !strings.Contains(body.Errors[0].Message, "connect your Linear") {
		t.Errorf("error = %+v, want NOT_CONFIGURED asking the user to connect their Linear", body.Errors[0])
	}

	if got := claimedBy(t, s.db, taskID); got != "" {
		t.Errorf("claimed_by_user_id = %q, want unclaimed (the refusal must not mutate)", got)
	}
	if got := swipeActions(t, s.db, taskID); len(got) != 0 {
		t.Errorf("swipe_events = %v, want none (a refused claim leaves no audit)", got)
	}
	if _, system, _, _ := res.snapshot(); system != 0 {
		t.Errorf("the org's credential was reached for %d times, want never", system)
	}
}

// TestTaskClaim_LinearTask_ResolverBackendError_Returns500: any other failure
// resolving the user's client is the server's fault, and the claim does not
// land.
func TestTaskClaim_LinearTask_ResolverBackendError_Returns500(t *testing.T) {
	res := &recordingLinearResolver{userErr: errors.New("vault unreachable")}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000301"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := claimedBy(t, s.db, taskID); got != "" {
		t.Errorf("claimed_by_user_id = %q, want unclaimed", got)
	}
}

// TestTaskClaim_LinearTask_AssignsAsTheActingUser: a claim resolves the acting
// user's client exactly once — never the org's — lands and audits the claim in
// TF as any claim does, and assigns the issue to the user's own Linear id and
// moves it to the team's in-progress canonical through that client.
func TestTaskClaim_LinearTask_AssignsAsTheActingUser(t *testing.T) {
	fake := newLinearIssueFake(t, &linear.User{ID: "lin-someone-else"}, linearFixtureState(t, linearStateTodo))
	res := &recordingLinearResolver{userClient: fake.client()}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000302"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	userCalls, systemCalls, gotOrg, gotUser := res.snapshot()
	if userCalls != 1 {
		t.Errorf("ForUser called %d times, want 1", userCalls)
	}
	if gotOrg != runmode.LocalDefaultOrgID || gotUser != runmode.LocalDefaultUserID {
		t.Errorf("ForUser routed (org=%q user=%q), want (org=%q user=%q)",
			gotOrg, gotUser, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID)
	}
	if systemCalls != 0 {
		t.Errorf("the org's credential was reached for %d times, want never", systemCalls)
	}

	if got := claimedBy(t, s.db, taskID); got != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %q, want the acting user", got)
	}
	if got := swipeActions(t, s.db, taskID); !slices.Equal(got, []string{swipeActionClaim}) {
		t.Errorf("swipe_events = %v, want one claim", got)
	}

	want := []linearWrite{{kind: "assign", value: linearMe}, {kind: "transition", value: linearStateDoing}}
	if got := fake.waitForWrites(t, len(want)); !slices.Equal(got, want) {
		t.Errorf("Linear writes = %v, want %v", got, want)
	}
}

// TestTaskClaim_LinearTask_ClearedSnapshotStillFindsTheRule: the rule is found
// through the entity's Linear team column, so an issue whose snapshot a source
// pause cleared still moves to its team's in-progress state on a claim.
func TestTaskClaim_LinearTask_ClearedSnapshotStillFindsTheRule(t *testing.T) {
	fake := newLinearIssueFake(t, &linear.User{ID: linearMe}, linearFixtureState(t, linearStateTodo))
	res := &recordingLinearResolver{userClient: fake.client()}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000306"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)
	if _, err := s.db.Exec(`UPDATE entities SET snapshot_json = NULL WHERE id = ?`, "e_"+taskID); err != nil {
		t.Fatalf("clear snapshot: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := []linearWrite{{kind: "transition", value: linearStateDoing}}
	if got := fake.waitForWrites(t, len(want)); !slices.Equal(got, want) {
		t.Errorf("Linear writes = %v, want %v", got, want)
	}
}

// TestTaskClaim_LinearTask_NoRule_NoLinearWrites: a Linear team the task's
// team has not armed has no in-progress state to move the issue to, so the
// claim lands in TF and Linear is asked nothing.
func TestTaskClaim_LinearTask_NoRule_NoLinearWrites(t *testing.T) {
	fake := newLinearIssueFake(t, nil, linearFixtureState(t, linearStateTodo))
	res := &recordingLinearResolver{userClient: fake.client()}
	s := newLinearClaimServer(t, res, false)
	const taskID = "00000000-0000-4000-8000-000000000303"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := claimedBy(t, s.db, taskID); got != runmode.LocalDefaultUserID {
		t.Errorf("claimed_by_user_id = %q, want the acting user", got)
	}
	// With no rule the sync returns before it starts its goroutine, so by the
	// time the response is written nothing can still reach Linear.
	if n := fake.Requests(); n != 0 {
		t.Errorf("Linear received %d requests, want none", n)
	}
}

// TestTaskClaim_GitHubTask_SkipsLinearResolver: only a Linear-backed claim
// consults the Linear resolver, so a user without Linear can still claim
// everything else.
func TestTaskClaim_GitHubTask_SkipsLinearResolver(t *testing.T) {
	res := &recordingLinearResolver{userErr: linear.ErrNoLinearUserCredential}
	s := newTestServer(t)
	s.linearResolver = res
	const eventType = "github:pr:opened"
	repoID := dbseed.GitHubRepository(t, s.db, runmode.LocalDefaultOrgID, "https://github.com", "sky", "repo")
	if _, err := s.db.Exec(
		`INSERT INTO entities (id, source, source_id, kind, state, scope, repository_id)
		 VALUES ('e_gh_lin', 'github', 'sky/repo#3', 'pr', 'active', 'https://github.com', ?)`, repoID,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES ('ev_gh_lin', 'e_gh_lin', ?, '')`, eventType,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		 VALUES ('00000000-0000-4000-8000-000000000304', 'e_gh_lin', ?, 'ev_gh_lin', 'queued')`, eventType,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	rec := doJSON(t, s, http.MethodPost, "/api/tasks/00000000-0000-4000-8000-000000000304/claim", map[string]any{"hesitation_ms": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if userCalls, _, _, _ := res.snapshot(); userCalls != 0 {
		t.Errorf("ForUser called %d times for a GitHub task, want 0", userCalls)
	}
}

// TestApplyLinearClaim: the claim writes only what the issue's state calls
// for, and never transitions after a failed assign.
func TestApplyLinearClaim(t *testing.T) {
	cases := []struct {
		name       string
		assignee   *linear.User
		state      string
		failAssign bool
		want       []linearWrite
	}{
		{
			name:     "already assigned to self and in progress",
			assignee: &linear.User{ID: linearMe},
			// A member other than the canonical: in progress is membership.
			state: linearStateReview,
			want:  nil,
		},
		{
			name:     "assigned elsewhere",
			assignee: &linear.User{ID: "lin-someone-else"},
			state:    linearStateTodo,
			want:     []linearWrite{{kind: "assign", value: linearMe}, {kind: "transition", value: linearStateDoing}},
		},
		{
			name:  "in progress but unassigned",
			state: linearStateDoing,
			want:  []linearWrite{{kind: "assign", value: linearMe}},
		},
		{
			name:     "assigned to self but not in progress",
			assignee: &linear.User{ID: linearMe},
			state:    linearStateTodo,
			want:     []linearWrite{{kind: "transition", value: linearStateDoing}},
		},
		{
			name:       "assign refused",
			state:      linearStateTodo,
			failAssign: true,
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newLinearIssueFake(t, tc.assignee, linearFixtureState(t, tc.state))
			fake.failAssign = tc.failAssign
			applyLinearClaim(t.Context(), fake.client(), linearIssueIdentifier, linearMe, linearClaimRule)
			if got := fake.Writes(); !slices.Equal(got, tc.want) {
				t.Errorf("writes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyLinearUnclaim: a requeue reverses only what the claim changed,
// judged against the state the claim recorded, and only while the issue
// still shows the claim.
func TestApplyLinearUnclaim(t *testing.T) {
	cases := []struct {
		name     string
		prior    domain.SourceStatus
		assignee *linear.User // the issue's assignee now
		state    string       // its state now
		want     []linearWrite
	}{
		{
			name:     "claimed unassigned from Backlog",
			prior:    domain.SourceStatus{StatusID: linearStateBacklog, StatusName: "Backlog"},
			assignee: &linear.User{ID: linearMe},
			state:    linearStateDoing,
			want:     []linearWrite{{kind: "unassign"}, {kind: "transition", value: linearStateBacklog}},
		},
		{
			name:     "already mine and in progress",
			prior:    domain.SourceStatus{StatusID: linearStateReview, StatusName: "In Review", AssigneeID: linearMe},
			assignee: &linear.User{ID: linearMe},
			state:    linearStateReview,
			want:     nil,
		},
		{
			name:     "already mine in Todo",
			prior:    domain.SourceStatus{StatusID: linearStateTodo, StatusName: "Todo", AssigneeID: linearMe},
			assignee: &linear.User{ID: linearMe},
			state:    linearStateDoing,
			want:     []linearWrite{{kind: "transition", value: linearStateTodo}},
		},
		{
			name:     "unassigned but already in progress",
			prior:    domain.SourceStatus{StatusID: linearStateDoing, StatusName: "In Progress"},
			assignee: &linear.User{ID: linearMe},
			state:    linearStateDoing,
			want:     []linearWrite{{kind: "unassign"}},
		},
		{
			name:     "reassigned to someone else since",
			prior:    domain.SourceStatus{StatusID: linearStateTodo, StatusName: "Todo"},
			assignee: &linear.User{ID: "lin-someone-else"},
			state:    linearStateDoing,
			want:     nil,
		},
		{
			name:  "unassigned since",
			prior: domain.SourceStatus{StatusID: linearStateTodo, StatusName: "Todo"},
			state: linearStateDoing,
			want:  nil,
		},
		{
			name:     "moved on out of in progress since",
			prior:    domain.SourceStatus{StatusID: linearStateTodo, StatusName: "Todo"},
			assignee: &linear.User{ID: linearMe},
			state:    linearStateDone,
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newLinearIssueFake(t, tc.assignee, linearFixtureState(t, tc.state))
			applyLinearUnclaim(t.Context(), fake.client(), linearIssueIdentifier, linearMe, tc.prior, linearClaimRule.InProgressMembers)
			if got := fake.Writes(); !slices.Equal(got, tc.want) {
				t.Errorf("writes = %v, want %v", got, tc.want)
			}
		})
	}

	// A record with no state id could unassign the issue but never move it
	// back, so it asks Linear nothing at all, not even for the issue.
	t.Run("recorded state has no id", func(t *testing.T) {
		fake := newLinearIssueFake(t, &linear.User{ID: linearMe}, linearFixtureState(t, linearStateDoing))
		applyLinearUnclaim(t.Context(), fake.client(), linearIssueIdentifier, linearMe,
			domain.SourceStatus{StatusName: "Todo"}, linearClaimRule.InProgressMembers)
		if n := fake.Requests(); n != 0 {
			t.Errorf("Linear received %d requests, want none", n)
		}
	})
}

// TestTaskRequeue_LinearTask_UndoesTheClaim: claiming a Linear task records
// the issue's state, and returning the task to the queue puts the issue back
// in that state, unassigned, as the user who claimed it. Backlog is not the
// rule's first pickup state, so the issue landing there shows the recorded
// state was read.
func TestTaskRequeue_LinearTask_UndoesTheClaim(t *testing.T) {
	fake := newLinearIssueFake(t, nil, linearFixtureState(t, linearStateBacklog))
	res := &recordingLinearResolver{userClient: fake.client()}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000305"
	seedQueuedLinearTask(t, s.db, taskID, linearStateBacklogRef)

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", map[string]any{"hesitation_ms": 0}); rec.Code != http.StatusOK {
		t.Fatalf("claim status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	fake.waitForWrites(t, 2)

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("requeue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := []linearWrite{
		{kind: "assign", value: linearMe}, {kind: "transition", value: linearStateDoing},
		{kind: "unassign"}, {kind: "transition", value: linearStateBacklog},
	}
	if got := fake.waitForWrites(t, len(want)); !slices.Equal(got, want) {
		t.Errorf("Linear writes = %v, want %v", got, want)
	}
	if userCalls, systemCalls, _, gotUser := res.snapshot(); userCalls != 2 || systemCalls != 0 || gotUser != runmode.LocalDefaultUserID {
		t.Errorf("resolver = (ForUser %d as %q, system %d), want ForUser twice as the acting user and the org's credential never",
			userCalls, gotUser, systemCalls)
	}
}

// TestTaskRequeue_LinearTask_NothingRecorded: a task no person claimed has no
// recorded state to put back, so its requeue asks Linear nothing, not even
// for the user's client.
func TestTaskRequeue_LinearTask_NothingRecorded(t *testing.T) {
	fake := newLinearIssueFake(t, &linear.User{ID: linearMe}, linearFixtureState(t, linearStateDoing))
	res := &recordingLinearResolver{userClient: fake.client()}
	s := newLinearClaimServer(t, res, true)
	const taskID = "00000000-0000-4000-8000-000000000306"
	seedQueuedLinearTask(t, s.db, taskID, linearStateTodoRef)

	if rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/requeue", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("requeue status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if userCalls, _, _, _ := res.snapshot(); userCalls != 0 {
		t.Errorf("ForUser called %d times, want 0", userCalls)
	}
	if n := fake.Requests(); n != 0 {
		t.Errorf("Linear received %d requests, want none", n)
	}
}
