package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// jiraCatalogRig is a test server whose org's Jira credential points at a fake
// catalog serving the SKY and OPS projects, both in the registry on the org's
// site, with the local team tracking SKY. OPS's workflow has a status SKY's
// does not.
type jiraCatalogRig struct {
	s        *Server
	fake     *jiraCatalogFake
	site     string
	sky, ops string // registry row ids
}

// opsOnlyStatusID is a status only OPS's workflow has.
const opsOnlyStatusID = "20000"

func newJiraCatalogRig(t *testing.T) *jiraCatalogRig {
	t.Helper()
	s, fake := newServerWithJiraCatalog(t, "SKY", "OPS")
	fake.SetStatuses("OPS", fakeJiraStatus{ID: opsOnlyStatusID, Name: "Triage"})
	orgSet, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	r := &jiraCatalogRig{s: s, fake: fake, site: domain.EntityScope("jira", orgSet)}
	register := func(key string) string {
		p, err := s.allStores.JiraProjects.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, r.site, fakeJiraProjectID(key), key, key+" Project")
		if err != nil {
			t.Fatalf("register %s: %v", key, err)
		}
		return p.ID
	}
	r.sky, r.ops = register("SKY"), register("OPS")
	r.track(t, r.sky)
	return r
}

// track makes ids the local team's whole tracked set on the rig's site.
func (r *jiraCatalogRig) track(t *testing.T, ids ...string) {
	t.Helper()
	rules := make([]domain.JiraProjectStatusRules, 0, len(ids))
	for _, id := range ids {
		rules = append(rules, domain.JiraProjectStatusRules{JiraProjectID: id})
	}
	if err := r.s.allStores.JiraStatusRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, r.site, rules); err != nil {
		t.Fatalf("track jira projects: %v", err)
	}
}

func (r *jiraCatalogRig) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	rec := doJSON(t, r.s, method, path, body)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

func (r *jiraCatalogRig) createRule(t *testing.T, eventType string, predicate map[string]any) (int, map[string]any) {
	t.Helper()
	return r.do(t, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type": eventType, "name": "scoped", "scope_predicate": predicate,
	})
}

func (r *jiraCatalogRig) patch(t *testing.T, id string, body map[string]any) (int, map[string]any) {
	t.Helper()
	return r.do(t, http.MethodPatch, "/api/event-handlers/"+id, body)
}

type fieldFault struct{ field, value string }

// wantFieldFaults asserts a 422 whose faults name each field and value, in
// order.
func wantFieldFaults(t *testing.T, code int, got map[string]any, want ...fieldFault) {
	t.Helper()
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != len(want) {
		t.Fatalf("faults = %q, want %d", messages, len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(messages[i], w.field+": ") || !strings.Contains(messages[i], fmt.Sprintf("%q", w.value)) {
			t.Errorf("fault %d = %q, want it to name %s and %q", i, messages[i], w.field, w.value)
		}
	}
}

// A status of a tracked project's workflow, an issue type of a tracked
// project and one of the instance's priorities are accepted and stored as
// sent.
func TestEventHandlerJiraCatalog_AcceptsIDsTheEditorOffers(t *testing.T) {
	r := newJiraCatalogRig(t)
	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{
		"status_id_in":     []string{statusInProgressID},
		"priority_id_in":   []string{"2"},
		"issue_type_id_in": []string{"10004"},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := fmt.Sprintf(`{"issue_type_id_in":["10004"],"priority_id_in":["2"],"status_id_in":[%q]}`, statusInProgressID)
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}
}

// A name, a status only an untracked project's workflow has, and an id no
// catalog holds are each refused, every one reported beside the database
// check's fault for an untracked project, and nothing is written.
func TestEventHandlerJiraCatalog_RefusesNamesAndUntrackedIDs(t *testing.T) {
	r := newJiraCatalogRig(t)
	before := len(listEventHandlers(t, r.s, "rule"))

	code, got := r.createRule(t, domain.EventJiraIssueAvailable, map[string]any{
		"jira_project_id_in": []string{r.ops},
		"status_id_in":       []string{statusToDoID, "In Progress", opsOnlyStatusID},
		"priority_id_in":     []string{"High", "99"},
		"issue_type_id_in":   []string{"Bug"},
	})
	wantFieldFaults(t, code, got,
		fieldFault{"jira_project_id_in", r.ops},
		fieldFault{"status_id_in", "In Progress"},
		fieldFault{"status_id_in", opsOnlyStatusID},
		fieldFault{"issue_type_id_in", "Bug"},
		fieldFault{"priority_id_in", "High"},
		fieldFault{"priority_id_in", "99"},
	)
	if after := len(listEventHandlers(t, r.s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}
}

func TestEventHandlerJiraCatalog_TransitionFiltersAreChecked(t *testing.T) {
	r := newJiraCatalogRig(t)
	code, got := r.createRule(t, domain.EventJiraIssueStatusChanged, map[string]any{
		"new_status_id_in": []string{statusInReviewID},
		"old_status_id_in": []string{"To Do"},
	})
	wantFieldFaults(t, code, got, fieldFault{"old_status_id_in", "To Do"})
	code, got = r.createRule(t, domain.EventJiraIssuePriorityChanged, map[string]any{
		"new_priority_id_in": []string{"1"},
		"old_priority_id_in": []string{"Low"},
	})
	wantFieldFaults(t, code, got, fieldFault{"old_priority_id_in", "Low"})
}

// A write asks Jira only about the ids it adds: an edit that adds none
// succeeds while Jira is down without reaching it, one that adds an id while
// Jira is down stores nothing and says Jira did not answer, and a stored id
// Jira no longer offers does not block an edit that keeps it.
func TestEventHandlerJiraCatalog_AsksJiraOnlyAboutAddedIDs(t *testing.T) {
	r := newJiraCatalogRig(t)
	code, created := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{"status_id_in": []string{statusToDoID}})
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	kept := fmt.Sprintf(`{"status_id_in":[%q]}`, statusToDoID)

	r.fake.SetFailing(true)
	calls := r.fake.Calls()
	if code, got := r.patch(t, id, map[string]any{"name": "renamed"}); code != http.StatusOK {
		t.Fatalf("name-only PATCH = %d: %v", code, got)
	}
	if code, got := r.patch(t, id, map[string]any{"scope_predicate": map[string]any{"status_id_in": []string{statusToDoID}}}); code != http.StatusOK {
		t.Fatalf("PATCH resending the stored predicate = %d: %v", code, got)
	}
	if n := r.fake.Calls(); n != calls {
		t.Errorf("edits that add no id reached Jira %d times", n-calls)
	}

	code, got := r.patch(t, id, map[string]any{"scope_predicate": map[string]any{"status_id_in": []string{statusToDoID, statusDoneID}}})
	if code != http.StatusBadGateway {
		t.Fatalf("PATCH adding an id while Jira is down = %d, want 502: %v", code, got)
	}
	if _, row := r.do(t, http.MethodGet, "/api/event-handlers/"+id, nil); row["scope_predicate_json"] != kept {
		t.Errorf("a refused PATCH changed the predicate to %v", row["scope_predicate_json"])
	}

	r.fake.SetFailing(false)
	r.fake.SetStatuses("SKY", fakeJiraStatus{ID: statusDoneID, Name: "Done"})
	if code, got := r.patch(t, id, map[string]any{"scope_predicate": map[string]any{"status_id_in": []string{statusToDoID, statusDoneID}}}); code != http.StatusOK {
		t.Errorf("PATCH keeping a status the workflow dropped and adding one it has = %d: %v", code, got)
	}
}

// A team that tracks no project offers no status or issue type, so every one
// is refused without asking Jira.
func TestEventHandlerJiraCatalog_NoTrackedProject(t *testing.T) {
	r := newJiraCatalogRig(t)
	r.track(t)
	calls := r.fake.Calls()
	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{
		"status_id_in":     []string{statusToDoID},
		"issue_type_id_in": []string{"10004"},
	})
	wantFieldFaults(t, code, got, fieldFault{"status_id_in", statusToDoID}, fieldFault{"issue_type_id_in", "10004"})
	if n := r.fake.Calls(); n != calls {
		t.Errorf("reached Jira %d times, want none", n-calls)
	}
}

// A field fault already found is reported over a later catalog Jira could not
// read: it is the caller's to fix whatever Jira says.
func TestEventHandlerJiraCatalog_FaultsFoundFirstWinOverAnUpstreamFailure(t *testing.T) {
	r := newJiraCatalogRig(t)
	r.fake.SetPrioritiesFailing()

	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{
		"status_id_in":   []string{"In Progress"},
		"priority_id_in": []string{"2"},
	})
	wantFieldFaults(t, code, got, fieldFault{"status_id_in", "In Progress"})

	code, got = r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{
		"status_id_in":   []string{statusToDoID},
		"priority_id_in": []string{"2"},
	})
	if code != http.StatusBadGateway {
		t.Errorf("priorities unreadable and nothing found = %d, want 502: %v", code, got)
	}
}

// A write that adds an id to an org whose Jira credential is gone is a 409:
// there is nothing to check it against until Jira is connected again.
func TestEventHandlerJiraCatalog_UnconnectedJira(t *testing.T) {
	r := newJiraCatalogRig(t)
	code, created := r.createRule(t, domain.EventJiraIssueAssigned, nil)
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	unconfigureEventSources(t)
	code, got := r.patch(t, created["id"].(string), map[string]any{"scope_predicate": map[string]any{"priority_id_in": []string{"2"}}})
	if code != http.StatusConflict {
		t.Errorf("= %d, want 409: %v", code, got)
	}
}

// The body's own 422s come back in the same answer as the catalog's faults,
// and win over a Jira that is not connected, with the reference check's,
// since they are the caller's to fix whatever Jira would say.
func TestEventHandlerJiraCatalog_AnsweredWithTheBodysFaults(t *testing.T) {
	r := newJiraCatalogRig(t)
	code, created := r.createRule(t, domain.EventJiraIssueAssigned, nil)
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	fields := func(got map[string]any) []string {
		items, _ := got["errors"].([]any)
		var out []string
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			out = append(out, fmt.Sprint(item["field"]))
		}
		return out
	}

	code, got := r.patch(t, id, map[string]any{
		"default_priority": 2,
		"scope_predicate":  map[string]any{"status_id_in": []string{"In Progress"}},
	})
	if want := []string{"default_priority", "scope_predicate.status_id_in"}; code != http.StatusUnprocessableEntity || !slices.Equal(fields(got), want) {
		t.Errorf("= %d %v, want 422 on %v", code, got, want)
	}

	unconfigureEventSources(t)
	code, got = r.patch(t, id, map[string]any{
		"default_priority": 2,
		"scope_predicate":  map[string]any{"priority_id_in": []string{"2"}, "jira_project_id_in": []string{r.ops}},
	})
	if want := []string{"default_priority", "scope_predicate.jira_project_id_in"}; code != http.StatusUnprocessableEntity || !slices.Equal(fields(got), want) {
		t.Errorf("unconnected Jira = %d %v, want 422 on %v", code, got, want)
	}
}

// A rule for a source an admin turned off is refused on its event_type
// without asking Jira about its predicate's ids.
func TestEventHandlerJiraCatalog_TurnedOffSourceIsNotAsked(t *testing.T) {
	r := newJiraCatalogRig(t)
	if code, got := r.do(t, http.MethodPatch, orgSourcePath("jira"), map[string]any{"disabled": true}); code != http.StatusOK {
		t.Fatalf("turn jira off: %d: %v", code, got)
	}
	calls := r.fake.Calls()
	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{"status_id_in": []string{"In Progress"}})
	items, _ := got["errors"].([]any)
	if code != http.StatusUnprocessableEntity || len(items) != 1 || items[0].(map[string]any)["field"] != "event_type" {
		t.Errorf("= %d %v, want one 422 on event_type", code, got)
	}
	if n := r.fake.Calls(); n != calls {
		t.Errorf("reached Jira %d times, want none", n-calls)
	}
}

// A trigger's predicate is checked like a rule's.
func TestEventHandlerJiraCatalog_TriggerCreateIsChecked(t *testing.T) {
	r := newJiraCatalogRig(t)
	seedBlueprintForTrigger(t, r.s, "bp-jira-catalog")
	code, got := r.do(t, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":      domain.EventJiraIssueAssigned,
		"blueprint_id":    "bp-jira-catalog",
		"scope_predicate": map[string]any{"status_id_in": []string{"In Progress"}},
	})
	wantFieldFaults(t, code, got, fieldFault{"status_id_in", "In Progress"})
}
