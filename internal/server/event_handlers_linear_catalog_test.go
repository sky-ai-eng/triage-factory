package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
)

// A workflow state of a tracked Linear team, a label an issue in one can
// carry and a label group of one are accepted and stored as sent.
func TestEventHandlerLinearCatalog_AcceptsIDsTheEditorOffers(t *testing.T) {
	s := newLinearTeamInServer(t, linearTeamEng)
	code, got := createLinearRule(t, s, domain.EventLinearIssueLabelAdded, map[string]any{
		"label_id_in":       []string{linearLabelHard},
		"label_group_id_in": []string{linearLabelDifficulty},
		"has_label_id_in":   []string{linearLabelBug},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	code, got = createLinearRule(t, s, domain.EventLinearIssueStatusChanged, map[string]any{
		"new_status_id_in": []string{linearStateReview},
		"old_status_id_in": []string{linearStateDoing},
	})
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, got)
	}
	want := fmt.Sprintf(`{"new_status_id_in":[%q],"old_status_id_in":[%q]}`, linearStateReview, linearStateDoing)
	if got["scope_predicate_json"] != want {
		t.Errorf("stored predicate = %v, want %s", got["scope_predicate_json"], want)
	}
}

// A state name, a state only an untracked team's workflow has, a label name, a
// group given as a label and a label given as a group are each refused, every
// one reported, and nothing is written.
func TestEventHandlerLinearCatalog_RefusesNamesAndUntrackedIDs(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng, linearFixtureOps)
	trackLinearTeams(t, s, linearTeamEng)
	opsOnly := fixtureUUID("linear-state-ops-only")
	fake.SetStates(linearTeamOps, linear.WorkflowState{ID: opsOnly, Name: "Queued", Type: "unstarted"})
	before := len(listEventHandlers(t, s, "rule"))

	code, got := createLinearRule(t, s, domain.EventLinearIssueAssigned, map[string]any{
		"status_id_in":    []string{linearStateTodo, "Todo", opsOnly},
		"has_label_id_in": []string{"Bug", linearLabelDifficulty},
	})
	messages := repoInFaultMessages(t, code, got)
	want := []struct{ field, value string }{
		{"status_id_in", "Todo"},
		{"status_id_in", opsOnly},
		{"has_label_id_in", "Bug"},
		{"has_label_id_in", linearLabelDifficulty},
	}
	if len(messages) != len(want) {
		t.Fatalf("faults = %q, want %d", messages, len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(messages[i], w.field+": ") || !strings.Contains(messages[i], fmt.Sprintf("%q", w.value)) {
			t.Errorf("fault %d = %q, want it to name %s and %q", i, messages[i], w.field, w.value)
		}
	}

	code, got = createLinearRule(t, s, domain.EventLinearIssueLabelRemoved, map[string]any{
		"label_group_id_in": []string{linearLabelHard, "Difficulty"},
	})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 2 {
		t.Errorf("faults = %q, want one each for a label given as a group and a group name", messages)
	}
	if after := len(listEventHandlers(t, s, "rule")); after != before {
		t.Errorf("a refused create changed the rule count from %d to %d", before, after)
	}
}

// A write that adds no id asks Linear nothing, so it succeeds while Linear is
// down; one that adds an id while Linear is down stores nothing.
func TestEventHandlerLinearCatalog_AsksLinearOnlyAboutAddedIDs(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	trackLinearTeams(t, s, linearTeamEng)
	code, created := createLinearRule(t, s, domain.EventLinearIssueAssigned, map[string]any{"status_id_in": []string{linearStateTodo}})
	if code != http.StatusCreated {
		t.Fatalf("seed rule: %d: %v", code, created)
	}
	id := created["id"].(string)

	fake.SetFailing(true)
	calls := fake.Calls()
	if rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("name-only PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	if n := fake.Calls(); n != calls {
		t.Errorf("an edit that adds no id reached Linear %d times", n-calls)
	}
	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"scope_predicate": map[string]any{"status_id_in": []string{linearStateTodo, linearStateDoing}},
	})
	if rec.Code != http.StatusBadGateway {
		t.Errorf("PATCH adding a state while Linear is down = %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

// A team that tracks no Linear team offers no state or label, so every one is
// refused without asking Linear.
func TestEventHandlerLinearCatalog_NoTrackedTeam(t *testing.T) {
	s, fake := newServerWithLinearCatalog(t, linearFixtureEng)
	code, got := createLinearRule(t, s, domain.EventLinearIssueBodyUpdated, map[string]any{"has_label_id_in": []string{linearLabelBug}})
	if messages := repoInFaultMessages(t, code, got); len(messages) != 1 {
		t.Errorf("faults = %q, want one", messages)
	}
	if fake.Calls() != 0 {
		t.Errorf("reached Linear %d times, want none", fake.Calls())
	}
}
