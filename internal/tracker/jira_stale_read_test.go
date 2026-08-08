package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// jiraIssuePage renders a one-issue search response for the tracked key. Only
// the fields the refresh diff reads are carried, so a page differs from its
// neighbour in exactly the two dimensions these tests vary.
func jiraIssuePage(status, updated string) string {
	return `{"issues":[
		{"key":"SKY-1","fields":{
			"summary":"Stale-read subject","status":{"name":"` + status + `"},
			"assignee":{"displayName":"Alice","accountId":"acc-1"},
			"created":"2026-06-01T00:00:00.000+0000","updated":"` + updated + `"
		}}
	]}`
}

// jiraPageServer serves one prepared search page per refresh cycle, in order,
// and fails the test if a cycle asks for a page that wasn't scripted — which is
// how a guard that skipped a fetch (rather than skipping the diff behind it)
// would show up.
func jiraPageServer(t *testing.T, pages []string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/search") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if call >= len(pages) {
			t.Errorf("search call %d beyond the %d scripted pages", call+1, len(pages))
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		body := pages[call]
		call++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jiraRefreshFixture seeds a snapshot-less SKY-1 stub and returns everything a
// cycle needs. projects is nil throughout this file: discovery is then a no-op,
// so every search the server sees is a phase-2 refresh and the cycle count is
// unambiguous.
func jiraRefreshFixture(t *testing.T, srv *httptest.Server) (*Tracker, *recordingPublisher, db.Stores) {
	t.Helper()
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", ""); err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	pub := &recordingPublisher{}
	return New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org), pub, stores
}

// storedJiraStatus reads back the status the entity's snapshot currently holds.
func storedJiraStatus(t *testing.T, stores db.Stores) string {
	t.Helper()
	ent, err := stores.Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-1")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	var snap domain.JiraSnapshot
	if err := json.Unmarshal([]byte(ent.SnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal stored snapshot %q: %v", ent.SnapshotJSON, err)
	}
	return snap.Status
}

// TestRefreshJira_StaleReadNeitherEmitsNorRegressesBaseline is the acceptance
// property in full: ONE real change produces exactly ONE event, whatever order
// the search index's pages arrive in.
//
// Cycle 3 is served a page that predates cycle 2's — the non-monotonic pair an
// eventually-consistent index declines to rule out. Without the guard it would
// diff backwards (a second status_changed, under a different dedup key, so a
// second task for a status the ticket already left) and then overwrite the
// fresher snapshot, so cycle 4's re-read of the true state would emit the same
// transition a third time. Both must be silent, and the stored snapshot must
// never have moved backwards for that to be possible.
func TestRefreshJira_StaleReadNeitherEmitsNorRegressesBaseline(t *testing.T) {
	const (
		older = "2026-06-10T09:00:00.000+0000"
		newer = "2026-06-10T10:00:00.000+0000"
	)
	srv := jiraPageServer(t, []string{
		jiraIssuePage("To Do", older),       // cycle 1: quiet seed of the stub
		jiraIssuePage("In Progress", newer), // cycle 2: the one real change
		jiraIssuePage("To Do", older),       // cycle 3: stale page, arriving late
		jiraIssuePage("In Progress", newer), // cycle 4: the truth again
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

	runCycle := func(n int) {
		t.Helper()
		if _, err := tr.RefreshJira(context.Background(), client, srv.URL, nil); err != nil {
			t.Fatalf("RefreshJira cycle %d: %v", n, err)
		}
	}

	runCycle(1)
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("cycle 1 (quiet seed) emitted %v, want none", eventTypes(evts))
	}

	runCycle(2)
	evts := pub.nonSystemEvents()
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueStatusChanged {
		t.Fatalf("cycle 2 should emit exactly [status_changed], got %v", eventTypes(evts))
	}

	runCycle(3)
	if evts := pub.nonSystemEvents(); len(evts) != 1 {
		t.Errorf("cycle 3 (stale read) emitted %v; want the cycle-2 event alone", eventTypes(evts))
	}
	if got := storedJiraStatus(t, stores); got != "In Progress" {
		t.Errorf("stored status after the stale read = %q, want %q — the baseline regressed", got, "In Progress")
	}

	runCycle(4)
	if evts := pub.nonSystemEvents(); len(evts) != 1 {
		t.Errorf("cycle 4 re-read the true state and emitted %v; want the cycle-2 event alone "+
			"(the baseline never regressed, so there is no forward transition left to re-fire)", eventTypes(evts))
	}
}

// TestRefreshJira_EqualUpdatedStillDiffs pins the strictly-older boundary.
// Jira's `updated` is millisecond-resolution, so two edits inside one
// millisecond share a timestamp; treating equal as stale would swallow the
// second one permanently, since no later read would ever re-derive it.
func TestRefreshJira_EqualUpdatedStillDiffs(t *testing.T) {
	const same = "2026-06-10T10:00:00.000+0000"
	srv := jiraPageServer(t, []string{
		jiraIssuePage("To Do", same),
		jiraIssuePage("In Progress", same),
	})
	tr, pub, stores := jiraRefreshFixture(t, srv)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

	for i := 1; i <= 2; i++ {
		if _, err := tr.RefreshJira(context.Background(), client, srv.URL, nil); err != nil {
			t.Fatalf("RefreshJira cycle %d: %v", i, err)
		}
	}
	evts := pub.nonSystemEvents()
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueStatusChanged {
		t.Errorf("equal updated with a differing status should emit [status_changed], got %v", eventTypes(evts))
	}
	if got := storedJiraStatus(t, stores); got != "In Progress" {
		t.Errorf("stored status = %q, want %q — the write must land too", got, "In Progress")
	}
}

// TestRefreshJira_UncomparableUpdatedDiffsAsBefore covers the degradation
// contract: a timestamp that is absent or in a shape we don't parse is not
// evidence that a read is stale, so the guard must stand aside rather than
// suppress. Snapshots written before the field existed carry none, so the
// alternative is a tracked issue that silently stops emitting forever.
func TestRefreshJira_UncomparableUpdatedDiffsAsBefore(t *testing.T) {
	cases := []struct {
		name          string
		seedUpdated   string
		secondUpdated string
	}{
		// In each case the second read looks older than the first (or is
		// missing a comparable time entirely), so a guard that resolved
		// unknowns against the fetch would suppress it.
		{"stored has no updated", "", "2026-06-10T09:00:00.000+0000"},
		{"fetched has no updated", "2026-06-10T10:00:00.000+0000", ""},
		{"stored unparseable", "10 June 2026", "2026-06-10T09:00:00.000+0000"},
		{"fetched unparseable", "2026-06-10T10:00:00.000+0000", "10 June 2026"},
		{"neither parseable", "10 June 2026", "9 June 2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := jiraPageServer(t, []string{
				jiraIssuePage("To Do", tc.seedUpdated),
				jiraIssuePage("In Progress", tc.secondUpdated),
			})
			tr, pub, stores := jiraRefreshFixture(t, srv)
			client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))

			for i := 1; i <= 2; i++ {
				if _, err := tr.RefreshJira(context.Background(), client, srv.URL, nil); err != nil {
					t.Fatalf("RefreshJira cycle %d: %v", i, err)
				}
			}
			evts := pub.nonSystemEvents()
			if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueStatusChanged {
				t.Errorf("should diff exactly as before the guard existed: want [status_changed], got %v", eventTypes(evts))
			}
			if got := storedJiraStatus(t, stores); got != "In Progress" {
				t.Errorf("stored status = %q, want %q", got, "In Progress")
			}
		})
	}
}

// TestJiraReadIsStale exercises the predicate directly, where the boundary
// cases are cheap to state.
func TestJiraReadIsStale(t *testing.T) {
	const (
		t1     = "2026-06-10T09:00:00.000+0000"
		t2     = "2026-06-10T10:00:00.000+0000"
		t2NoMS = "2026-06-10T10:00:00+0000"
		t2RFC  = "2026-06-10T10:00:00Z"
	)
	cases := []struct {
		name          string
		stored, fetch string
		want          bool
	}{
		{"fetch older", t2, t1, true},
		{"fetch newer", t1, t2, false},
		{"equal", t2, t2, false},
		{"equal across layouts", t2, t2NoMS, false},
		{"older across layouts", t2RFC, t1, true},
		{"stored empty", "", t1, false},
		{"fetch empty", t2, "", false},
		{"both empty", "", "", false},
		{"stored unparseable", "yesterday", t1, false},
		{"fetch unparseable", t2, "yesterday", false},
		{"sub-millisecond older", "2026-06-10T10:00:00.002+0000", "2026-06-10T10:00:00.001+0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, got := jiraReadIsStale(
				domain.JiraSnapshot{UpdatedAt: tc.stored},
				domain.JiraSnapshot{UpdatedAt: tc.fetch},
			)
			if got != tc.want {
				t.Errorf("jiraReadIsStale(stored=%q, fetched=%q) = %v, want %v", tc.stored, tc.fetch, got, tc.want)
			}
		})
	}
}
