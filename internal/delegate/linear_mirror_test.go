package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The Linear fixtures' workspace, Linear team and issue.
const (
	linearMirrorWorkspace  = "ws-acme"
	linearMirrorTeamID     = "team-eng"
	linearMirrorIssueUUID  = "uuid-7"
	linearMirrorIdentifier = "ENG-7"
)

// The Linear team's workflow: Todo is pickup, In Progress the in-progress
// bucket, Done the done one.
var (
	linearTodo  = linear.WorkflowState{ID: "st-todo", Name: "Todo", Type: "unstarted"}
	linearDoing = linear.WorkflowState{ID: "st-prog", Name: "In Progress", Type: "started"}
	linearDone  = linear.WorkflowState{ID: "st-done", Name: "Done", Type: "completed"}
)

func linearMirrorStateRef(s linear.WorkflowState) domain.LinearStateRef {
	return domain.LinearStateRef{ID: s.ID, Name: s.Name, Type: s.Type}
}

// linearMirrorRule is the ENG team's rule the mirror acts on.
func linearMirrorRule() domain.LinearTeamRules {
	return domain.LinearTeamRules{
		TeamID:              runmode.LocalDefaultTeamID,
		LinearTeamID:        linearMirrorTeamID,
		LinearTeamKey:       "ENG",
		PickupMembers:       []domain.LinearStateRef{linearMirrorStateRef(linearTodo)},
		InProgressMembers:   []domain.LinearStateRef{linearMirrorStateRef(linearDoing)},
		InProgressCanonical: linearMirrorStateRef(linearDoing),
		DoneMembers:         []domain.LinearStateRef{linearMirrorStateRef(linearDone)},
		DoneCanonical:       linearMirrorStateRef(linearDone),
	}
}

// recordingLinearServer fakes Linear's GraphQL endpoint for one issue. It
// answers the Issue read from the issue's current state and assignee, and
// applies and records every IssueUpdate input, so a later read sees what an
// earlier write changed. Like Linear it answers to the issue's UUID and its
// identifier, and it records which one each request named. Any other document
// fails the test: the mirror reads the issue and moves it, and nothing else.
type recordingLinearServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	identifier string
	state      linear.WorkflowState
	assignee   *linear.User
	updates    []map[string]any // every IssueUpdate input, in arrival order
	addressed  []string         // the issue id every request named
	failRead   bool
	// failTransition refuses every IssueUpdate, as Linear does when the
	// target state no longer exists. A refused update is not recorded.
	failTransition bool

	// transitioned fires after an IssueUpdate is applied; read fires after an
	// Issue read is served. Together they join a detached mirror whether or
	// not it writes.
	transitioned chan struct{}
	read         chan struct{}
}

func newRecordingLinearServer(t *testing.T, state linear.WorkflowState, assignee *linear.User) *recordingLinearServer {
	t.Helper()
	f := &recordingLinearServer{
		t:            t,
		identifier:   linearMirrorIdentifier,
		state:        state,
		assignee:     assignee,
		transitioned: make(chan struct{}, 8),
		read:         make(chan struct{}, 8),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *recordingLinearServer) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(raw, &body)
	w.Header().Set("Content-Type", "application/json")

	f.mu.Lock()
	id, _ := body.Variables["id"].(string)
	f.addressed = append(f.addressed, id)
	if id != linearMirrorIssueUUID && id != f.identifier {
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INVALID_INPUT"}}]}`)
		return
	}
	switch {
	case strings.Contains(body.Query, "mutation IssueUpdate("):
		if f.failTransition {
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found: WorkflowState","extensions":{"code":"INVALID_INPUT"}}]}`)
			return
		}
		input, _ := body.Variables["input"].(map[string]any)
		f.updates = append(f.updates, input)
		if stateID, ok := input["stateId"].(string); ok {
			for _, st := range []linear.WorkflowState{linearTodo, linearDoing, linearDone} {
				if st.ID == stateID {
					f.state = st
				}
			}
		}
		node := f.issueNode()
		f.mu.Unlock()
		writeLinearData(w, map[string]any{"issueUpdate": map[string]any{"success": true, "issue": node}})
		signal(f.transitioned)
	case strings.Contains(body.Query, "query Issue("):
		if f.failRead {
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors":[{"message":"read failed","extensions":{"code":"INVALID_INPUT"}}]}`)
			signal(f.read)
			return
		}
		node := f.issueNode()
		f.mu.Unlock()
		writeLinearData(w, map[string]any{"issue": node})
		signal(f.read)
	default:
		f.mu.Unlock()
		f.t.Errorf("fake linear: unexpected document %q", body.Query)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"unexpected document"}]}`)
	}
}

// issueNode renders the issue the way the IssueFields fragment selects it.
// The caller holds f.mu.
func (f *recordingLinearServer) issueNode() map[string]any {
	emptyPage := map[string]any{"hasNextPage": false, "endCursor": ""}
	var assignee any
	if f.assignee != nil {
		assignee = f.assignee
	}
	return map[string]any{
		"id":            linearMirrorIssueUUID,
		"identifier":    f.identifier,
		"title":         "Fix the thing",
		"description":   "",
		"url":           "https://linear.app/acme/issue/" + f.identifier,
		"priority":      0,
		"priorityLabel": "No priority",
		"createdAt":     "2026-10-01T00:00:00.000Z",
		"updatedAt":     "2026-10-01T00:00:00.000Z",
		"state":         f.state,
		"assignee":      assignee,
		"team":          map[string]any{"id": linearMirrorTeamID, "key": "ENG", "name": "Engineering"},
		"labels":        map[string]any{"nodes": []any{}, "pageInfo": emptyPage},
		"comments":      map[string]any{"nodes": []any{}},
		"children":      map[string]any{"nodes": []any{}, "pageInfo": emptyPage},
	}
}

// signal is a non-blocking send, so a fake never stalls on a join nobody
// waits for.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func writeLinearData(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// client builds an org-credential client pointed at the fake.
func (f *recordingLinearServer) client() *linear.Client {
	cfg := linear.APIKey("lin_org_test")
	cfg.Endpoint = f.srv.URL
	return linear.NewClient(cfg)
}

// snapshot returns every IssueUpdate input the fake applied and the issue's
// current state.
func (f *recordingLinearServer) snapshot() (updates []map[string]any, state linear.WorkflowState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.updates), f.state
}

// transitions renders the state ids the applied updates moved the issue to.
func (f *recordingLinearServer) transitions() []string {
	updates, _ := f.snapshot()
	var out []string
	for _, in := range updates {
		if id, ok := in["stateId"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

// assertNoAssignee fails if any write named an assignee or a delegate.
func (f *recordingLinearServer) assertNoAssignee(t *testing.T) {
	t.Helper()
	updates, _ := f.snapshot()
	for _, in := range updates {
		for _, field := range []string{"assigneeId", "delegateId"} {
			if _, ok := in[field]; ok {
				t.Errorf("the mirror wrote %s: %v", field, in)
			}
		}
	}
}

func (f *recordingLinearServer) waitTransition(t *testing.T) {
	t.Helper()
	select {
	case <-f.transitioned:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mirror's Linear transition")
	}
}

func (f *recordingLinearServer) waitRead(t *testing.T) {
	t.Helper()
	select {
	case <-f.read:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mirror's Linear issue read")
	}
}

// fakeLinearResolver hands out the fake's client as the org's service
// credential and counts how often it was asked. The mirror never asks for a
// user's client.
type fakeLinearResolver struct {
	client *linear.Client
	sysErr error

	mu             sync.Mutex
	forSystemCalls int
}

func (r *fakeLinearResolver) ForSystem(context.Context, string) (*linear.Client, error) {
	r.mu.Lock()
	r.forSystemCalls++
	r.mu.Unlock()
	if r.sysErr != nil {
		return nil, r.sysErr
	}
	return r.client, nil
}

func (r *fakeLinearResolver) ForUser(context.Context, string, string) (*linear.Client, error) {
	return nil, linear.ErrNoLinearUserCredential
}

func (r *fakeLinearResolver) ResolveSystemCredential(context.Context, string) (linear.SystemCredential, error) {
	return linear.SystemCredential{}, linear.ErrNoLinearSystemCredential
}

func (r *fakeLinearResolver) systemCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forSystemCalls
}

// bindLinearWorkspace binds workspaceID to the org and arms the ENG team's
// rule there for the local team.
func bindLinearWorkspace(t *testing.T, database *sql.DB, workspaceID string) {
	t.Helper()
	stores := sqlitestore.New(database)
	if _, err := stores.Orgs.SetLinearWorkspace(t.Context(), runmode.LocalDefaultOrgID, workspaceID, "acme"); err != nil {
		t.Fatalf("set linear workspace: %v", err)
	}
	if _, err := stores.LinearTeamRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, workspaceID, []domain.LinearTeamRules{linearMirrorRule()}); err != nil {
		t.Fatalf("arm linear team: %v", err)
	}
}

// seedLinearMirrorEntity creates the ENG issue's entity in workspaceID under
// identifier.
func seedLinearMirrorEntity(t *testing.T, database *sql.DB, workspaceID, identifier string) *domain.Entity {
	t.Helper()
	e, _, err := sqlitestore.New(database).Entities.FindOrCreateLinearSystem(t.Context(), runmode.LocalDefaultOrgID, workspaceID,
		identifier, linearMirrorIssueUUID, linearMirrorTeamID, "Fix the thing", "https://linear.app/acme/issue/"+identifier)
	if err != nil {
		t.Fatalf("seed linear entity: %v", err)
	}
	return e
}

// newLinearMirrorSpawner builds a spawner over real stores wired to a fake
// Linear whose issue starts in state with assignee.
func newLinearMirrorSpawner(t *testing.T, database *sql.DB, state linear.WorkflowState, assignee *linear.User) (*Spawner, *recordingLinearServer, *fakeLinearResolver) {
	t.Helper()
	fake := newRecordingLinearServer(t, state, assignee)
	res := &fakeLinearResolver{client: fake.client()}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.SetLinearResolver(res)
	return s, fake, res
}

// linearMirrorActions reads the Linear external_actions rows.
func linearMirrorActions(t *testing.T, database *sql.DB) []domain.ExternalAction {
	t.Helper()
	rows, err := database.Query(`
		SELECT action, target, COALESCE(external_id, ''), COALESCE(entity_id, ''), COALESCE(team_id, ''),
		       scope, COALESCE(url, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), credential
		  FROM external_actions WHERE provider = 'linear'`)
	if err != nil {
		t.Fatalf("read actions: %v", err)
	}
	defer rows.Close()
	var out []domain.ExternalAction
	for rows.Next() {
		var a domain.ExternalAction
		if err := rows.Scan(&a.Action, &a.Target, &a.ExternalID, &a.EntityID, &a.TeamID, &a.Scope, &a.URL, &a.FromState, &a.ToState, &a.Credential); err != nil {
			t.Fatalf("scan action: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read actions: %v", err)
	}
	return out
}

// --- runLinearMirror: the shared synchronous worker ------------------------

// An unassigned issue in Todo moves to the in-progress canonical, and the
// mirror never sets its assignee: the org credential assigns nobody. The move
// is audited under the org's Linear credential.
func TestRunLinearMirror_TransitionsAndNeverAssigns(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearTodo, nil)

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if got := fake.transitions(); !slices.Equal(got, []string{linearDoing.ID}) {
		t.Errorf("transitions = %v, want [%s]", got, linearDoing.ID)
	}
	fake.assertNoAssignee(t)
	if _, state := fake.snapshot(); state.ID != linearDoing.ID {
		t.Errorf("final state = %s, want In Progress", state.Name)
	}

	actions := linearMirrorActions(t, database)
	if len(actions) != 1 {
		t.Fatalf("recorded %d actions, want the one transition: %+v", len(actions), actions)
	}
	want := domain.ExternalAction{
		Action: domain.ActionIssueTransitioned, Target: linearMirrorIdentifier, ExternalID: linearMirrorIssueUUID,
		EntityID: e.ID, TeamID: runmode.LocalDefaultTeamID, Scope: linearMirrorWorkspace,
		URL: "https://linear.app/acme/issue/" + linearMirrorIdentifier, FromState: "Todo", ToState: "In Progress",
		Credential: domain.CredentialLinearOrg,
	}
	if actions[0] != want {
		t.Errorf("action = %+v\nwant     %+v", actions[0], want)
	}
}

// An issue already in an in-progress state is left alone: no write, no audit
// row. This is what lets the mirror coexist with the agent's own moves.
func TestRunLinearMirror_Idempotent_AlreadyInBucket_NoWrites(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearDoing, &linear.User{ID: "someone"})

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if updates, _ := fake.snapshot(); len(updates) != 0 {
		t.Errorf("updates = %v, want none (already in progress)", updates)
	}
	if n := len(linearMirrorActions(t, database)); n != 0 {
		t.Errorf("recorded %d actions, want 0", n)
	}
}

// The completion's re-assert finds the issue the mint-time pass moved and
// makes no further move.
func TestRunLinearMirror_SecondInProgressPass_NoFurtherMove(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearTodo, nil)

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())
	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if got := fake.transitions(); !slices.Equal(got, []string{linearDoing.ID}) {
		t.Errorf("transitions = %v, want exactly one move to In Progress", got)
	}
}

// A disabled mirror (no resolver wired) is a clean no-op.
func TestRunLinearMirror_NoResolver_NoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "")

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if n := len(linearMirrorActions(t, database)); n != 0 {
		t.Errorf("recorded %d actions with no resolver wired, want 0", n)
	}
}

// Forward-only: an issue already in a done state is never moved back to in
// progress, whoever moved it there.
func TestRunLinearMirror_SkipsWhenAlreadyDone(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearDone, nil)

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	updates, state := fake.snapshot()
	if len(updates) != 0 || state.ID != linearDone.ID {
		t.Errorf("updates = %v, state = %s; want no writes and the issue still Done", updates, state.Name)
	}
}

// An issue whose state can't be read is not written: it may already be done,
// and a blind move would drag it back.
func TestRunLinearMirror_SkipsWhenIssueUnreadable(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	s, fake, _ := newLinearMirrorSpawner(t, database, linearDone, nil)
	fake.failRead = true

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if updates, _ := fake.snapshot(); len(updates) != 0 {
		t.Errorf("updates = %v, want none (an unreadable issue is not written)", updates)
	}
}

// The mirror reads the issue's entity when it runs: every Linear request names
// the issue by its UUID, and the audit row records the identifier the entity
// carries then, not the one the caller read off the task. An issue that moved
// in between is transitioned by its UUID and audited under its new identifier.
func TestRunLinearMirror_AddressesTheIssueByUUIDAndRecordsItsCurrentIdentifier(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, "OPS-7")
	s, fake, _ := newLinearMirrorSpawner(t, database, linearTodo, nil)
	fake.identifier = "OPS-7"

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	fake.mu.Lock()
	addressed := slices.Clone(fake.addressed)
	fake.mu.Unlock()
	if len(addressed) == 0 {
		t.Fatal("the mirror made no Linear requests")
	}
	for _, id := range addressed {
		if id != linearMirrorIssueUUID {
			t.Errorf("a request addressed the issue as %q, want its UUID %s", id, linearMirrorIssueUUID)
		}
	}
	actions := linearMirrorActions(t, database)
	if len(actions) != 1 || actions[0].Target != "OPS-7" || actions[0].ExternalID != linearMirrorIssueUUID || actions[0].EntityID != e.ID {
		t.Errorf("actions = %+v, want one transition targeting OPS-7 / %s about %s", actions, linearMirrorIssueUUID, e.ID)
	}
}

// An issue tracked in another Linear workspace is not reachable through the
// org's current client, so the mirror leaves it alone: no client is resolved
// and nothing is recorded.
func TestRunLinearMirror_SkipsAnIssueInAnotherWorkspace(t *testing.T) {
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, "ws-new")
	e := seedLinearMirrorEntity(t, database, "ws-old", linearMirrorIdentifier)
	s, fake, res := newLinearMirrorSpawner(t, database, linearTodo, nil)

	s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

	if n := res.systemCalls(); n != 0 {
		t.Errorf("resolved the org's Linear client %d times for an issue in another workspace, want 0", n)
	}
	if updates, _ := fake.snapshot(); len(updates) != 0 {
		t.Errorf("updates = %v, want none", updates)
	}
	if n := len(linearMirrorActions(t, database)); n != 0 {
		t.Errorf("recorded %d actions, want 0", n)
	}
}

// --- linear.mirror_failures ------------------------------------------------

// linearMirrorFailureCounter backs the mirror's failure counter with a manual
// reader for the test's duration and returns a reader of the local org's
// count for each reason. Reasons that were never counted read as zero.
func linearMirrorFailureCounter(t *testing.T) func() map[string]int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := linearMirrorStats.Swap(newLinearMirrorCounters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { linearMirrorStats.Store(prev) })
	return func() map[string]int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "linear.mirror_failures" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("linear.mirror_failures is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if org, _ := dp.Attributes.Value("org.id"); org.AsString() != runmode.LocalDefaultOrgID {
						t.Errorf("a failure was counted under org %q, want %q", org.AsString(), runmode.LocalDefaultOrgID)
					}
					reason, _ := dp.Attributes.Value("reason")
					out[reason.AsString()] += dp.Value
				}
			}
		}
		return out
	}
}

// Each step that can stop a pass from moving an issue it should have moved is
// counted under its own reason, once per failed pass.
func TestRunLinearMirror_CountsEachFailure(t *testing.T) {
	cases := []struct {
		name   string
		stage  func(f *recordingLinearServer, r *fakeLinearResolver)
		entity func(e *domain.Entity) string // the entity id the pass is handed
		want   string
	}{
		{
			name:   "transition refused",
			stage:  func(f *recordingLinearServer, _ *fakeLinearResolver) { f.failTransition = true },
			entity: func(e *domain.Entity) string { return e.ID },
			want:   linearMirrorTransitionFailed,
		},
		{
			name:   "issue unreadable",
			stage:  func(f *recordingLinearServer, _ *fakeLinearResolver) { f.failRead = true },
			entity: func(e *domain.Entity) string { return e.ID },
			want:   linearMirrorIssueReadFailed,
		},
		{
			name:   "credential resolve failed",
			stage:  func(_ *recordingLinearServer, r *fakeLinearResolver) { r.sysErr = errors.New("vault unreachable") },
			entity: func(e *domain.Entity) string { return e.ID },
			want:   linearMirrorCredentialFailed,
		},
		{
			name:   "entity unreadable",
			stage:  func(*recordingLinearServer, *fakeLinearResolver) {},
			entity: func(*domain.Entity) string { return "no-such-entity" },
			want:   linearMirrorStoreReadFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := linearMirrorFailureCounter(t)
			database := newDelegateTestDB(t)
			bindLinearWorkspace(t, database, linearMirrorWorkspace)
			e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
			s, fake, res := newLinearMirrorSpawner(t, database, linearTodo, nil)
			tc.stage(fake, res)

			s.runLinearMirror(runmode.LocalDefaultOrgID, tc.entity(e), linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

			if got := read(); !maps.Equal(got, map[string]int64{tc.want: 1}) {
				t.Errorf("failures = %v, want one %s", got, tc.want)
			}
			if updates, _ := fake.snapshot(); len(updates) != 0 {
				t.Errorf("updates = %v, want none applied", updates)
			}
			if n := len(linearMirrorActions(t, database)); n != 0 {
				t.Errorf("recorded %d actions for a failed pass, want 0", n)
			}
		})
	}
}

// A pass that moves the issue, or skips it by design, is not a failure. The
// counter is a signal that something needs fixing, so the ordinary states it
// would otherwise drown in are left out.
func TestRunLinearMirror_CountsNoFailureForAMoveOrADeliberateSkip(t *testing.T) {
	cases := []struct {
		name      string
		state     linear.WorkflowState
		workspace string // the entity's workspace; the org's is linearMirrorWorkspace
		sysErr    error
	}{
		{name: "moved", state: linearTodo, workspace: linearMirrorWorkspace},
		{name: "already in progress", state: linearDoing, workspace: linearMirrorWorkspace},
		{name: "already done", state: linearDone, workspace: linearMirrorWorkspace},
		{name: "another workspace", state: linearTodo, workspace: "ws-old"},
		{name: "no linear credential", state: linearTodo, workspace: linearMirrorWorkspace,
			sysErr: fmt.Errorf("%w: org=acme", linear.ErrNoLinearSystemCredential)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := linearMirrorFailureCounter(t)
			database := newDelegateTestDB(t)
			bindLinearWorkspace(t, database, linearMirrorWorkspace)
			e := seedLinearMirrorEntity(t, database, tc.workspace, linearMirrorIdentifier)
			s, _, res := newLinearMirrorSpawner(t, database, tc.state, nil)
			res.sysErr = tc.sysErr

			s.runLinearMirror(runmode.LocalDefaultOrgID, e.ID, linearMirrorIdentifier, runmode.LocalDefaultTeamID, linearMirrorRule())

			if got := read(); len(got) != 0 {
				t.Errorf("failures = %v, want none", got)
			}
		})
	}
}

// --- lookupLinearRuleForTaskSystem -----------------------------------------

// A Linear task stamped with a team resolves the rule of the Linear team its
// entity sits in, whatever its identifier's prefix says; a task from another
// source, one with no team, one whose Linear team has no rule, and a lookup in
// a workspace the rule was not saved under resolve none.
func TestLookupLinearRuleForTaskSystem(t *testing.T) {
	ctx := context.Background()
	database := newDelegateTestDB(t)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	stores := sqlitestore.New(database)
	team := runmode.LocalDefaultTeamID
	lookup := func(workspaceID string, task *domain.Task) *domain.LinearTeamRules {
		return lookupLinearRuleForTaskSystem(ctx, stores.Entities, stores.LinearTeamRules, runmode.LocalDefaultOrgID, workspaceID, task)
	}

	// The identifier no longer spells ENG, as after a team key rename; the
	// entity's Linear team still names the rule.
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, "CORE-7")
	task := &domain.Task{EntityID: e.ID, EntitySource: "linear", EntitySourceID: "CORE-7", TeamID: &team}
	rule := lookup(linearMirrorWorkspace, task)
	if rule == nil || rule.LinearTeamID != linearMirrorTeamID || rule.InProgressCanonical.ID != linearDoing.ID {
		t.Errorf("rule = %+v, want the ENG team's rule", rule)
	}
	if rule := lookup("ws-other", task); rule != nil {
		t.Errorf("rule in another workspace = %+v, want nil", rule)
	}

	other, _, err := stores.Entities.FindOrCreateLinearSystem(ctx, runmode.LocalDefaultOrgID, linearMirrorWorkspace, "OPS-1", "uuid-ops-1", "team-ops", "t", "")
	if err != nil {
		t.Fatalf("seed OPS-1: %v", err)
	}
	for name, task := range map[string]*domain.Task{
		"github task":    {EntitySource: "github", EntitySourceID: "owner/repo#1", TeamID: &team},
		"jira task":      {EntityID: e.ID, EntitySource: "jira", EntitySourceID: "SKY-1", TeamID: &team},
		"no team":        {EntityID: e.ID, EntitySource: "linear", EntitySourceID: "CORE-7"},
		"unruled team":   {EntityID: other.ID, EntitySource: "linear", EntitySourceID: "OPS-1", TeamID: &team},
		"no such entity": {EntityID: "missing", EntitySource: "linear", EntitySourceID: "ENG-9", TeamID: &team},
	} {
		if rule := lookup(linearMirrorWorkspace, task); rule != nil {
			t.Errorf("%s: rule = %+v, want nil", name, rule)
		}
	}
	if rule := lookup("", task); rule != nil {
		t.Errorf("no workspace bound: rule = %+v, want nil", rule)
	}
}

// --- Hook 1: announceTaskPlacement -> in-progress mirror -------------------

// seedLinearConversation is seedJiraConversation's Linear twin: a running
// conversation on a task whose entity is the ENG issue in the fixtures'
// workspace.
func seedLinearConversation(t *testing.T, database *sql.DB, conversationID string) string {
	t.Helper()
	e := seedLinearMirrorEntity(t, database, linearMirrorWorkspace, linearMirrorIdentifier)
	eventID, err := sqlitestore.New(database).Events.Record(t.Context(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventLinearIssueAssigned, EntityID: &e.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, e.ID, domain.EventLinearIssueAssigned, conversationID, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	ensureTestPrompt(t, database, domain.Prompt{ID: "test-prompt", Name: "T", Body: "x", Source: "user"})
	brID := seedConversationBlueprint(t, database, conversationID, task.ID)
	stepIdx := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: conversationID, TaskID: task.ID, PromptID: "test-prompt",
		Status: "running", Model: "claude-sonnet-4-6", SessionID: "sess-" + conversationID,
		WorktreePath:   "/tmp/wt-linear-" + conversationID,
		BlueprintRunID: brID, BlueprintStepIndex: &stepIdx,
	})
	return task.ID
}

// setupLinearMirrorFixture seeds a bot-claimable Linear conversation and task
// with the ENG rule armed, and returns a spawner wired to a fake Linear whose
// issue starts in state, unassigned.
func setupLinearMirrorFixture(t *testing.T, suffix string, state linear.WorkflowState) (*Spawner, *sql.DB, string, string, *recordingLinearServer, *fakeLinearResolver) {
	t.Helper()
	database := newDelegateTestDB(t)
	seedLocalBotAgent(t, database)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	conversationID := "r-linear-" + suffix
	taskID := seedLinearConversation(t, database, conversationID)
	s, fake, res := newLinearMirrorSpawner(t, database, state, nil)
	return s, database, conversationID, taskID, fake, res
}

// A bot delegation against a Linear task moves the issue to the in-progress
// canonical when the run is minted, and assigns nobody.
func TestAnnounceTaskPlacement_LinearTask_MirrorsInProgress(t *testing.T) {
	s, database, _, taskID, fake, res := setupLinearMirrorFixture(t, "ip", linearTodo)
	stampBotClaim(t, database, taskID)

	s.announceTaskPlacement(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("board status = %q, want in_progress", got)
	}
	fake.waitTransition(t)
	if n := res.systemCalls(); n != 1 {
		t.Errorf("ForSystem calls = %d, want 1", n)
	}
	if got := fake.transitions(); !slices.Equal(got, []string{linearDoing.ID}) {
		t.Errorf("transitions = %v, want [%s]", got, linearDoing.ID)
	}
	fake.assertNoAssignee(t)
}

// The announcement's guards hold for a Linear task as for a Jira one: a
// user-claimed, unclaimed or terminal task is not announced, so the bot never
// reaches Linear.
func TestAnnounceTaskPlacement_LinearTaskNotTheBots_NoMirror(t *testing.T) {
	for name, stage := range map[string]func(t *testing.T, database *sql.DB, taskID string){
		"user claimed": stampUserClaim,
		"unclaimed":    func(*testing.T, *sql.DB, string) {},
		"terminal": func(t *testing.T, database *sql.DB, taskID string) {
			stampBotClaim(t, database, taskID)
			if _, err := database.Exec(`UPDATE tasks SET status = 'dismissed' WHERE id = ?`, taskID); err != nil {
				t.Fatalf("dismiss task: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, database, _, taskID, fake, res := setupLinearMirrorFixture(t, strings.ReplaceAll(name, " ", "-"), linearTodo)
			stage(t, database, taskID)

			s.announceTaskPlacement(runmode.LocalDefaultOrgID, taskID)

			if n := res.systemCalls(); n != 0 {
				t.Errorf("ForSystem calls = %d, want 0", n)
			}
			if updates, _ := fake.snapshot(); len(updates) != 0 {
				t.Errorf("updates = %v, want no Linear writes", updates)
			}
		})
	}
}

// A GitHub-backed task resolves no Linear rule, so the mirror no-ops even
// when the Linear resolver is wired.
func TestAnnounceTaskPlacement_GitHubTask_NoLinearMirror(t *testing.T) {
	s, database, _, taskID := setupAdvanceFixture(t, "gh-nolinear")
	stampBotClaim(t, database, taskID)
	bindLinearWorkspace(t, database, linearMirrorWorkspace)
	fake := newRecordingLinearServer(t, linearTodo, nil)
	res := &fakeLinearResolver{client: fake.client()}
	s.SetLinearResolver(res)

	s.announceTaskPlacement(runmode.LocalDefaultOrgID, taskID)

	if got := readTaskStatus(t, database, taskID); got != "in_progress" {
		t.Fatalf("board status = %q, want in_progress", got)
	}
	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (GitHub task: no Linear rule)", n)
	}
}

// --- Hook 2: terminateBlueprint completed -> in-progress mirror ------------

// A clean completion re-asserts the in-progress bucket, never Done: the work
// awaits review and merge. Starting from Todo exercises the catch-up for an
// issue a failed mint-time pass left behind.
func TestTerminateBlueprint_CompletedLinearTask_MirrorsInProgress(t *testing.T) {
	s, database, conversationID, taskID, fake, res := setupLinearMirrorFixture(t, "complete", linearTodo)
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if got := readTaskStatus(t, database, taskID); got != "done" {
		t.Fatalf("task status = %q, want done (the board card still completes)", got)
	}
	fake.waitTransition(t)
	if n := res.systemCalls(); n != 1 {
		t.Errorf("ForSystem calls = %d, want 1", n)
	}
	if got := fake.transitions(); !slices.Equal(got, []string{linearDoing.ID}) {
		t.Errorf("transitions = %v, want [%s] (completion must not move the issue to Done)", got, linearDoing.ID)
	}
}

// A clean completion over an issue already in progress reads it and writes
// nothing.
func TestTerminateBlueprint_CompletedLinearTask_AlreadyInProgress_NoWrite(t *testing.T) {
	s, database, conversationID, taskID, fake, _ := setupLinearMirrorFixture(t, "complete-noop", linearDoing)
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	// Join the detached mirror on its read before asserting; it makes no
	// write, so there is no transition to wait on.
	fake.waitRead(t)
	updates, state := fake.snapshot()
	if len(updates) != 0 || state.ID != linearDoing.ID {
		t.Errorf("updates = %v, state = %s; want no writes and the issue still In Progress", updates, state.Name)
	}
}

// A failed or aborted blueprint does not move the issue: the mirror is gated
// to the completed branch.
func TestTerminateBlueprint_AbortedLinearTask_NoMirror(t *testing.T) {
	s, database, conversationID, taskID, fake, res := setupLinearMirrorFixture(t, "abort", linearTodo)
	stampBotClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusAborted, "needs a human", nil, true)

	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 on abort", n)
	}
	if updates, _ := fake.snapshot(); len(updates) != 0 {
		t.Errorf("updates = %v, want none on abort", updates)
	}
}

// A user takeover before a clean completion leaves the issue to the user: the
// completion hook re-reads the claim and stands down.
func TestTerminateBlueprint_CompletedLinearTaskButUserTookOver_NoMirror(t *testing.T) {
	s, database, conversationID, taskID, fake, res := setupLinearMirrorFixture(t, "takeover", linearTodo)
	stampUserClaim(t, database, taskID)
	bpr := blueprintRunIDForConversation(t, database, conversationID)

	s.terminateBlueprint(runmode.LocalDefaultOrgID, bpr, taskID, "event", "",
		time.Now(), runConfig{orgID: runmode.LocalDefaultOrgID}, domain.BlueprintRunStatusCompleted, "", nil, true)

	if n := res.systemCalls(); n != 0 {
		t.Errorf("ForSystem calls = %d, want 0 (user takeover)", n)
	}
	if updates, _ := fake.snapshot(); len(updates) != 0 {
		t.Errorf("updates = %v, want none after takeover", updates)
	}
}
