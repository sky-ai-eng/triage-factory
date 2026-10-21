package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newLinearTeamInServer is a test server whose org holds a Linear credential
// and whose local team tracks the named Linear teams.
func newLinearTeamInServer(t *testing.T, tracked ...string) *Server {
	t.Helper()
	s, _ := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	trackLinearTeams(t, s, tracked...)
	return s
}

// trackLinearTeams makes ids the local team's whole tracked set in the test
// workspace.
func trackLinearTeams(t *testing.T, s *Server, ids ...string) {
	t.Helper()
	rules := make([]domain.LinearTeamRules, 0, len(ids))
	for _, id := range ids {
		rules = append(rules, domain.LinearTeamRules{LinearTeamID: id, LinearTeamKey: "K" + id[:4], LinearTeamName: id})
	}
	if _, err := s.allStores.LinearTeamRules.ReplaceForTeam(t.Context(), runmode.LocalDefaultTeamID, linearTestWorkspaceID, rules); err != nil {
		t.Fatalf("track linear teams: %v", err)
	}
}

// createLinearRule creates a rule of eventType with predicate and returns the
// response.
func createLinearRule(t *testing.T, s *Server, eventType string, predicate map[string]any) (int, map[string]any) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      eventType,
		"name":            "scoped",
		"scope_predicate": predicate,
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

func TestEventHandlerLinearTeamIn_CreateAcceptsTrackedTeams(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng, linearTeamOps)

	code, got := createLinearRule(t, s, domain.EventLinearIssueAssigned, map[string]any{"linear_team_id_in": []string{linearTeamEng, linearTeamOps}})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := fmt.Sprintf(`{"linear_team_id_in":[%q,%q]}`, linearTeamEng, linearTeamOps)
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}
}

// A Linear team the team does not track, a team key, and an id nothing
// answers to are each refused, every one reported, and nothing is written.
func TestEventHandlerLinearTeamIn_CreateRefusesWhatTheTeamDoesNotTrack(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	before := len(listEventHandlers(t, s, "rule"))

	code, got := createLinearRule(t, s, domain.EventLinearIssueStatusChanged, map[string]any{
		"linear_team_id_in": []string{linearTeamEng, linearTeamOps, "ENG", linearTeamGhost},
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 3 {
		t.Fatalf("faults = %q, want one each for the untracked team, the key and the unknown id", messages)
	}
	for i, want := range []string{linearTeamOps, "ENG", linearTeamGhost} {
		if !strings.HasPrefix(messages[i], "linear_team_id_in: ") || !strings.Contains(messages[i], fmt.Sprintf("%q", want)) {
			t.Errorf("fault %d = %q, want it to name linear_team_id_in and %q", i, messages[i], want)
		}
	}
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}
}

// identifier_changed's filter on the team the issue left is checked too, and
// its fault names that field.
func TestEventHandlerLinearTeamIn_IdentifierChangedChecksBothFilters(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)

	code, got := createLinearRule(t, s, domain.EventLinearIssueIdentifierChanged, map[string]any{
		"linear_team_id_in":     []string{linearTeamEng},
		"old_linear_team_id_in": []string{linearTeamOps},
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "old_linear_team_id_in: ") || !strings.Contains(messages[0], linearTeamOps) {
		t.Fatalf("faults = %q, want one naming old_linear_team_id_in and the untracked team", messages)
	}

	code, got = createLinearRule(t, s, domain.EventLinearIssueIdentifierChanged, map[string]any{
		"old_linear_team_id_in": []string{linearTeamEng},
	})
	if code != http.StatusCreated {
		t.Fatalf("a tracked team the issue left: status = %d, want 201: %v", code, got)
	}
}

// Untracking a Linear team must not block an edit that leaves it in place; an
// entry the edit adds is still checked, and a refused edit changes nothing.
func TestEventHandlerLinearTeamIn_PatchChecksOnlyAddedTeams(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	code, created := createLinearRule(t, s, domain.EventLinearIssueCommented, map[string]any{"linear_team_id_in": []string{linearTeamEng}})
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)
	trackLinearTeams(t, s, linearTeamOps)

	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("name-only PATCH: %d: %s", rec.Code, rec.Body.String())
	}
	kept := fmt.Sprintf(`{"linear_team_id_in":[%q,%q]}`, linearTeamEng, linearTeamOps)
	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"linear_team_id_in": []string{linearTeamEng, linearTeamOps}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH keeping an untracked team and adding a tracked one: %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"linear_team_id_in": []string{linearTeamEng, linearTeamGhost}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 || !strings.Contains(messages[0], linearTeamGhost) {
		t.Errorf("faults = %q, want one naming the added id", messages)
	}
	stored := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	var row map[string]any
	_ = json.Unmarshal(stored.Body.Bytes(), &row)
	if row["scope_predicate_json"] != kept {
		t.Errorf("refused PATCH changed the predicate: %v, want %s", row["scope_predicate_json"], kept)
	}
}

func TestEventHandlerLinearTeamIn_CreateTriggerRefusesUntrackedTeams(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	seedBlueprintForTrigger(t, s, "p-linear-team-in")

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":      domain.EventLinearIssueAssigned,
		"blueprint_id":    "p-linear-team-in",
		"scope_predicate": map[string]any{"linear_team_id_in": []string{linearTeamOps}},
	})
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if messages := repoInFaultMessages(t, rec.Code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
}

// Rules saved under the org's previous Linear workspace are not the team's
// tracked set, so after the credential moves to another workspace their ids
// are refused like any other.
func TestEventHandlerLinearTeamIn_OnlyTheCurrentWorkspaceCounts(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	bindLinearWorkspaceForTest(t, s, "ws-other")

	code, got := createLinearRule(t, s, domain.EventLinearIssueAssigned, map[string]any{"linear_team_id_in": []string{linearTeamEng}})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one for the team tracked under the previous workspace", messages)
	}
}

// The key field is gone from the predicate schema, so a caller still sending
// it is told so rather than having it dropped.
func TestEventHandlerLinearTeamIn_RefusesTeamKey(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)

	for _, field := range []string{"linear_team_key", "old_linear_team_key"} {
		code, got := createLinearRule(t, s, domain.EventLinearIssueIdentifierChanged, map[string]any{field: "ENG"})
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %v", field, code, got)
		}
	}
}

// The check runs inside the write's transaction under the caller's claims, so
// a plain team member, with no admin role anywhere, must be able to read the
// org's Linear workspace and the team's tracked Linear teams through RLS. A
// policy that hid either would refuse every entry that member writes.
func TestLinearTeamInFaults_PlainMemberUnderRLS_Postgres(t *testing.T) {
	rig := newRepoScopeRig(t)
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO org_settings (org_id, linear_workspace_id) VALUES ($1, 'ws-test')
		 ON CONFLICT (org_id) DO UPDATE SET linear_workspace_id = EXCLUDED.linear_workspace_id`, rig.orgID)
	pgtest.MustExec(t, rig.h.AdminDB,
		`INSERT INTO linear_team_rules (team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name)
		 VALUES ($1, 'ws-test', $3, 'ENG', 'Engineering'), ($2, 'ws-test', $4, 'OPS', 'Operations')`,
		rig.teamA, rig.teamB, linearTeamEng, linearTeamOps)

	faultsFor := func(teamIn ...string) []string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"linear_team_id_in": teamIn})
		predicate := string(b)
		var faults []string
		if err := rig.s.tx.WithTx(t.Context(), rig.orgID, rig.plainB, func(tx db.TxStores) error {
			var e error
			faults, e = linearTeamInFaults(t.Context(), tx, rig.orgID, rig.teamB, &predicate, nil)
			return e
		}); err != nil {
			t.Fatalf("linearTeamInFaults(%v): %v", teamIn, err)
		}
		return faults
	}

	if faults := faultsFor(linearTeamOps); len(faults) != 0 {
		t.Errorf("teamB's own Linear team: faults = %v, want none", faults)
	}
	if faults := faultsFor(linearTeamEng); len(faults) != 1 {
		t.Errorf("teamA's Linear team: faults = %v, want one", faults)
	}
}

// parent_changed's new_parent_id takes the parent's UUID: an identifier is
// refused, alongside the team filter's own faults, and a UUID or an empty
// value (a cleared parent) is stored.
func TestEventHandlerLinearParent_RefusesAnIdentifier(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	before := len(listEventHandlers(t, s, "rule"))

	code, got := createLinearRule(t, s, domain.EventLinearIssueParentChanged, map[string]any{
		"linear_team_id_in": []string{linearTeamOps},
		"new_parent_id":     "ENG-12",
	})
	messages := repoInFaultMessages(t, code, got)
	if len(messages) != 2 {
		t.Fatalf("faults = %q, want one for the untracked team and one for the identifier", messages)
	}
	if !strings.HasPrefix(messages[1], "new_parent_id: ") || !strings.Contains(messages[1], `"ENG-12"`) {
		t.Errorf("fault = %q, want it to name new_parent_id and the identifier", messages[1])
	}
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}

	for _, parent := range []string{linearIssueUUID, ""} {
		code, got = createLinearRule(t, s, domain.EventLinearIssueParentChanged, map[string]any{"new_parent_id": parent})
		if code != http.StatusCreated {
			t.Fatalf("new_parent_id %q: status = %d, want 201: %v", parent, code, got)
		}
		if want := fmt.Sprintf(`{"new_parent_id":%q}`, parent); got["scope_predicate_json"] != want {
			t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
		}
	}

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      domain.EventLinearIssueParentChanged,
		"name":            "scoped",
		"scope_predicate": map[string]any{"new_parent": "ENG-12"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("new_parent: status = %d, want 400 for an unknown field: %s", rec.Code, rec.Body.String())
	}
}

// Every Linear event type registers the check, so none of them stores a
// reference the team cannot match on.
func TestLinearTeamInFaults_RegisteredForEveryLinearType(t *testing.T) {
	predicateReferenceMu.RLock()
	defer predicateReferenceMu.RUnlock()
	n := 0
	for eventType := range predicateReferenceChecks {
		if strings.HasPrefix(eventType, "linear:") {
			n++
		}
	}
	if n != 13 {
		t.Errorf("registered checks for %d linear event types, want all 13", n)
	}
	if _, ok := predicateReferenceChecks[domain.EventLinearIssueIdentifierChanged]; !ok {
		t.Errorf("no check registered for %s", domain.EventLinearIssueIdentifierChanged)
	}
}
