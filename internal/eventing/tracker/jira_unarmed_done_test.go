package tracker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An issue in a project no rule arms is refreshed every cycle and has no done
// statuses of its own project to finish by. These tests cover the fallback:
// Jira's done status category finishes it, through the paths an armed
// project's done status takes.

// jiraCategoryPage renders a one-issue search response for SKY-1 whose status
// carries Jira's status category object, numeric id and all, the way Cloud and
// Data Center return it.
func jiraCategoryPage(statusID, status, category, updated string) string {
	return `{"issues":[
		{"id":"10001","key":"SKY-1","fields":{
			"summary":"Touched issue",
			"status":{"id":"` + statusID + `","name":"` + status + `","statusCategory":{"id":3,"key":"` + category + `","colorName":"green","name":"Whatever"}},
			"assignee":{"displayName":"Alice","accountId":"acc-1"},
			"created":"2026-06-01T00:00:00.000+0000","updated":"` + updated + `"
		}}
	]}`
}

// storedJiraEntity reads back SKY-1's entity and its stored snapshot.
func storedJiraEntity(t *testing.T, stores db.Stores) (*domain.Entity, domain.JiraSnapshot) {
	t.Helper()
	ent, err := stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "jira", "https://jira.example.com", "SKY-1")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	var snap domain.JiraSnapshot
	if ent.SnapshotJSON != "" {
		if err := json.Unmarshal([]byte(ent.SnapshotJSON), &snap); err != nil {
			t.Fatalf("unmarshal stored snapshot %q: %v", ent.SnapshotJSON, err)
		}
	}
	return ent, snap
}

// TestRefreshJira_UnarmedIssueCompletesByStatusCategory: an issue in a project
// no rule arms that moves into a status Jira files under its done category
// emits jira:issue:completed beside status_changed, the event the router
// closes the entity and its tasks on, and the snapshot records the category.
func TestRefreshJira_UnarmedIssueCompletesByStatusCategory(t *testing.T) {
	srv := jiraPageServer(t, []string{
		jiraCategoryPage("3", "In Progress", "indeterminate", "2026-06-10T09:00:00.000+0000"),
		jiraCategoryPage("10002", "Closed", "done", "2026-06-10T10:00:00.000+0000"),
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := tr.RefreshJira(context.Background(), "https://jira.example.com", client, srv.URL, nil); err != nil {
			t.Fatalf("RefreshJira cycle %d: %v", cycle, err)
		}
		if cycle == 1 {
			if evts := pub.nonSystemEvents(); len(evts) != 0 {
				t.Fatalf("cycle 1 (quiet seed) emitted %v, want none", eventTypes(evts))
			}
			if ent, _ := storedJiraEntity(t, stores); ent.State != "active" {
				t.Fatalf("after cycle 1 the entity is %q, want active: its issue is in progress", ent.State)
			}
		}
	}

	got := eventTypes(pub.nonSystemEvents())
	want := []string{domain.EventJiraIssueStatusChanged, domain.EventJiraIssueCompleted}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cycle 2 emitted %v, want %v", got, want)
	}
	if _, snap := storedJiraEntity(t, stores); snap.StatusCategory != domain.JiraStatusCategoryDone {
		t.Errorf("stored status_category = %q, want %q", snap.StatusCategory, domain.JiraStatusCategoryDone)
	}
}

// TestRefreshJira_UnarmedStubInDoneCategoryClosesQuietly: an issue an agent
// touched after it was already finished is seeded closed on its first
// refresh, emitting nothing, as a stub in an armed project's done status is.
func TestRefreshJira_UnarmedStubInDoneCategoryClosesQuietly(t *testing.T) {
	srv := jiraPageServer(t, []string{
		jiraCategoryPage("10002", "Closed", "done", "2026-06-10T10:00:00.000+0000"),
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

	if _, err := tr.RefreshJira(context.Background(), "https://jira.example.com", client, srv.URL, nil); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if ent, _ := storedJiraEntity(t, stores); ent.State != "closed" {
		t.Errorf("entity state = %q, want closed", ent.State)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("quiet seed emitted %v, want none", eventTypes(evts))
	}
}

// TestRefreshJira_UnarmedDoneSnapshotWithoutCategoryIsOwedItsClose: an entity
// already sitting in a finished status when its snapshot first gains the
// category (one stored before the field was recorded) has no transition left
// to diff. The refresh that records the category emits nothing, and the next
// one finds the entity active with a terminal snapshot and owes its close.
func TestRefreshJira_UnarmedDoneSnapshotWithoutCategoryIsOwedItsClose(t *testing.T) {
	const updated = "2026-06-10T10:00:00.000+0000"
	srv := jiraPageServer(t, []string{
		jiraCategoryPage("10002", "Closed", "done", updated),
		jiraCategoryPage("10002", "Closed", "done", updated),
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

	ent, _ := storedJiraEntity(t, stores)
	prior := `{"id":"10001","key":"SKY-1","summary":"Touched issue","status":"Closed","status_id":"10002","assignee":"Alice","assignee_account_id":"acc-1","updated_at":"` + updated + `"}`
	if ok, err := stores.Entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, ent.ID, prior, ent.PollSeq); err != nil || !ok {
		t.Fatalf("seed a snapshot without the category: ok=%v err=%v", ok, err)
	}

	if _, err := tr.RefreshJira(context.Background(), "https://jira.example.com", client, srv.URL, nil); err != nil {
		t.Fatalf("RefreshJira cycle 1: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("cycle 1 emitted %v, want none: the status did not change", eventTypes(evts))
	}
	if _, snap := storedJiraEntity(t, stores); snap.StatusCategory != domain.JiraStatusCategoryDone {
		t.Fatalf("after cycle 1 status_category = %q, want it recorded", snap.StatusCategory)
	}

	if _, err := tr.RefreshJira(context.Background(), "https://jira.example.com", client, srv.URL, nil); err != nil {
		t.Fatalf("RefreshJira cycle 2: %v", err)
	}
	if got := eventTypes(pub.nonSystemEvents()); len(got) != 1 || got[0] != domain.EventSystemEntityCloseOwed {
		t.Errorf("cycle 2 emitted %v, want [%s]", got, domain.EventSystemEntityCloseOwed)
	}
}

// TestRefreshJira_ArmedRuleOutranksStatusCategory: in an armed project the
// team's done statuses alone decide. A status Jira files under its done
// category that the rule does not name is a status change, not a completion.
func TestRefreshJira_ArmedRuleOutranksStatusCategory(t *testing.T) {
	srv := jiraPageServer(t, []string{
		jiraCategoryPage("3", "In Progress", "indeterminate", "2026-06-10T09:00:00.000+0000"),
		jiraCategoryPage("10003", "Won't Do", "done", "2026-06-10T10:00:00.000+0000"),
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	ent, _ := storedJiraEntity(t, stores)
	// The rule is on the project the fixture's entity references. It carries
	// no key, so discovery has no project to ask about and every search the
	// server sees is a refresh; its done set is all the terminal test reads.
	rules := JiraRules{{JiraProjectID: ent.JiraProjectID, DoneMembers: []domain.JiraStatusRef{{ID: "10002", Name: "Shipped"}}}}

	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := tr.RefreshJira(context.Background(), "https://jira.example.com", client, srv.URL, rules); err != nil {
			t.Fatalf("RefreshJira cycle %d: %v", cycle, err)
		}
	}
	if got := eventTypes(pub.nonSystemEvents()); len(got) != 1 || got[0] != domain.EventJiraIssueStatusChanged {
		t.Errorf("emitted %v, want [%s] alone", got, domain.EventJiraIssueStatusChanged)
	}
	if ent, _ := storedJiraEntity(t, stores); ent.State != "active" {
		t.Errorf("entity state = %q, want active", ent.State)
	}
}
