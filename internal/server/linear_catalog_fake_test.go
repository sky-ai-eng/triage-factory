package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/zalando/go-keyring"
)

// The Linear ids the picker and the write gate are exercised against. Linear
// ids are UUIDs, and the handlers refuse anything else, so the fixtures are
// real ones derived from a readable seed.
var (
	linearTeamEng   = fixtureUUID("linear-team-eng")
	linearTeamOps   = fixtureUUID("linear-team-ops")
	linearTeamGhost = fixtureUUID("linear-team-ghost")

	linearStateTriage   = fixtureUUID("linear-state-triage")
	linearStateBacklog  = fixtureUUID("linear-state-backlog")
	linearStateTodo     = fixtureUUID("linear-state-todo")
	linearStateDoing    = fixtureUUID("linear-state-doing")
	linearStateReview   = fixtureUUID("linear-state-review")
	linearStateDone     = fixtureUUID("linear-state-done")
	linearStateCanceled = fixtureUUID("linear-state-canceled")
	linearStateUnknown  = fixtureUUID("linear-state-unknown")
)

// linearFixtureStates is every fixture team's workflow, deliberately out of
// position order so a read that forgets to sort shows it.
var linearFixtureStates = []linear.WorkflowState{
	{ID: linearStateDone, Name: "Done", Type: "completed", Position: 5},
	{ID: linearStateTriage, Name: "Triage", Type: "triage", Position: 0},
	{ID: linearStateBacklog, Name: "Backlog", Type: "backlog", Position: 1},
	{ID: linearStateTodo, Name: "Todo", Type: "unstarted", Position: 2},
	{ID: linearStateDoing, Name: "In Progress", Type: "started", Position: 3},
	{ID: linearStateReview, Name: "In Review", Type: "started", Position: 4},
	{ID: linearStateCanceled, Name: "Canceled", Type: "canceled", Position: 6},
}

// linearCatalogFake stands in for Linear's GraphQL endpoint, answering the
// documents the catalog routes and the write gate send — Teams (filtered and
// cursor-paged), Team, TeamStates (cursor-paged) and WorkflowState — from one
// in-memory workspace, so a paging, filtering or resolution bug shows up as
// wrong rows rather than as a stub running out of scripted replies. A team
// with no states of its own set has linearFixtureStates.
type linearCatalogFake struct {
	URL string

	mu      sync.Mutex
	teams   []linear.Team
	states  map[string][]linear.WorkflowState
	failing bool
	// teamFailing fails the Team read of these ids only, so one request can
	// carry a field fault and an upstream failure together.
	teamFailing map[string]bool
	calls       int
}

func newLinearCatalogFake(t *testing.T, teams ...linear.Team) *linearCatalogFake {
	t.Helper()
	f := &linearCatalogFake{teams: teams, states: map[string][]linear.WorkflowState{}, teamFailing: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *linearCatalogFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(raw, &body)
	op := operationName(body.Query)

	f.mu.Lock()
	f.calls++
	failing := f.failing || (op == "Team" && f.teamFailing[stringVar(body.Variables, "id")])
	teams := append([]linear.Team(nil), f.teams...)
	statesOf := func(teamID string) []linear.WorkflowState {
		if st, custom := f.states[teamID]; custom {
			return st
		}
		return linearFixtureStates
	}
	workflows := make(map[string][]linear.WorkflowState, len(teams))
	for _, t := range teams {
		workflows[t.ID] = statesOf(t.ID)
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if failing {
		// An auth failure rather than a 5xx: the client retries a read on a
		// 5xx with backoff, and what is under test is what the handler does
		// with a failed upstream, not how the client got there.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`)
		return
	}

	switch op {
	case "Teams":
		q := teamsFilterQuery(body.Variables)
		var matched []linear.Team
		for _, t := range teams {
			if q == "" || strings.Contains(strings.ToLower(t.Key), q) || strings.Contains(strings.ToLower(t.Name), q) {
				matched = append(matched, t)
			}
		}
		writeGraphQLData(w, map[string]any{"teams": fakeConnection(matched, body.Variables)})
	case "Team":
		id := stringVar(body.Variables, "id")
		for _, t := range teams {
			if t.ID == id {
				writeGraphQLData(w, map[string]any{"team": t})
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found: Team","extensions":{"code":"INVALID_INPUT"}}]}`)
	case "TeamStates":
		states, known := workflows[stringVar(body.Variables, "id")]
		if !known {
			writeGraphQLData(w, map[string]any{"team": nil})
			return
		}
		writeGraphQLData(w, map[string]any{"team": map[string]any{"states": fakeConnection(states, body.Variables)}})
	case "WorkflowState":
		id := stringVar(body.Variables, "id")
		for _, t := range teams {
			for _, st := range workflows[t.ID] {
				if st.ID == id {
					writeGraphQLData(w, map[string]any{"workflowState": map[string]any{
						"id": st.ID, "name": st.Name, "type": st.Type, "position": st.Position,
						"team": map[string]any{"id": t.ID},
					}})
					return
				}
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"Entity not found: WorkflowState","extensions":{"code":"INVALID_INPUT"}}]}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":[{"message":"unexpected document `+op+`"}]}`)
	}
}

// fakeConnection is one page of items as a GraphQL connection, paged by the
// request's first and after, with the cursor an offset.
func fakeConnection[T any](items []T, vars map[string]any) map[string]any {
	start, _ := strconv.Atoi(stringVar(vars, "after"))
	first := int(vars["first"].(float64))
	start = min(start, len(items))
	end := min(start+first, len(items))
	page := items[start:end]
	if page == nil {
		page = []T{}
	}
	return map[string]any{
		"nodes":    page,
		"pageInfo": map[string]any{"hasNextPage": end < len(items), "endCursor": strconv.Itoa(end)},
	}
}

// operationName is the name after "query" in a document.
func operationName(doc string) string {
	_, after, ok := strings.Cut(doc, "query ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(after, "(")
	name, _, _ = strings.Cut(name, " ")
	return strings.TrimSpace(name)
}

func stringVar(vars map[string]any, name string) string {
	s, _ := vars[name].(string)
	return s
}

// teamsFilterQuery recovers the search text from the TeamFilter the client
// builds: an or of containsIgnoreCase on key and name.
func teamsFilterQuery(vars map[string]any) string {
	filter, _ := vars["filter"].(map[string]any)
	or, _ := filter["or"].([]any)
	if len(or) == 0 {
		return ""
	}
	key, _ := or[0].(map[string]any)["key"].(map[string]any)
	q, _ := key["containsIgnoreCase"].(string)
	return strings.ToLower(q)
}

func writeGraphQLData(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// Calls is how many requests reached Linear — the assertion behind "a write
// that changes nothing about Linear asks Linear nothing".
func (f *linearCatalogFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// SetTeams replaces the workspace's teams, standing in for one created,
// deleted or made private after TF stored it.
func (f *linearCatalogFake) SetTeams(teams ...linear.Team) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teams = teams
}

// SetStates replaces one team's workflow.
func (f *linearCatalogFake) SetStates(teamID string, states ...linear.WorkflowState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[teamID] = states
}

// SetTeamFailing makes the Team read of one id fail while everything else
// keeps answering.
func (f *linearCatalogFake) SetTeamFailing(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teamFailing[id] = true
}

// SetFailing makes every later request fail, standing in for an unreachable
// or rejecting Linear.
func (f *linearCatalogFake) SetFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
}

// linearEndpointResolver resolves the org's credential exactly as production
// does — through the real resolver over the real secret store — and points the
// client at the fake rather than at Linear.
type linearEndpointResolver struct {
	linear.Resolver
	endpoint string
}

func (r linearEndpointResolver) ForSystem(ctx context.Context, orgID string) (*linear.Client, error) {
	cred, err := r.ResolveSystemCredential(ctx, orgID)
	if err != nil {
		return nil, err
	}
	cfg := cred.Config()
	cfg.Endpoint = r.endpoint
	return linear.NewClient(cfg).WithOrg(orgID), nil
}

var (
	linearFixtureEng = linear.Team{ID: linearTeamEng, Key: "ENG", Name: "Engineering"}
	linearFixtureOps = linear.Team{ID: linearTeamOps, Key: "OPS", Name: "Operations", Private: true}
)

// newServerWithLinearCatalog is a test server whose org holds a Linear API key
// and whose Linear client talks to a fake workspace carrying teams.
func newServerWithLinearCatalog(t *testing.T, teams ...linear.Team) (*Server, *linearCatalogFake) {
	t.Helper()
	s, fake := newServerWithUnconnectedLinear(t, teams...)
	if err := s.secrets.Put(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyLinearAPIKey, "lin_api_test", ""); err != nil {
		t.Fatalf("seed linear api key: %v", err)
	}
	bindLinearWorkspaceForTest(t, s, linearTestWorkspaceID)
	return s, fake
}

// linearTestWorkspaceID is the workspace the bound test credential belongs to.
const linearTestWorkspaceID = "ws-test"

// bindLinearWorkspaceForTest records workspaceID as the org's Linear
// workspace, which the credential bind does in production. Rules are read and
// written only in the org's current workspace.
func bindLinearWorkspaceForTest(t *testing.T, s *Server, workspaceID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE org_settings SET linear_workspace_id = ? WHERE org_id = ?`, workspaceID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("bind linear workspace: %v", err)
	}
}

// newServerWithUnconnectedLinear is newServerWithLinearCatalog before anyone
// bound a Linear credential.
func newServerWithUnconnectedLinear(t *testing.T, teams ...linear.Team) (*Server, *linearCatalogFake) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	fake := newLinearCatalogFake(t, teams...)
	s := newTestServer(t)
	s.linearResolver = linearEndpointResolver{Resolver: s.linearResolver, endpoint: fake.URL}
	return s, fake
}
