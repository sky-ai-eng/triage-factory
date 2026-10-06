package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func teamLinearTeamsPath(teamID string) string { return "/api/teams/" + teamID + "/linear-teams" }

// armedLinearTeam is a body element mapping all three rules the way the
// picker's pre-fill would: pickup from the not-started states, in_progress
// from the started ones, done from completed and canceled.
func armedLinearTeam(id string) map[string]any {
	return map[string]any{
		"id":          id,
		"pickup":      map[string]any{"member_ids": []string{linearStateTriage, linearStateBacklog, linearStateTodo}},
		"in_progress": map[string]any{"member_ids": []string{linearStateDoing, linearStateReview}, "canonical_id": linearStateDoing},
		"done":        map[string]any{"member_ids": []string{linearStateDone, linearStateCanceled}, "canonical_id": linearStateDone},
	}
}

func putLinearTeams(t *testing.T, s *Server, elems ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if elems == nil {
		elems = []map[string]any{}
	}
	return doJSON(t, s, http.MethodPut, teamLinearTeamsPath("default"), map[string]any{"linear_teams": elems})
}

func decodeLinearTeams(t *testing.T, rec *httptest.ResponseRecorder) []linearTeamSettings {
	t.Helper()
	var out struct {
		LinearTeams []linearTeamSettings `json:"linear_teams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode linear_teams: %v; body=%s", err, rec.Body.String())
	}
	if out.LinearTeams == nil {
		t.Fatalf("linear_teams is null; body=%s", rec.Body.String())
	}
	return out.LinearTeams
}

// storedLinearTeamsRead is the team-settings read's linear_teams — what a
// client sees after the write.
func storedLinearTeamsRead(t *testing.T, s *Server) []linearTeamSettings {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, teamSettingsPath("default"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET team settings = %d; body=%s", rec.Code, rec.Body.String())
	}
	return decodeLinearTeams(t, rec)
}

func mustPutLinearTeams(t *testing.T, s *Server, elems ...map[string]any) []linearTeamSettings {
	t.Helper()
	rec := putLinearTeams(t, s, elems...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT linear-teams = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return decodeLinearTeams(t, rec)
}

// linearKicks reports whether the write re-dued Linear polling, waiting
// briefly because the callback fires on its own goroutine.
func linearKicks(t *testing.T, s *Server) func() bool {
	t.Helper()
	ch := make(chan struct{}, 8)
	s.SetOnLinearChanged(func(string) { ch <- struct{}{} })
	return func() bool {
		select {
		case <-ch:
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
}

func stateNames(refs []linearStateRefJSON) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name+"/"+r.Type)
	}
	return strings.Join(names, ",")
}

// TestLinearTeamsPut_ArmsATeam is the happy path: ids in, and back out the
// team's key and name and each state's name and type as Linear gives them,
// identical to what the next settings read shows.
func TestLinearTeamsPut_ArmsATeam(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	kicked := linearKicks(t, s)

	got := mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))
	if len(got) != 1 {
		t.Fatalf("echo = %+v, want one team", got)
	}
	eng := got[0]
	if eng.ID != linearTeamEng || eng.Key != "ENG" || eng.Name != "Engineering" || !eng.Armed {
		t.Errorf("echo = %+v, want ENG / Engineering, armed", eng)
	}
	if n := stateNames(eng.Pickup.Members); n != "Triage/triage,Backlog/backlog,Todo/unstarted" {
		t.Errorf("pickup = %s", n)
	}
	if n := stateNames(eng.InProgress.Members); n != "In Progress/started,In Review/started" {
		t.Errorf("in_progress = %s", n)
	}
	if eng.InProgress.Canonical == nil || eng.InProgress.Canonical.ID != linearStateDoing || eng.InProgress.Canonical.Name != "In Progress" {
		t.Errorf("in_progress canonical = %+v", eng.InProgress.Canonical)
	}
	if eng.Done.Canonical == nil || *eng.Done.Canonical != (linearStateRefJSON{ID: linearStateDone, Name: "Done", Type: "completed"}) {
		t.Errorf("done canonical = %+v", eng.Done.Canonical)
	}
	if read := storedLinearTeamsRead(t, s); !reflect.DeepEqual(read, got) {
		t.Errorf("settings read after the write =\n%+v\nwant the echo\n%+v", read, got)
	}
	if !kicked() {
		t.Error("arming a Linear team did not re-due Linear polling")
	}
}

// TestLinearTeamsPut_WatchWithoutRules: an element naming only an id watches
// the team. It is stored, with its key and name from Linear, and maps nothing
// — so polling has nothing new to ask.
func TestLinearTeamsPut_WatchWithoutRules(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	kicked := linearKicks(t, s)

	got := mustPutLinearTeams(t, s, map[string]any{"id": linearTeamEng})
	want := []linearTeamSettings{{
		ID: linearTeamEng, Key: "ENG", Name: "Engineering", Armed: false,
		Pickup:     linearPickupRuleJSON{Members: []linearStateRefJSON{}},
		InProgress: linearStateRuleJSON{Members: []linearStateRefJSON{}},
		Done:       linearStateRuleJSON{Members: []linearStateRefJSON{}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("echo =\n%+v\nwant\n%+v", got, want)
	}
	if read := storedLinearTeamsRead(t, s); !reflect.DeepEqual(read, want) {
		t.Errorf("settings read = %+v, want %+v", read, want)
	}
	if kicked() {
		t.Error("watching an unmapped Linear team re-dued Linear polling")
	}
}

// TestLinearTeamsPut_BodyOrderIsDisplayOrder: the stored order is the order
// the caller sent, not the store's id order.
func TestLinearTeamsPut_BodyOrderIsDisplayOrder(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	first, second := linearTeamOps, linearTeamEng
	if first < second {
		first, second = second, first // send them against id order
	}
	got := mustPutLinearTeams(t, s, map[string]any{"id": first}, armedLinearTeam(second))
	if got[0].ID != first || got[1].ID != second {
		t.Errorf("echo order = %s,%s, want %s,%s", got[0].ID, got[1].ID, first, second)
	}
	read := storedLinearTeamsRead(t, s)
	if read[0].ID != first || read[1].ID != second {
		t.Errorf("read order = %s,%s, want %s,%s", read[0].ID, read[1].ID, first, second)
	}
}

// TestLinearTeamsPut_ShapeFaults: what the body itself gets wrong is a 400
// naming the field, before any read and before Linear is asked anything.
func TestLinearTeamsPut_ShapeFaults(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	withPickup := func(ids ...string) map[string]any {
		e := armedLinearTeam(linearTeamEng)
		e["pickup"] = map[string]any{"member_ids": ids}
		return e
	}
	withDone := func(rule map[string]any) map[string]any {
		e := armedLinearTeam(linearTeamEng)
		e["done"] = rule
		return e
	}
	cases := []struct {
		name          string
		body          map[string]any
		reason, field string
	}{
		{"no set named", map[string]any{}, "MISSING_FIELD", "linear_teams"},
		{"element with no id", map[string]any{"linear_teams": []map[string]any{{}}}, "MISSING_FIELD", "linear_teams[0].id"},
		{"a key, not an id", map[string]any{"linear_teams": []map[string]any{{"id": "ENG"}}}, "INVALID_FIELD", "linear_teams[0].id"},
		{"upper-case id", map[string]any{"linear_teams": []map[string]any{{"id": strings.ToUpper(linearTeamEng)}}}, "INVALID_FIELD", "linear_teams[0].id"},
		{"team twice", map[string]any{"linear_teams": []map[string]any{{"id": linearTeamEng}, {"id": linearTeamEng}}}, "INVALID_FIELD", "linear_teams[1].id"},
		{"a state name", map[string]any{"linear_teams": []map[string]any{withPickup("Todo")}}, "INVALID_FIELD", "linear_teams[0].pickup.member_ids"},
		{"a state twice", map[string]any{"linear_teams": []map[string]any{withPickup(linearStateTodo, linearStateTodo)}}, "INVALID_FIELD", "linear_teams[0].pickup.member_ids"},
		{"canonical not an id", map[string]any{"linear_teams": []map[string]any{withDone(map[string]any{"member_ids": []string{linearStateDone}, "canonical_id": "Done"})}}, "INVALID_FIELD", "linear_teams[0].done.canonical_id"},
		{"pickup has no canonical", map[string]any{"linear_teams": []map[string]any{{"id": linearTeamEng, "pickup": map[string]any{"member_ids": []string{}, "canonical_id": linearStateTodo}}}}, "UNKNOWN_FIELD", "canonical_id"},
		{"an in_review rule", map[string]any{"linear_teams": []map[string]any{{"id": linearTeamEng, "in_review": map[string]any{}}}}, "UNKNOWN_FIELD", "in_review"},
		{"a name with the id", map[string]any{"linear_teams": []map[string]any{{"id": linearTeamEng, "name": "Engineering"}}}, "UNKNOWN_FIELD", "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, teamLinearTeamsPath("default"), c.body)
			assertLinearFault(t, rec, http.StatusBadRequest, c.reason, c.field)
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a shape fault reached Linear %d times", fake.Calls())
	}
	if got := storedLinearTeamsRead(t, s); len(got) != 0 {
		t.Errorf("a refused write stored %+v", got)
	}
}

// TestLinearTeamsPut_TeamNotVisible: the picker offers only teams the org
// credential can see, so the write refuses any other — a 422 naming the
// element, and none of the set is stored.
func TestLinearTeamsPut_TeamNotVisible(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	rec := putLinearTeams(t, s, armedLinearTeam(linearTeamEng), map[string]any{"id": linearTeamGhost})
	assertLinearFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[1].id")
	if got := storedLinearTeamsRead(t, s); len(got) != 0 {
		t.Errorf("a refused write stored %+v", got)
	}
}

// TestLinearTeamsPut_StateFromAnotherWorkflow: every state id must be one of
// the team's own; the fault names the rule it arrived in.
func TestLinearTeamsPut_StateFromAnotherWorkflow(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	e := armedLinearTeam(linearTeamEng)
	e["in_progress"] = map[string]any{"member_ids": []string{linearStateDoing, linearStateUnknown}, "canonical_id": linearStateDoing}
	rec := putLinearTeams(t, s, e)
	assertLinearFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0].in_progress.member_ids")
	if !strings.Contains(decodeErrorItems(t, rec)[0].Message, linearStateUnknown) {
		t.Errorf("message does not name the unknown state: %s", rec.Body.String())
	}
}

// TestLinearTeamsPut_RuleFaults: a write-target rule is members and a canonical
// together, the canonical one of the members — decided from the ids alone, so
// Linear is not asked.
func TestLinearTeamsPut_RuleFaults(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	cases := []struct {
		name  string
		rule  string
		value map[string]any
		field string
	}{
		{"members, no canonical", "in_progress", map[string]any{"member_ids": []string{linearStateDoing}}, "linear_teams[0].in_progress.canonical_id"},
		{"canonical outside members", "in_progress", map[string]any{"member_ids": []string{linearStateDoing}, "canonical_id": linearStateReview}, "linear_teams[0].in_progress.canonical_id"},
		{"canonical, no members", "done", map[string]any{"member_ids": []string{}, "canonical_id": linearStateDone}, "linear_teams[0].done.member_ids"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := armedLinearTeam(linearTeamEng)
			e[c.rule] = c.value
			assertLinearFault(t, putLinearTeams(t, s, e), http.StatusUnprocessableEntity, "INVALID_FIELD", c.field)
		})
	}
	if fake.Calls() != 0 {
		t.Errorf("a rule fault reached Linear %d times", fake.Calls())
	}
}

// TestLinearTeamsPut_HalfAMappingRefused: a team is watched with no rules or
// mapped with all three. Anything between is a 422 on the element.
func TestLinearTeamsPut_HalfAMappingRefused(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	full := armedLinearTeam(linearTeamEng)

	pickupOnly := map[string]any{"id": linearTeamEng, "pickup": full["pickup"]}
	assertLinearFault(t, putLinearTeams(t, s, pickupOnly), http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0]")

	noDone := map[string]any{"id": linearTeamEng, "pickup": full["pickup"], "in_progress": full["in_progress"]}
	rec := putLinearTeams(t, s, noDone)
	assertLinearFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0]")
	if msg := decodeErrorItems(t, rec)[0].Message; !strings.Contains(msg, "done") {
		t.Errorf("message %q does not name the unmapped rule", msg)
	}

	// Clearing one rule of an armed team is the same half mapping, reached
	// from the other side.
	mustPutLinearTeams(t, s, full)
	clearDone := map[string]any{"id": linearTeamEng, "done": map[string]any{"member_ids": []string{}, "canonical_id": ""}}
	assertLinearFault(t, putLinearTeams(t, s, clearDone), http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0]")
	if read := storedLinearTeamsRead(t, s); len(read) != 1 || !read[0].Armed {
		t.Errorf("a refused write changed the stored team: %+v", read)
	}
}

// TestLinearTeamsPut_AbsentRuleKeepsStored: resending an armed team as just its
// id keeps its rules, and asks Linear nothing.
func TestLinearTeamsPut_AbsentRuleKeepsStored(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	armed := mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))
	before := fake.Calls()
	kicked := linearKicks(t, s)

	got := mustPutLinearTeams(t, s, map[string]any{"id": linearTeamEng})
	if !reflect.DeepEqual(got, armed) {
		t.Errorf("id-only resend changed the team:\n%+v\nwant\n%+v", got, armed)
	}
	if fake.Calls() != before {
		t.Errorf("a resend changing nothing asked Linear %d times", fake.Calls()-before)
	}
	if kicked() {
		t.Error("a resend changing nothing re-dued Linear polling")
	}
}

// TestLinearTeamsPut_DisarmIsThreeEmptyRules: clearing every rule unarms the
// team without untracking it, without asking Linear, and re-dues polling
// because what the poller asks changed.
func TestLinearTeamsPut_DisarmIsThreeEmptyRules(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))
	before := fake.Calls()
	kicked := linearKicks(t, s)

	empty := map[string]any{"member_ids": []string{}, "canonical_id": ""}
	got := mustPutLinearTeams(t, s, map[string]any{
		"id": linearTeamEng, "pickup": map[string]any{"member_ids": []string{}}, "in_progress": empty, "done": empty,
	})
	if len(got) != 1 || got[0].Armed || len(got[0].Pickup.Members) != 0 || got[0].Done.Canonical != nil {
		t.Errorf("disarmed echo = %+v", got)
	}
	if got[0].Key != "ENG" {
		t.Errorf("disarming lost the stored key: %+v", got[0])
	}
	if fake.Calls() != before {
		t.Errorf("disarming asked Linear %d times", fake.Calls()-before)
	}
	if !kicked() {
		t.Error("disarming a team did not re-due Linear polling")
	}
}

// TestLinearTeamsPut_StoredTeamGrandfathered: a team deleted in Linear, or a
// Linear that is down, never locks the team out of resending or removing what
// it stored.
func TestLinearTeamsPut_StoredTeamGrandfathered(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng), map[string]any{"id": linearTeamOps})

	fake.SetTeams(linearFixtureOps) // ENG deleted upstream
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng), map[string]any{"id": linearTeamOps})

	fake.SetFailing(true)
	got := mustPutLinearTeams(t, s, map[string]any{"id": linearTeamOps})
	if len(got) != 1 || got[0].ID != linearTeamOps {
		t.Errorf("removal with Linear down = %+v, want only OPS left", got)
	}
	if got := mustPutLinearTeams(t, s); len(got) != 0 {
		t.Errorf("clearing the set left %+v", got)
	}
}

// TestLinearTeamsPut_ChangedRuleRefreshesTheTeam: a team the write asks Linear
// about is stored as Linear names it now — its key and the states' names.
func TestLinearTeamsPut_ChangedRuleRefreshesTheTeam(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))

	fake.SetTeams(linear.Team{ID: linearTeamEng, Key: "PLAT", Name: "Platform"})
	renamed := append([]linear.WorkflowState(nil), linearFixtureStates...)
	for i := range renamed {
		if renamed[i].ID == linearStateDoing {
			renamed[i].Name = "Doing"
		}
	}
	fake.SetStates(linearTeamEng, renamed...)

	e := armedLinearTeam(linearTeamEng)
	e["in_progress"] = map[string]any{"member_ids": []string{linearStateDoing}, "canonical_id": linearStateDoing}
	got := mustPutLinearTeams(t, s, e)
	if got[0].Key != "PLAT" || got[0].Name != "Platform" {
		t.Errorf("team = %s / %s, want the refreshed PLAT / Platform", got[0].Key, got[0].Name)
	}
	if got[0].InProgress.Canonical == nil || got[0].InProgress.Canonical.Name != "Doing" {
		t.Errorf("in_progress canonical = %+v, want the renamed state", got[0].InProgress.Canonical)
	}
}

// TestLinearTeamsPut_UpstreamFailureStoresNothing: adding a team while Linear
// is unreachable is a 502, never a stored-unverified row.
func TestLinearTeamsPut_UpstreamFailureStoresNothing(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	fake.SetFailing(true)
	assertLinearFault(t, putLinearTeams(t, s, armedLinearTeam(linearTeamEng)), http.StatusBadGateway, "UPSTREAM_UNAVAILABLE", "")
	if got := storedLinearTeamsRead(t, s); len(got) != 0 {
		t.Errorf("an unverified write stored %+v", got)
	}
}

// TestLinearTeamsPut_FieldFaultSurvivesALaterUpstreamFailure: a fault already
// found is certain whether or not Linear answers later in the set, so it is
// what the caller hears.
func TestLinearTeamsPut_FieldFaultSurvivesALaterUpstreamFailure(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	// ENG resolves and names a state its workflow does not have; the read of
	// OPS, next in the set, then fails.
	e := armedLinearTeam(linearTeamEng)
	e["pickup"] = map[string]any{"member_ids": []string{linearStateUnknown}}
	fake.SetTeamFailing(linearTeamOps)
	rec := putLinearTeams(t, s, e, map[string]any{"id": linearTeamOps})
	assertLinearFault(t, rec, http.StatusUnprocessableEntity, "INVALID_FIELD", "linear_teams[0].pickup.member_ids")
}

// TestLinearTeamsPut_UnconnectedWorkspace: with no Linear credential nothing
// can be added — a 409 naming why — but a removal still goes through.
func TestLinearTeamsPut_UnconnectedWorkspace(t *testing.T) {
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng)
	mustPutLinearTeams(t, s, armedLinearTeam(linearTeamEng))
	if _, err := s.secrets.Delete(t.Context(), runmode.LocalDefaultOrgID, integrations.KeyLinearAPIKey); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	assertLinearFault(t, putLinearTeams(t, s, armedLinearTeam(linearTeamEng), map[string]any{"id": linearTeamOps}),
		http.StatusConflict, "NOT_CONFIGURED", "")
	if got := mustPutLinearTeams(t, s); len(got) != 0 {
		t.Errorf("removal on an unconnected workspace left %+v", got)
	}
}
