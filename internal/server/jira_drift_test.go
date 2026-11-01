package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A Jira rules save decides status drift for each project whose workflow it read:
// a changed rule is resolved against it, a carried one is not, a dropped project
// names nothing, and a save that reads nothing leaves it to the drift check.
func TestJiraProjectsPut_DecidesStatusDriftFromTheWorkflowItRead(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	s, fake := newServerWithJiraCatalog(t, "SKY")
	if res := putJiraProjects(t, s, "SKY"); res.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d; body=%s", res.Code, res.Body.String())
	}
	stored, _ := teamJiraRules(t, s)
	if len(stored) != 1 {
		t.Fatalf("rules = %+v, want SKY", stored)
	}
	row := stored[0].JiraProjectID
	key := stored[0].TeamID + ":" + row
	rec := notifytest.Record(t)
	put := func(elements ...map[string]any) {
		t.Helper()
		if elements == nil {
			elements = []map[string]any{}
		}
		if res := putJiraProjectElements(t, s, elements...); res.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", res.Code, res.Body.String())
		}
	}

	// In Progress leaves SKY's workflow.
	fake.SetStatuses("SKY",
		fakeJiraStatus{ID: statusToDoID, Name: "To Do"},
		fakeJiraStatus{ID: statusInReviewID, Name: "Code Review"},
		fakeJiraStatus{ID: statusDoneID, Name: "Done"})

	// Nothing changed, so nothing was read.
	put(map[string]any{"id": row})
	rec.Inactive(t, org, kinds.JiraStatusDrift)

	// The pickup change reads the workflow, and the in-progress rule the save
	// carried still names the status that left it.
	put(map[string]any{"id": row, "pickup": map[string]any{"member_ids": []string{statusToDoID, statusInReviewID}}})
	rec.Active(t, org, kinds.JiraStatusDrift, key)
	if open := rec.Open(org, kinds.JiraStatusDrift); len(open) == 1 {
		got := open[0].Params.(kinds.JiraStatusRule)
		if got.ProjectKey != "SKY" || len(got.Statuses) != 1 || got.Statuses[0] != "In Progress" || !got.Stopped {
			t.Errorf("params = %+v, want In Progress gone, stopping the move into it", got)
		}
	}

	put(map[string]any{"id": row, "in_progress": map[string]any{
		"member_ids": []string{statusInReviewID}, "canonical_id": statusInReviewID,
	}})
	rec.Inactive(t, org, kinds.JiraStatusDrift)

	kinds.JiraStatusDrift.Set(t.Context(), org, kinds.JiraStatusRule{TeamID: stored[0].TeamID, JiraProjectID: row}, true)
	put()
	rec.Inactive(t, org, kinds.JiraStatusDrift)
}

// A task rule save that changes its filter or enabled state checks drift at once:
// enabling one naming a status no tracked project has raises it, removing the
// status resolves it, and disabling or deleting resolves it without asking Jira.
func TestEventHandlerSave_ChecksJiraPredicateDrift(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	r := newJiraCatalogRig(t)
	r.track(t, r.sky, r.ops)
	code, created := r.createRule(t, domain.EventJiraIssueAssigned, map[string]any{
		"status_id_in": []string{opsOnlyStatusID, statusInProgressID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d: %v", code, created)
	}
	id := created["id"].(string)
	// The team stops tracking OPS, whose status the filter names.
	r.track(t, r.sky)
	rec := notifytest.Record(t)

	if code, got := r.patch(t, id, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable = %d: %v", code, got)
	}
	rec.Inactive(t, org, kinds.JiraPredicateDrift)

	if code, got := r.patch(t, id, map[string]any{"enabled": true}); code != http.StatusOK {
		t.Fatalf("enable = %d: %v", code, got)
	}
	rec.Active(t, org, kinds.JiraPredicateDrift, id)
	if open := rec.Open(org, kinds.JiraPredicateDrift); len(open) == 1 {
		got := open[0].Params.(kinds.JiraHandlerFilter)
		if got.HandlerKind != domain.EventHandlerKindRule || got.RuleName != "scoped" ||
			len(got.Statuses) != 1 || got.Statuses[0] != opsOnlyStatusID || got.Stopped {
			t.Errorf("params = %+v, want the OPS status gone and the rule still matching In Progress", got)
		}
	}

	if code, got := r.patch(t, id, map[string]any{"scope_predicate": map[string]any{
		"status_id_in": []string{statusInProgressID},
	}}); code != http.StatusOK {
		t.Fatalf("fix = %d: %v", code, got)
	}
	rec.Inactive(t, org, kinds.JiraPredicateDrift)

	kinds.JiraPredicateDrift.Set(t.Context(), org, kinds.JiraHandlerFilter{HandlerID: id}, true)
	r.fake.SetFailing(true)
	if code, got := r.do(t, http.MethodDelete, "/api/event-handlers/"+id, nil); code != http.StatusOK {
		t.Fatalf("delete = %d: %v", code, got)
	}
	rec.Inactive(t, org, kinds.JiraPredicateDrift)
}
