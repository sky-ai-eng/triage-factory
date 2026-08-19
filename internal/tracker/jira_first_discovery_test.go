package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRefreshJira_FirstDiscoveryAssignedToCurrentUserEmitsAssignment pins the
// first-sight path from TFAC-852. An issue assigned to somebody else matches
// neither discovery query; after it is assigned to the polling credential, its
// arrival through assignee=currentUser() is itself the assignment transition.
func TestRefreshJira_FirstDiscoveryAssignedToCurrentUserEmitsAssignment(t *testing.T) {
	const searchResp = `{"issues":[
		{"key":"SKY-852","fields":{
			"summary":"Newly assigned issue","status":{"name":"In Progress"},
			"assignee":{"displayName":"Alice","accountId":"acc-1"},
			"priority":{"name":"High"},"issuetype":{"name":"Task"},
			"created":"2026-08-19T00:00:00.000+0000","updated":"2026-08-19T00:01:00.000+0000"
		}}
	],"total":1}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(searchResp))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}}

	emitted, err := tr.RefreshJira(ctx, client, srv.URL, projects)
	if err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("events emitted = %d, want 1", emitted)
	}

	events := pub.nonSystemEvents()
	if len(events) != 1 || events[0].EventType != domain.EventJiraIssueAssigned {
		t.Fatalf("first discovery events = %v, want [%s]", eventTypes(events), domain.EventJiraIssueAssigned)
	}
	if events[0].OrgID != org {
		t.Errorf("event org = %q, want %q", events[0].OrgID, org)
	}

	entity, err := stores.Entities.GetBySource(ctx, org, "jira", "SKY-852")
	if err != nil || entity == nil {
		t.Fatalf("GetBySource: entity=%v err=%v", entity, err)
	}
	if !strings.Contains(entity.SnapshotJSON, `"assignee_account_id":"acc-1"`) {
		t.Errorf("snapshot was not seeded with the assignment: %s", entity.SnapshotJSON)
	}

	queued, err := stores.EventQueue.ListForEntity(ctx, org, entity.ID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(queued) != 1 || queued[0].EventType != domain.EventJiraIssueAssigned {
		t.Fatalf("queued events = %v, want one %s", queued, domain.EventJiraIssueAssigned)
	}
	if queued[0].EventID != events[0].ID {
		t.Errorf("queued event id = %q, bus event id = %q", queued[0].EventID, events[0].ID)
	}

	// The persisted snapshot is the re-emit fence: rediscovery on the next
	// cycle must not create a second assignment event.
	secondPub := &recordingPublisher{}
	second := New(database, secondPub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	emitted, err = second.RefreshJira(ctx, client, srv.URL, projects)
	if err != nil {
		t.Fatalf("RefreshJira cycle 2: %v", err)
	}
	if emitted != 0 || len(secondPub.nonSystemEvents()) != 0 {
		t.Fatalf("cycle 2 re-emitted assignment: count=%d events=%v", emitted, eventTypes(secondPub.nonSystemEvents()))
	}
}
