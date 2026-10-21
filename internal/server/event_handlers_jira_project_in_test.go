package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// jiraProjectInRig is a test server whose org has Jira configured, with the
// SKY and OPS projects in the registry on the org's Jira site.
type jiraProjectInRig struct {
	s        *Server
	site     string
	sky, ops string // registry row ids
}

func newJiraProjectInRig(t *testing.T) *jiraProjectInRig {
	t.Helper()
	s := newTestServer(t)
	configureEventSources(t, s)
	orgSet, err := s.allStores.Orgs.GetSettingsSystem(t.Context(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("GetSettingsSystem: %v", err)
	}
	r := &jiraProjectInRig{s: s, site: domain.EntityScope("jira", orgSet)}
	r.sky = r.registerProject(t, r.site, "10000", "SKY")
	r.ops = r.registerProject(t, r.site, "10001", "OPS")
	return r
}

// registerProject adds a project to the registry on site and returns its row
// id. Registered is not tracked: the registry holds every project TF has seen.
func (r *jiraProjectInRig) registerProject(t *testing.T, site, externalID, key string) string {
	t.Helper()
	p, err := r.s.allStores.JiraProjects.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, site, externalID, key, key+" Project")
	if err != nil {
		t.Fatalf("register %s: %v", key, err)
	}
	return p.ID
}

// track makes ids the local team's whole tracked set on the rig's site.
func (r *jiraProjectInRig) track(t *testing.T, ids ...string) {
	t.Helper()
	rules := make([]domain.JiraProjectStatusRules, 0, len(ids))
	for _, id := range ids {
		rules = append(rules, domain.JiraProjectStatusRules{JiraProjectID: id})
	}
	if err := r.s.allStores.JiraStatusRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, r.site, rules); err != nil {
		t.Fatalf("track jira projects: %v", err)
	}
}

// createRule creates a rule of eventType with predicate and returns the
// response.
func (r *jiraProjectInRig) createRule(t *testing.T, eventType string, predicate map[string]any) (int, map[string]any) {
	t.Helper()
	rec := doJSON(t, r.s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      eventType,
		"name":            "scoped",
		"scope_predicate": predicate,
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

func TestEventHandlerJiraProjectIn_CreateAcceptsTrackedProjects(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky, r.ops)

	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{"jira_project_id_in": []string{r.sky, r.ops}})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := fmt.Sprintf(`{"jira_project_id_in":[%q,%q]}`, r.sky, r.ops)
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}
}

// A registry row the team does not track, a project key, and an id nothing
// answers to are each refused, every one reported, and nothing is written.
func TestEventHandlerJiraProjectIn_CreateRefusesWhatTheTeamDoesNotTrack(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)
	const unknownID = "00000000-0000-0000-0000-0000000000ff"
	before := len(listEventHandlers(t, r.s, "rule"))

	code, got := r.createRule(t, domain.EventJiraIssueStatusChanged, map[string]any{
		"jira_project_id_in": []string{r.sky, r.ops, "SKY", unknownID},
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 3 {
		t.Fatalf("faults = %q, want one each for the untracked row, the key and the unknown id", messages)
	}
	for i, want := range []string{r.ops, "SKY", unknownID} {
		if !strings.HasPrefix(messages[i], "jira_project_id_in: ") || !strings.Contains(messages[i], fmt.Sprintf("%q", want)) {
			t.Errorf("fault %d = %q, want it to name jira_project_id_in and %q", i, messages[i], want)
		}
	}
	if after := len(listEventHandlers(t, r.s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}
}

// body_updated's schema lives apart from the other Jira events' and carries
// the field too, so it is checked like them.
func TestEventHandlerJiraProjectIn_BodyUpdatedIsChecked(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)

	code, got := r.createRule(t, domain.EventJiraIssueBodyUpdated, map[string]any{"jira_project_id_in": []string{r.ops}})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
}

// key_changed's filter on the project the issue left is checked too, and its
// fault names that field.
func TestEventHandlerJiraProjectIn_KeyChangedChecksBothFilters(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)

	code, got := r.createRule(t, domain.EventJiraIssueKeyChanged, map[string]any{
		"jira_project_id_in":     []string{r.sky},
		"old_jira_project_id_in": []string{r.ops},
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "old_jira_project_id_in: ") || !strings.Contains(messages[0], r.ops) {
		t.Fatalf("faults = %q, want one naming old_jira_project_id_in and the untracked project", messages)
	}

	code, got = r.createRule(t, domain.EventJiraIssueKeyChanged, map[string]any{"old_jira_project_id_in": []string{r.sky}})
	if code != http.StatusCreated {
		t.Fatalf("a tracked project the issue left: status = %d, want 201: %v", code, got)
	}
}

// Untracking a project must not block an edit that leaves it in place; an
// entry the edit adds is still checked, and a refused edit changes nothing.
func TestEventHandlerJiraProjectIn_PatchChecksOnlyAddedProjects(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)
	code, created := r.createRule(t, domain.EventJiraIssueCommented, map[string]any{"jira_project_id_in": []string{r.sky}})
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	r.track(t, r.ops)

	if rec := doJSON(t, r.s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("name-only PATCH: %d: %s", rec.Code, rec.Body.String())
	}
	kept := fmt.Sprintf(`{"jira_project_id_in":[%q,%q]}`, r.sky, r.ops)
	if rec := doJSON(t, r.s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"jira_project_id_in": []string{r.sky, r.ops}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH keeping an untracked project and adding a tracked one: %d: %s", rec.Code, rec.Body.String())
	}

	other := r.registerProject(t, r.site, "10002", "OTHER")
	rec := doJSON(t, r.s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"jira_project_id_in": []string{r.sky, other}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 || !strings.Contains(messages[0], other) {
		t.Errorf("faults = %q, want one naming the added untracked row", messages)
	}
	stored := doJSON(t, r.s, http.MethodGet, "/api/event-handlers/"+id, nil)
	var row map[string]any
	_ = json.Unmarshal(stored.Body.Bytes(), &row)
	if row["scope_predicate_json"] != kept {
		t.Errorf("refused PATCH changed the predicate: %v, want %s", row["scope_predicate_json"], kept)
	}
}

func TestEventHandlerJiraProjectIn_CreateTriggerRefusesUntrackedProjects(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)
	seedBlueprintForTrigger(t, r.s, "p-jira-project-in")

	rec := doJSON(t, r.s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":      domain.EventJiraIssueAssigned,
		"blueprint_id":    "p-jira-project-in",
		"scope_predicate": map[string]any{"jira_project_id_in": []string{r.ops}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
}

// Rules for projects on the org's previous Jira site are not the team's
// tracked set, so after the org moves sites their ids are refused like any
// other.
func TestEventHandlerJiraProjectIn_OnlyTheCurrentSiteCounts(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)
	seedSourceHost(t, r.s, eventsource.KindJira, "https://jira-other.example.com")

	code, got := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{"jira_project_id_in": []string{r.sky}})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one for the project tracked on the previous site", messages)
	}
}

// The key fields are gone from the predicate schema, so a caller still
// sending one is told so rather than having it dropped.
func TestEventHandlerJiraProjectIn_RefusesProjectKey(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)

	for _, field := range []string{"project", "old_project"} {
		code, got := r.createRule(t, domain.EventJiraIssueKeyChanged, map[string]any{field: "SKY"})
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %v", field, code, got)
		}
	}
}

// The team-settings read names each tracked project by the registry id
// jira_project_id_in takes, beside its key and name, and keeps naming it by
// that id after the project's key is renamed.
func TestTeamSettingsGet_NamesJiraProjectsByRegistryID(t *testing.T) {
	r := newJiraProjectInRig(t)
	r.track(t, r.sky)

	read := func() []jiraProjectSettings {
		t.Helper()
		rec := doJSON(t, r.s, http.MethodGet, teamSettingsPath("default"), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET team settings = %d; body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			JiraProjects []jiraProjectSettings `json:"jira_projects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.JiraProjects
	}

	if got := read(); len(got) != 1 || got[0].ID != r.sky || got[0].Key != "SKY" || got[0].Name != "SKY Project" {
		t.Fatalf("jira_projects = %+v, want SKY under its registry id", got)
	}
	if _, err := r.s.allStores.JiraProjects.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, r.site, "10000", "SKYNEW", "Sky renamed"); err != nil {
		t.Fatalf("rename SKY: %v", err)
	}
	if got := read(); len(got) != 1 || got[0].ID != r.sky || got[0].Key != "SKYNEW" {
		t.Errorf("jira_projects after the rename = %+v, want the same id under the new key", got)
	}
}

// The check runs inside the write's transaction under the caller's claims, so
// a plain team member, with no admin role anywhere, must be able to read the
// org's Jira site, the team's rules and the registry rows they reference
// through RLS. A policy that hid any of them would refuse every entry that
// member writes.
func TestJiraProjectInFaults_PlainMemberUnderRLS_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)
	const jiraBase = "https://jira.example.com"
	var site string
	if err := rig.s.tx.WithTx(t.Context(), rig.orgID, rig.orgOwner, func(tx db.TxStores) error {
		cur, err := tx.Orgs.GetSettings(t.Context(), rig.orgID)
		if err != nil {
			return err
		}
		cur.JiraBaseURL = jiraBase
		stored, err := tx.Orgs.UpdateSettingsVersioned(t.Context(), rig.orgID, cur, cur.Version)
		site = domain.EntityScope("jira", stored)
		return err
	}); err != nil {
		t.Fatalf("set jira site: %v", err)
	}
	register := func(externalID, key string) string {
		t.Helper()
		p, err := rig.s.allStores.JiraProjects.FindOrCreateSystem(t.Context(), rig.orgID, site, externalID, key, key)
		if err != nil {
			t.Fatalf("register %s: %v", key, err)
		}
		return p.ID
	}
	sky, ops := register("10000", "SKY"), register("10001", "OPS")
	track := func(actingUser, teamID, projectID string) {
		t.Helper()
		if err := rig.s.tx.WithTx(t.Context(), rig.orgID, actingUser, func(tx db.TxStores) error {
			return tx.JiraStatusRules.ReplaceForTeam(t.Context(), teamID, site, []domain.JiraProjectStatusRules{{JiraProjectID: projectID}})
		}); err != nil {
			t.Fatalf("track %s for %s: %v", projectID, teamID, err)
		}
	}
	track(rig.orgOwner, rig.teamA, sky)
	track(rig.memberB, rig.teamB, ops)

	faultsFor := func(projectIn ...string) []string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jira_project_id_in": projectIn})
		predicate := string(b)
		var faults []string
		if err := rig.s.tx.WithTx(t.Context(), rig.orgID, rig.plainB, func(tx db.TxStores) error {
			var e error
			faults, e = jiraProjectInFaults(t.Context(), tx, rig.orgID, rig.teamB, &predicate, nil)
			return e
		}); err != nil {
			t.Fatalf("jiraProjectInFaults(%v): %v", projectIn, err)
		}
		return faults
	}

	if faults := faultsFor(ops); len(faults) != 0 {
		t.Errorf("teamB's own project: faults = %v, want none", faults)
	}
	if faults := faultsFor(sky); len(faults) != 1 {
		t.Errorf("teamA's project: faults = %v, want one", faults)
	}
}

// Every Jira event type registers the check, so none of them stores a
// reference the team cannot match on.
func TestJiraProjectInFaults_RegisteredForEveryJiraType(t *testing.T) {
	predicateReferenceMu.RLock()
	defer predicateReferenceMu.RUnlock()
	n := 0
	for eventType := range predicateReferenceChecks {
		if strings.HasPrefix(eventType, "jira:") {
			n++
		}
	}
	if n != 12 {
		t.Errorf("registered checks for %d jira event types, want all 12", n)
	}
	for _, eventType := range []string{domain.EventJiraIssueKeyChanged, domain.EventJiraIssueBodyUpdated} {
		if _, ok := predicateReferenceChecks[eventType]; !ok {
			t.Errorf("no check registered for %s", eventType)
		}
	}
}
