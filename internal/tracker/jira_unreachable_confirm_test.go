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

// jiraUnreachableFixture stands up a Jira whose searches match nothing — so every
// tracked key goes unanswered by the refresh — and whose issue endpoint answers
// with issueStatus. It reports how many times the issue endpoint was asked,
// which is what separates "declined to confirm" from "confirmed and declined to
// act".
func jiraUnreachableFixture(t *testing.T, issueStatus int, issueBody string) (*httptest.Server, *int32) {
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

func TestRefreshJira_ConfirmedUnreachableRetiresEntity(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusNotFound, `{"errorMessages":["Issue does not exist"]}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraUnreachableGrace),
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
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueUnreachable {
		t.Fatalf("emitted %v, want exactly one %s", eventTypes(evts), domain.EventJiraIssueUnreachable)
	}
	if evts[0].EntityID == nil {
		t.Error("unreachable event carries no entity id — the router closes the entity the event names")
	}
	if evts[0].DedupKey != "" {
		t.Errorf("dedup_key = %q, want empty — an issue can only stop existing once", evts[0].DedupKey)
	}
}

// The case the confirmation exists to protect: the issue is absent from every
// search but still resolves. Closing here would retire a live entity — along
// with its tasks — over an unindexed or newly invisible issue.
func TestRefreshJira_MissingButResolvableIssueIsNotRetired(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusOK,
		`{"key":"SKY-1","fields":{"summary":"Still here","status":{"name":"In Progress"}}}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraUnreachableGrace),
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
		t.Fatalf("emitted %v, want nothing — the issue resolves, so its absence from search proves nothing", eventTypes(evts))
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "jira", "SKY-1")
	if err != nil || ent == nil {
		t.Fatalf("read entity back: %v", err)
	}
	if ent.State != "active" {
		t.Errorf("entity state = %q, want active", ent.State)
	}
	// The confirmation stamped it, so it drops out of the candidate set and
	// stops consuming the per-cycle budget. Without this an entity that will
	// confirm present forever sits at the head of the oldest-first candidate
	// ordering and starves every key behind it — including ones that would
	// have confirmed unreachable. A second cycle must therefore not re-probe.
	if ent.LastPolledAt == nil || time.Since(*ent.LastPolledAt) >= jiraUnreachableGrace {
		t.Fatalf("last_polled_at = %v, want freshly stamped", ent.LastPolledAt)
	}
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira cycle 2: %v", err)
	}
	if got := atomic.LoadInt32(probes); got != 1 {
		t.Errorf("issue endpoint asked %d times across two cycles, want 1 — a confirmed-present key must not be re-probed every cycle", got)
	}
}

func TestRefreshJira_MovedIssueIsRekeyedInPlace(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusOK,
		`{"key":"NEW-7","fields":{"summary":"Moved","status":{"name":"In Progress"}}}`)
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	old, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "OLD-7", "issue", "History stays here", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(`UPDATE entities SET last_polled_at=? WHERE id=?`, time.Now().Add(-2*jiraUnreachableGrace), old.ID); err != nil {
		t.Fatalf("prepare entity: %v", err)
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	// NEW is intentionally not configured: durable entities follow Jira even
	// when a move leaves the discovery set.
	if _, err := tr.RefreshJira(ctx, client, srv.URL, JiraRules{{Key: "OLD"}}); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("probes = %d, want 1", got)
	}
	if got, _ := stores.Entities.GetBySource(ctx, org, "jira", "OLD-7"); got != nil {
		t.Fatal("old key still resolves to an entity")
	}
	got, err := stores.Entities.GetBySource(ctx, org, "jira", "NEW-7")
	if err != nil || got == nil {
		t.Fatalf("new key entity: %v", err)
	}
	if got.ID != old.ID || got.Title != "History stays here" {
		t.Errorf("rekey lost identity/history: got id=%q title=%q", got.ID, got.Title)
	}
	if evts := tr.pub.(*recordingPublisher).nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("move emitted events: %v", eventTypes(evts))
	}
}

func TestRefreshJira_MovedIssueMergesIntoCurrentKey(t *testing.T) {
	srv, _ := jiraUnreachableFixture(t, http.StatusOK,
		`{"key":"NEW-9","fields":{"summary":"Moved","status":{"name":"In Progress"}}}`)
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	old, _, _ := stores.Entities.FindOrCreate(ctx, org, "jira", "OLD-9", "issue", "old", "")
	current, _, _ := stores.Entities.FindOrCreate(ctx, org, "jira", "NEW-9", "issue", "current", "")
	if _, err := database.Exec(`UPDATE entities SET last_polled_at=? WHERE id=?`, time.Now().Add(-2*jiraUnreachableGrace), old.ID); err != nil {
		t.Fatal(err)
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, JiraRules{{Key: "OLD"}, {Key: "NEW"}}); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if got, _ := stores.Entities.Get(ctx, org, old.ID); got != nil {
		t.Fatal("obsolete entity survived merge")
	}
	got, err := stores.Entities.GetBySource(ctx, org, "jira", "NEW-9")
	if err != nil || got == nil || got.ID != current.ID {
		t.Fatalf("survivor = %#v, err=%v; want %s", got, err, current.ID)
	}
}

// A key missing for the first time is not confirmed at all. Absence from one
// search is the weakest possible evidence, and the request is only worth
// spending once the key looks durably unanswered.
func TestRefreshJira_RecentlyAnsweredKeyIsNotConfirmed(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusNotFound, `{}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

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
	srv, probes := jiraUnreachableFixture(t, http.StatusInternalServerError, `{"errorMessages":["boom"]}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE entities SET last_polled_at = ? WHERE source_id = 'SKY-1'`,
		time.Now().Add(-2*jiraUnreachableGrace),
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
