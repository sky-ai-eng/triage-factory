package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// jiraGoneFixture stands up a Jira whose searches match nothing — so every
// tracked key goes unanswered by the refresh — and whose issue endpoint answers
// with issueStatus. It reports how many times the issue endpoint was asked,
// which is what separates "declined to confirm" from "confirmed and declined to
// act".
func jiraGoneFixture(t *testing.T, issueStatus int, issueBody string) (*httptest.Server, *int32) {
	t.Helper()
	var probes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/issue/") {
			atomic.AddInt32(&probes, 1)
			w.WriteHeader(issueStatus)
			_, _ = w.Write([]byte(issueBody))
			return
		}
		// Both the discovery JQL pair and the key-batch refresh land here.
		_, _ = w.Write([]byte(`{"issues":[],"total":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &probes
}

func TestRefreshJira_ConfirmedDeletionRetiresEntity(t *testing.T) {
	srv, probes := jiraGoneFixture(t, http.StatusNotFound, `{"errorMessages":["Issue does not exist"]}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraGoneGrace),
	); err != nil {
		t.Fatalf("backdate last_polled_at: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("issue endpoint asked %d times, want exactly 1 — the confirmation is the whole safety margin", got)
	}
	evts := pub.nonSystemEvents()
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueDeleted {
		t.Fatalf("emitted %v, want exactly one %s", eventTypes(evts), domain.EventJiraIssueDeleted)
	}
	if evts[0].EntityID == nil {
		t.Error("deletion event carries no entity id — the router closes the entity the event names")
	}
	if evts[0].DedupKey != "" {
		t.Errorf("dedup_key = %q, want empty — an issue can only stop existing once", evts[0].DedupKey)
	}
}

// The case the confirmation exists to protect: the issue is absent from every
// search but still resolves. Closing here would retire a live entity — along
// with its tasks — over an unindexed or newly invisible issue.
func TestRefreshJira_MissingButResolvableIssueIsNotRetired(t *testing.T) {
	srv, probes := jiraGoneFixture(t, http.StatusOK,
		`{"key":"SKY-1","fields":{"summary":"Still here","status":{"name":"In Progress"}}}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraGoneGrace),
	); err != nil {
		t.Fatalf("backdate last_polled_at: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("issue endpoint asked %d times, want exactly 1", got)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("emitted %v, want nothing — the issue resolves, so its absence from search is not deletion", eventTypes(evts))
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "jira", "SKY-1")
	if err != nil || ent == nil {
		t.Fatalf("read entity back: %v", err)
	}
	if ent.State != "active" {
		t.Errorf("entity state = %q, want active", ent.State)
	}
}

// A key missing for the first time is not confirmed at all. Absence from one
// search is the weakest possible evidence, and the request is only worth
// spending once the key looks durably unanswered.
func TestRefreshJira_RecentlyAnsweredKeyIsNotConfirmed(t *testing.T) {
	srv, probes := jiraGoneFixture(t, http.StatusNotFound, `{}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}}

	// FindOrCreate stamps last_polled_at at creation, so this entity reads as
	// freshly answered — no backdating.
	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got != 0 {
		t.Fatalf("issue endpoint asked %d times, want 0 — one missed cycle is not grounds to spend a confirmation", got)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("emitted %v, want nothing", eventTypes(evts))
	}
}

// A confirmation that fails for any other reason is not evidence either way.
func TestRefreshJira_FailedConfirmationLeavesEntityTracked(t *testing.T) {
	srv, probes := jiraGoneFixture(t, http.StatusInternalServerError, `{"errorMessages":["boom"]}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: []string{"Done"}}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraGoneGrace),
	); err != nil {
		t.Fatalf("backdate last_polled_at: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got == 0 {
		t.Fatal("test wiring: the confirmation never ran")
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("emitted %v, want nothing — a failed confirmation says nothing about the issue", eventTypes(evts))
	}
}
