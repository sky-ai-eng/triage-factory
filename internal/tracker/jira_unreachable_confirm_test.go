package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
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

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "https://jira.example.com", "SKY-1", "10001", "issue", "", ""); err != nil {
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
	if _, err := tr.RefreshJira(ctx, "https://jira.example.com", client, srv.URL, projects); err != nil {
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
	var meta events.JiraIssueUnreachableMetadata
	if err := json.Unmarshal([]byte(evts[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Reason != events.JiraUnreachableNotFound || meta.IssueKey != "SKY-1" || meta.IssueID != "10001" || meta.Project != "SKY" {
		t.Errorf("metadata = %+v, want not_found for SKY-1 (10001) in SKY", meta)
	}
}

// The case the confirmation exists to protect: the issue is absent from every
// search but still resolves. Closing here would retire a live entity — along
// with its tasks — over an unindexed or newly invisible issue.
func TestRefreshJira_MissingButResolvableIssueIsNotRetired(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusOK,
		`{"id":"10001","key":"SKY-1","fields":{"summary":"Still here","status":{"name":"In Progress"}}}`)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "https://jira.example.com", "SKY-1", "10001", "issue", "", ""); err != nil {
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
	if _, err := tr.RefreshJira(ctx, "https://jira.example.com", client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("issue endpoint asked %d times, want exactly 1", got)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("emitted %v, want nothing — the issue resolves, so its absence from search proves nothing", eventTypes(evts))
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "jira", "https://jira.example.com", "SKY-1")
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
	if _, err := tr.RefreshJira(ctx, "https://jira.example.com", client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira cycle 2: %v", err)
	}
	if got := atomic.LoadInt32(probes); got != 1 {
		t.Errorf("issue endpoint asked %d times across two cycles, want 1 — a confirmed-present key must not be re-probed every cycle", got)
	}
}

// An entity created before issue ids were recorded, whose issue moved before
// it learned its id: the refresh reads it by key, and a key the issue has left
// answers under the new one, so the entity is asked about on its first miss —
// no grace — and learns its id and its current key from the answer.
func TestRefreshJira_IdlessMovedIssueLearnsItsIDAndIsRenamed(t *testing.T) {
	srv, probes := jiraUnreachableFixture(t, http.StatusOK,
		`{"id":"10007","key":"NEW-7","fields":{"summary":"Moved","status":{"name":"In Progress"}}}`)
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	const site = "https://jira.example.com"
	// FindOrCreate stamps last_polled_at at creation: this is the entity's
	// first miss.
	old, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", site, "OLD-7", "", "issue", "History stays here", site+"/browse/OLD-7")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, site, client, site, JiraRules{{Key: "OLD"}, {Key: "NEW"}}); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if got := atomic.LoadInt32(probes); got != 1 {
		t.Fatalf("probes = %d, want 1 — an id-less entity is confirmed on its first miss", got)
	}
	if got, _ := stores.Entities.GetBySource(ctx, org, "jira", site, "OLD-7"); got != nil {
		t.Fatal("old key still resolves to an entity")
	}
	got, err := stores.Entities.GetBySource(ctx, org, "jira", site, "NEW-7")
	if err != nil || got == nil {
		t.Fatalf("new key entity: %v", err)
	}
	if got.ID != old.ID || got.Title != "History stays here" || got.ExternalID != "10007" || got.URL != site+"/browse/NEW-7" {
		t.Errorf("entity = %+v, want the same row, its id learned, key and url renamed", got)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("the confirmation emitted %v; the next refresh diffs the move", eventTypes(evts))
	}
}

// One issue left under two rows by a move nothing followed: the older row,
// under the key the issue left, and a newer one discovery created under its
// current key, carrying the id. When the older row learns the id, the two are
// merged, and the older row survives with the newer one's live state.
func TestRefreshJira_LegacyDuplicatePairMergesIntoTheOlderRow(t *testing.T) {
	srv, _ := jiraUnreachableFixture(t, http.StatusOK,
		`{"id":"10009","key":"NEW-9","fields":{"summary":"Moved","status":{"name":"In Progress"}}}`)
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	const site = "https://jira.example.com"
	old, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", site, "OLD-9", "", "issue", "old", "")
	if err != nil {
		t.Fatalf("seed old: %v", err)
	}
	current, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", site, "NEW-9", "10009", "issue", "current", "")
	if err != nil {
		t.Fatalf("seed current: %v", err)
	}
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, site, client, site, JiraRules{{Key: "OLD"}, {Key: "NEW"}}); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}
	if got, _ := stores.Entities.Get(ctx, org, current.ID); got != nil {
		t.Fatal("the newer row survived the merge")
	}
	got, err := stores.Entities.GetBySource(ctx, org, "jira", site, "NEW-9")
	if err != nil || got == nil || got.ID != old.ID || got.ExternalID != "10009" || got.State != "active" {
		t.Fatalf("NEW-9 = %+v, err=%v; want the older row %s, carrying the id", got, err, old.ID)
	}
}

// An issue with a known id missing for the first time is not confirmed at
// all. Absence from one search is the weakest possible evidence, and the
// request is only worth spending once the issue looks durably unanswered.
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
	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "https://jira.example.com", "SKY-1", "10001", "issue", "", ""); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, "https://jira.example.com", client, srv.URL, projects); err != nil {
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

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "https://jira.example.com", "SKY-1", "10001", "issue", "", ""); err != nil {
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
	if _, err := tr.RefreshJira(ctx, "https://jira.example.com", client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if got := atomic.LoadInt32(probes); got == 0 {
		t.Fatal("test wiring: the confirmation never ran")
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("emitted %v, want nothing — a failed confirmation says nothing about the issue", eventTypes(evts))
	}
}
