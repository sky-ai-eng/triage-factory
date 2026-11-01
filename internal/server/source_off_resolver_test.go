package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/zalando/go-keyring"
)

// These tests hold the off switch to its promise end to end: with a source
// turned off, every route that would resolve a Jira or Linear client answers
// the pause, or skips where it is best-effort, and the fake standing in for
// the tracker receives nothing. The resolvers are the server's own, so what
// is under test is the gate production wires, not a stand-in for it.

// turnSource turns kind off, or back on, through the org admin's route.
func turnSource(t *testing.T, s *Server, kind string, off bool) {
	t.Helper()
	if rec := doJSON(t, s, http.MethodPatch, orgSourcePath(kind), map[string]any{"disabled": off}); rec.Code != http.StatusOK {
		t.Fatalf("turn %s disabled=%v: %d: %s", kind, off, rec.Code, rec.Body.String())
	}
}

// assertAllPaused fails unless every response is the pause's 409.
func assertAllPaused(t *testing.T, recs map[string]*httptest.ResponseRecorder) {
	t.Helper()
	for name, rec := range recs {
		t.Run("off/"+name, func(t *testing.T) {
			assertOneFault(t, rec, http.StatusConflict, httpx.ReasonSourceDisabled, "")
		})
	}
}

// assertAllOK fails unless every response is a 200.
func assertAllOK(t *testing.T, recs map[string]*httptest.ResponseRecorder) {
	t.Helper()
	for name, rec := range recs {
		t.Run("on/"+name, func(t *testing.T) {
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSourceOff_JiraCatalogAndTrackedProjectsAskJiraNothing(t *testing.T) {
	s, fake := newServerWithJiraCatalog(t, "SKY")
	requests := func() map[string]*httptest.ResponseRecorder {
		return map[string]*httptest.ResponseRecorder{
			"projects list":    doJSON(t, s, http.MethodPost, jiraProjectsListPath, map[string]any{}),
			"project":          doJSON(t, s, http.MethodGet, jiraProjectPath("SKY"), nil),
			"statuses list":    doJSON(t, s, http.MethodPost, jiraStatusesListPath("SKY"), map[string]any{}),
			"status":           doJSON(t, s, http.MethodGet, jiraStatusPath("SKY", statusDoneID), nil),
			"priorities list":  doJSON(t, s, http.MethodPost, jiraPrioritiesPath()+"/list", map[string]any{}),
			"priority":         doJSON(t, s, http.MethodGet, jiraPrioritiesPath()+"/2", nil),
			"issue types list": doJSON(t, s, http.MethodPost, jiraIssueTypesListPath("SKY"), map[string]any{}),
			"issue type":       doJSON(t, s, http.MethodGet, jiraProjectPath("SKY")+"/issue-types/10004", nil),
			"tracked projects": putJiraProjects(t, s, "SKY"),
		}
	}

	turnSource(t, s, eventsource.KindJira, true)
	assertAllPaused(t, requests())
	if n := fake.Calls(); n != 0 {
		t.Errorf("Jira received %d requests while turned off, want none", n)
	}
	if got := teamJiraProjectKeys(t, s); len(got) != 0 {
		t.Errorf("tracked projects = %v while Jira is off, want none added", got)
	}

	turnSource(t, s, eventsource.KindJira, false)
	assertAllOK(t, requests())
	if fake.Calls() == 0 {
		t.Error("Jira received no request once turned back on")
	}
}

func TestSourceOff_LinearCatalogAndTrackedTeamsAskLinearNothing(t *testing.T) {
	s, fake := newServerWithLinearUsers(t)
	trackLinearTeams(t, s, linearTeamEng)
	requests := func() map[string]*httptest.ResponseRecorder {
		return map[string]*httptest.ResponseRecorder{
			"teams list":      doJSON(t, s, http.MethodPost, linearTeamsListPath, map[string]any{}),
			"team":            doJSON(t, s, http.MethodGet, linearTeamPath(linearTeamEng), nil),
			"states list":     doJSON(t, s, http.MethodPost, linearStatesListPath(linearTeamEng), map[string]any{}),
			"state":           doJSON(t, s, http.MethodGet, linearStatePath(linearTeamEng, linearStateDone), nil),
			"labels list":     doJSON(t, s, http.MethodPost, linearLabelsPath(linearTeamEng)+"/list", map[string]any{}),
			"label":           doJSON(t, s, http.MethodGet, linearLabelsPath(linearTeamEng)+"/"+linearLabelBug, nil),
			"users list":      doJSON(t, s, http.MethodPost, linearUsersListPath, map[string]any{}),
			"user":            doJSON(t, s, http.MethodGet, linearUserPath(linearUserAda), nil),
			"team visibility": doJSON(t, s, http.MethodGet, linearTeamVisibilityPath, nil),
			"tracked teams":   putLinearTeams(t, s, armedLinearTeam(linearTeamEng)),
		}
	}

	turnSource(t, s, eventsource.KindLinear, true)
	assertAllPaused(t, requests())
	if n := fake.Calls(); n != 0 {
		t.Errorf("Linear received %d requests while turned off, want none", n)
	}

	turnSource(t, s, eventsource.KindLinear, false)
	assertAllOK(t, requests())
	if fake.Calls() == 0 {
		t.Error("Linear received no request once turned back on")
	}
}

// The event-handler PATCH and promote check every catalog id they add with the
// tracker. With the tracker off that check answers the pause instead of a 502
// about an outage; a 422 the body already earned still wins.
func TestSourceOff_EventHandlerCatalogCheckAsksJiraNothing(t *testing.T) {
	r := newJiraCatalogRig(t)
	seedBlueprintForTrigger(t, r.s, "bp-jira-off")
	rule := func() string {
		t.Helper()
		code, created := r.createRule(t, domain.EventJiraIssueAssigned, nil)
		if code != http.StatusCreated {
			t.Fatalf("seed rule: %d: %v", code, created)
		}
		return created["id"].(string)
	}
	patched, promoted := rule(), rule()
	adding := map[string]any{"status_id_in": []string{statusInProgressID}}
	requests := func() map[string]*httptest.ResponseRecorder {
		return map[string]*httptest.ResponseRecorder{
			"patch": doJSON(t, r.s, http.MethodPatch, "/api/event-handlers/"+patched, map[string]any{"scope_predicate": adding}),
			"promote": doJSON(t, r.s, http.MethodPost, "/api/event-handlers/"+promoted+"/promote", map[string]any{
				"blueprint_id": "bp-jira-off", "breaker_threshold": 3, "min_autonomy_suitability": 0.0, "scope_predicate": adding,
			}),
		}
	}

	turnSource(t, r.s, eventsource.KindJira, true)
	calls := r.fake.Calls()
	assertAllPaused(t, requests())
	if code, got := r.patch(t, patched, map[string]any{"default_priority": 2, "scope_predicate": adding}); code != http.StatusUnprocessableEntity {
		t.Errorf("PATCH with a fault of its own = %d, want that 422 over the pause: %v", code, got)
	}
	if n := r.fake.Calls(); n != calls {
		t.Errorf("Jira received %d requests while turned off, want none", n-calls)
	}

	turnSource(t, r.s, eventsource.KindJira, false)
	assertAllOK(t, requests())
	if r.fake.Calls() == calls {
		t.Error("Jira received no request once turned back on")
	}
}

// redirectedJiraResolver is the server's own Jira resolver, gate and
// credential reads included, with every user client it resolves pointed at a
// fake instead of the org's Jira site. It counts the clients it hands out: a
// resolution the gate refused hands out none, so nothing can reach Jira
// through it, which holds for a write the handler runs on a goroutine of its
// own as much as for one it runs inline.
type redirectedJiraResolver struct {
	jira.Resolver

	mu      sync.Mutex
	url     string
	clients int
}

func (r *redirectedJiraResolver) ForUser(ctx context.Context, orgID, userID string) (*jira.Client, error) {
	if _, err := r.Resolver.ForUser(ctx, orgID, userID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients++
	return jira.NewClient(jira.DataCenterPAT(r.url, "user-tok")).WithOrg(orgID), nil
}

func (r *redirectedJiraResolver) pointAt(url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.url = url
}

func (r *redirectedJiraResolver) handedOut() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients
}

// redirectedLinearResolver is redirectedJiraResolver for Linear.
type redirectedLinearResolver struct {
	linear.Resolver

	mu       sync.Mutex
	endpoint string
	clients  int
}

func (r *redirectedLinearResolver) ForUser(ctx context.Context, orgID, userID string) (*linear.Client, error) {
	if _, err := r.Resolver.ForUser(ctx, orgID, userID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients++
	cfg := linear.APIKey("lin_user_test")
	cfg.Endpoint = r.endpoint
	return linear.NewClient(cfg).WithOrg(orgID), nil
}

func (r *redirectedLinearResolver) pointAt(endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoint = endpoint
}

func (r *redirectedLinearResolver) handedOut() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients
}

// entitySnapshots reads the snapshots of the entities ids names, for
// restoreSnapshots to put back: turning a source off clears them, and a test
// that turns it back on stands in for the poll cycle that re-seeds them.
func entitySnapshots(t *testing.T, s *Server, ids ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, id := range ids {
		var snapshot string
		if err := s.db.QueryRow(`SELECT snapshot_json FROM entities WHERE id = ?`, id).Scan(&snapshot); err != nil {
			t.Fatalf("read snapshot of %s: %v", id, err)
		}
		out[id] = snapshot
	}
	return out
}

func restoreSnapshots(t *testing.T, s *Server, snapshots map[string]string) {
	t.Helper()
	for id, snapshot := range snapshots {
		if _, err := s.db.Exec(`UPDATE entities SET snapshot_json = ? WHERE id = ?`, snapshot, id); err != nil {
			t.Fatalf("restore snapshot of %s: %v", id, err)
		}
	}
}

// userWriteRig drives one task through the user-initiated writes the off
// switch must stop: a claim, which writes before it lands, and the requeue's
// reversal of a claim, which is best-effort.
type userWriteRig struct {
	s      *Server
	taskID string
}

func (r userWriteRig) claim(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, r.s, http.MethodPost, "/api/tasks/"+r.taskID+"/claim", map[string]any{"hesitation_ms": 0})
}

func (r userWriteRig) mustClaim(t *testing.T) {
	t.Helper()
	if rec := r.claim(t); rec.Code != http.StatusOK {
		t.Fatalf("claim = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func (r userWriteRig) requeue(t *testing.T) {
	t.Helper()
	if rec := doJSON(t, r.s, http.MethodPost, "/api/tasks/"+r.taskID+"/requeue", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// With Jira off, a person's claim and the stock deck's claim and done refuse
// with the pause before writing, and a requeue lands without reversing its
// claim in Jira. Turned back on, each reaches Jira again.
func TestSourceOff_JiraUserWritesAskJiraNothing(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s)
	seedStockTicket(t, s, "SKY-1")
	seedStockTicket(t, s, "SKY-2")
	if err := s.secrets.PutUser(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		jira.UserTokenKey("https://jira.example.com"), "user-pat", "Jira user token"); err != nil {
		t.Fatalf("seed user credential: %v", err)
	}
	r := userWriteRig{s: s, taskID: "00000000-0000-4000-8000-000000001222"}
	seedQueuedJiraTask(t, s.db, "e_off_jira", "SKY-9", r.taskID)
	if _, err := s.db.Exec(`UPDATE entities SET snapshot_json = ? WHERE id = 'e_off_jira'`,
		`{"key":"SKY-9","status":"To Do","status_id":"`+statusToDoID+`","assignee":""}`); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	onFake, offFake := newJiraIssueFake(t, statusToDoID, ""), newJiraIssueFake(t, statusToDoID, "")
	res := &redirectedJiraResolver{Resolver: s.jiraResolver, url: onFake.URL}
	s.jiraResolver = res

	r.mustClaim(t)
	snapshots := entitySnapshots(t, s, "e_off_jira", "e_stock_SKY-1", "e_stock_SKY-2")

	res.pointAt(offFake.URL)
	turnSource(t, s, eventsource.KindJira, true)
	resolved := res.handedOut()
	r.requeue(t)
	assertAllPaused(t, map[string]*httptest.ResponseRecorder{
		"claim":       r.claim(t),
		"stock claim": doJSON(t, s, http.MethodPost, "/api/jira/stock/claim", map[string]any{"entity_ids": []string{"e_stock_SKY-1"}}),
		"stock done":  doJSON(t, s, http.MethodPost, "/api/jira/stock/done", map[string]any{"entity_ids": []string{"e_stock_SKY-2"}}),
	})
	if who := claimedBy(t, s.db, r.taskID); who != "" {
		t.Errorf("claimed by %q while Jira is off, want the claim refused before it landed", who)
	}
	if n := res.handedOut() - resolved; n != 0 {
		t.Errorf("resolved %d Jira clients while Jira is off, want none", n)
	}

	res.pointAt(onFake.URL)
	turnSource(t, s, eventsource.KindJira, false)
	restoreSnapshots(t, s, snapshots)
	resolved = res.handedOut()
	r.mustClaim(t)
	r.requeue(t)
	if n := res.handedOut() - resolved; n != 2 {
		t.Errorf("the claim and the requeue resolved %d Jira clients once Jira is back on, want 2", n)
	}
	before := onFake.Requests()
	for action, entityID := range map[string]string{"claim": "e_stock_SKY-1", "done": "e_stock_SKY-2"} {
		if rec, body := postStock(t, s, action, entityID); rec.Code != http.StatusOK || body.Applied != 1 {
			t.Errorf("stock %s once Jira is back on = %d %s, want it applied", action, rec.Code, rec.Body.String())
		}
	}
	if onFake.Requests() == before {
		t.Error("the stock deck sent Jira nothing once it was back on")
	}
	if n := offFake.Requests(); n != 0 {
		t.Errorf("Jira received %d requests while turned off, want none", n)
	}
}

func TestSourceOff_LinearUserWritesAskLinearNothing(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	bindLinearWorkspaceForTest(t, s, linearTestWorkspaceID)
	ctx := t.Context()
	if err := s.users.UpsertLinearIdentity(ctx, runmode.LocalDefaultUserID, linearTestWorkspaceID, linearMe, "Me", "api_key"); err != nil {
		t.Fatalf("seed linear identity: %v", err)
	}
	if _, err := s.linearRules.ReplaceForTeam(ctx, runmode.LocalDefaultTeamID, linearTestWorkspaceID, []domain.LinearTeamRules{linearClaimRule}); err != nil {
		t.Fatalf("seed linear rule: %v", err)
	}
	cred, err := linear.MarshalUserCredential(linear.UserCredential{Method: linear.AuthMethodAPIKey, Token: "lin_user_test"})
	if err != nil {
		t.Fatalf("marshal user credential: %v", err)
	}
	if err := s.secrets.PutUser(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID,
		linear.UserTokenKey(linearTestWorkspaceID), cred, "Linear user credential"); err != nil {
		t.Fatalf("seed user credential: %v", err)
	}
	r := userWriteRig{s: s, taskID: "00000000-0000-4000-8000-000000001223"}
	seedQueuedLinearTask(t, s.db, r.taskID, linearStateTodoRef)
	onFake := newLinearIssueFake(t, nil, linearFixtureState(t, linearStateTodo))
	offFake := newLinearIssueFake(t, nil, linearFixtureState(t, linearStateTodo))
	res := &redirectedLinearResolver{Resolver: s.linearResolver, endpoint: onFake.URL}
	s.linearResolver = res

	r.mustClaim(t)
	onFake.waitForWrites(t, 2)
	snapshots := entitySnapshots(t, s, "e_"+r.taskID)

	res.pointAt(offFake.URL)
	turnSource(t, s, eventsource.KindLinear, true)
	resolved := res.handedOut()
	r.requeue(t)
	assertAllPaused(t, map[string]*httptest.ResponseRecorder{"claim": r.claim(t)})
	if who := claimedBy(t, s.db, r.taskID); who != "" {
		t.Errorf("claimed by %q while Linear is off, want the claim refused before it landed", who)
	}
	if n := res.handedOut() - resolved; n != 0 {
		t.Errorf("resolved %d Linear clients while Linear is off, want none", n)
	}

	res.pointAt(onFake.URL)
	turnSource(t, s, eventsource.KindLinear, false)
	restoreSnapshots(t, s, snapshots)
	resolved = res.handedOut()
	r.mustClaim(t)
	r.requeue(t)
	if n := res.handedOut() - resolved; n != 2 {
		t.Errorf("the claim and the requeue resolved %d Linear clients once Linear is back on, want 2", n)
	}
	if n := offFake.Requests(); n != 0 {
		t.Errorf("Linear received %d requests while turned off, want none", n)
	}
}
