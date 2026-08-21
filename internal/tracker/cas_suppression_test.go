package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// failingBatchQueue wraps a real EventQueueStore but fails every combined
// snapshot-CAS emit, either as a lost CAS (ok=false — the straggler
// ex-leader shape: another writer advanced poll_seq between this cycle's
// read and its write) or as a transaction error. Every other method
// delegates, so the queue behaves normally for anything else.
type failingBatchQueue struct {
	db.EventQueueStore
	err   error // nil → report a lost CAS instead of an error
	calls int32
}

func (q *failingBatchQueue) EnqueueBatchWithSnapshotCAS(context.Context, string, string, string, int64, []domain.Event, []string) (bool, []string, error) {
	atomic.AddInt32(&q.calls, 1)
	return false, nil, q.err
}

// countQueueRows counts every event_queue row in the test database,
// whatever entity or status it belongs to. The CAS-lost assertions want
// "this writer wrote nothing", so an entity-scoped read would be too
// narrow — a stray row for another entity is still a row that shouldn't
// exist.
func countQueueRows(t *testing.T, tr *Tracker) int {
	t.Helper()
	var n int
	if err := tr.database.QueryRow(`SELECT COUNT(*) FROM event_queue`).Scan(&n); err != nil {
		t.Fatalf("count event_queue: %v", err)
	}
	return n
}

// ghDraftToReadyServer serves a PR that is draft on the first GraphQL
// refresh and ready on every one after it — a real draft→ready transition
// for the second and later cycles to diff. The REST arm answers the
// stub's node_id resolve.
func ghDraftToReadyServer(t *testing.T) *httptest.Server {
	t.Helper()
	const basicResp = `{
		"number": 7, "node_id": "PR_node7", "title": "Stub PR",
		"state": "open", "merged": false,
		"html_url": "https://github.com/octo/repo/pull/7",
		"user": {"login": "bob"},
		"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
	}`
	refreshResp := func(draft string) string {
		return `{"data":{"nodes":[
			{"id":"PR_node7","number":7,"title":"Stub PR","author":{"login":"bob"},
			 "state":"OPEN","merged":false,"isDraft":` + draft + `,
			 "url":"https://github.com/octo/repo/pull/7","repository":{"nameWithOwner":"octo/repo"},
			 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
		]}}`
	}

	var graphqlCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			if atomic.AddInt32(&graphqlCalls, 1) == 1 {
				_, _ = w.Write([]byte(refreshResp("true")))
			} else {
				_, _ = w.Write([]byte(refreshResp("false")))
			}
		case strings.Contains(r.URL.Path, "/pulls/7"):
			_, _ = w.Write([]byte(basicResp))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRefreshGitHub_FailedCommitSuppressesTransitions pins the contract end
// to end, for both ways the combined write can fail to commit: a lost CAS
// and a transaction error.
//
// The snapshot-diff is the SOLE re-emit prevention, so a cycle whose
// snapshot write doesn't land must not publish the transitions it diffed —
// they would re-derive next cycle under fresh event ids (the event/trigger
// fence can't collapse them) and double-fire delegation. And because the
// snapshot and its events now commit together, a failure writes NOTHING:
// no queue rows for a transition nobody recorded. Nothing is lost either
// way — the next cycle whose write DOES land re-diffs from the surviving
// snapshot and emits the transition then, exactly once.
func TestRefreshGitHub_FailedCommitSuppressesTransitions(t *testing.T) {
	cases := []struct {
		name string
		err  error // nil → lost CAS; non-nil → tx error
	}{
		{name: "cas lost"},
		{name: "commit errored", err: errors.New("db down mid-batch")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := ghDraftToReadyServer(t)

			ctx := context.Background()
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)
			org := runmode.LocalDefaultOrgID
			client := ghclient.NewClient(srv.URL, "tok")

			entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", "")
			if err != nil {
				t.Fatalf("seed stub: %v", err)
			}

			// Cycle 1 against the REAL store: quiet-seeds the draft snapshot.
			pub := &recordingPublisher{}
			tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
			if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
				t.Fatalf("RefreshGitHub cycle 1: %v", err)
			}
			if evts := pub.nonSystemEvents(); len(evts) != 0 {
				t.Fatalf("cycle 1 (seed) must emit no events, got %v", eventTypes(evts))
			}

			// Cycle 2 as the failing writer: the diff computes draft→ready,
			// but the combined write reports lost/errored — the transition
			// must be suppressed, not published off a snapshot that didn't
			// win, and nothing may reach the outbox.
			failing := &failingBatchQueue{EventQueueStore: stores.EventQueue, err: tc.err}
			losingPub := &recordingPublisher{}
			loser := New(database, losingPub, stores.Tasks, stores.Entities, stores.Repos, failing, org)
			if _, _, err := loser.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
				t.Fatalf("RefreshGitHub cycle 2 (loser): %v", err)
			}
			if atomic.LoadInt32(&failing.calls) == 0 {
				t.Fatal("test wiring: the losing cycle never attempted the combined snapshot+events write")
			}
			if evts := losingPub.nonSystemEvents(); len(evts) != 0 {
				t.Errorf("a failed snapshot+events commit must suppress the cycle's transitions, got %v", eventTypes(evts))
			}
			if n := countQueueRows(t, loser); n != 0 {
				t.Errorf("event_queue has %d rows after the losing cycle, want 0 — a writer that lost the CAS must enqueue nothing", n)
			}

			// Cycle 3 against the real store: the snapshot still says draft
			// (the loser's write never landed), so the SAME transition
			// re-diffs, the commit lands, and the event publishes —
			// suppression lost nothing, and the transition is queued exactly
			// once (a second enqueue would violate event_queue's unique
			// event_id index).
			winnerPub := &recordingPublisher{}
			winner := New(database, winnerPub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
			if _, _, err := winner.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
				t.Fatalf("RefreshGitHub cycle 3: %v", err)
			}
			evts := winnerPub.nonSystemEvents()
			if len(evts) != 1 || evts[0].EventType != domain.EventGitHubPRReadyForReview {
				t.Fatalf("the winning cycle should emit the suppressed transition exactly once, got %v", eventTypes(evts))
			}
			rows, err := stores.EventQueue.ListForEntity(ctx, org, entity.ID)
			if err != nil {
				t.Fatalf("ListForEntity: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("event_queue has %d rows for the entity, want exactly 1", len(rows))
			}
			if rows[0].EventID != evts[0].ID {
				t.Errorf("queued event_id = %q but the bus event carries %q — the published copy must carry the id the enqueue minted",
					rows[0].EventID, evts[0].ID)
			}
			if evts[0].OrgID != rows[0].OrgID {
				t.Errorf("published event org = %q but its queue row committed under %q — the copy must name the tenant it was written under",
					evts[0].OrgID, rows[0].OrgID)
			}
			if rows[0].Status != domain.QueuedEventStatusPending {
				t.Errorf("queue row status = %q, want pending", rows[0].Status)
			}
		})
	}
}

// TestRefreshGitHub_CommittedEventsRouteWithoutTheBus is the crash-after-
// commit case: the transaction lands, then the process dies before the
// cosmetic bus fan-out. Modeled by a publisher that drops everything.
// The queue row is what the router consumes, so the event still routes —
// only the real-time push is missed.
func TestRefreshGitHub_CommittedEventsRouteWithoutTheBus(t *testing.T) {
	srv := ghDraftToReadyServer(t)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := ghclient.NewClient(srv.URL, "tok")

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", "")
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}

	// Cycle 1 seeds the draft snapshot; cycle 2 diffs draft→ready. Both run
	// against a publisher that never delivers anything.
	tr := New(database, droppingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 1: %v", err)
	}
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 2: %v", err)
	}

	rows, err := stores.EventQueue.ListForEntity(ctx, org, entity.ID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("event_queue has %d rows, want 1 — a committed transition must be routable without the bus", len(rows))
	}
	if rows[0].EventType != domain.EventGitHubPRReadyForReview {
		t.Errorf("queued event_type = %q, want %q", rows[0].EventType, domain.EventGitHubPRReadyForReview)
	}
	if rows[0].Status != domain.QueuedEventStatusPending {
		t.Errorf("queue row status = %q, want pending (waiting for the drain worker)", rows[0].Status)
	}
}

// TestRefreshJira_FailedCommitSuppressesTransitions is the Jira arm of the
// same contract — the two arms share the emit path, and this is what keeps
// them from drifting.
func TestRefreshJira_FailedCommitSuppressesTransitions(t *testing.T) {
	// The issue's status is whatever the test last set: cycle 1 seeds
	// "In Progress", and flipping to "In Review" gives the later cycles a
	// real transition to diff. A cycle issues several searches (a JQL pair
	// per project, then the key-batch refresh), so the state has to be a
	// flag the test flips between cycles, not a call counter.
	var status atomic.Value
	status.Store("In Progress")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/search") {
			_, _ = w.Write([]byte(`{"issues":[
				{"key":"SKY-1","fields":{
					"summary":"Touched issue","status":{"name":"` + status.Load().(string) + `"},
					"assignee":{"displayName":"Alice","accountId":"acc-1"},
					"created":"2026-06-01T00:00:00.000+0000","updated":"2026-06-10T00:00:00.000+0000"
				}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	projects := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-1", "issue", "", "")
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}

	// Cycle 1 against the REAL store: quiet-seeds the In Progress snapshot.
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := tr.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira cycle 1: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Fatalf("cycle 1 (seed) must emit no events, got %v", eventTypes(evts))
	}
	status.Store("In Review")

	// Cycle 2 as the CAS-losing straggler: suppressed, and nothing queued.
	failing := &failingBatchQueue{EventQueueStore: stores.EventQueue}
	losingPub := &recordingPublisher{}
	loser := New(database, losingPub, stores.Tasks, stores.Entities, stores.Repos, failing, org)
	if _, err := loser.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira cycle 2 (loser): %v", err)
	}
	if atomic.LoadInt32(&failing.calls) == 0 {
		t.Fatal("test wiring: the losing cycle never attempted the combined snapshot+events write")
	}
	if evts := losingPub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("a lost snapshot CAS must suppress the cycle's transitions, got %v", eventTypes(evts))
	}
	if n := countQueueRows(t, loser); n != 0 {
		t.Errorf("event_queue has %d rows after the losing cycle, want 0", n)
	}

	// Cycle 3 against the real store: the same transition re-diffs and its
	// events commit — once each.
	winnerPub := &recordingPublisher{}
	winner := New(database, winnerPub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, err := winner.RefreshJira(ctx, client, srv.URL, projects); err != nil {
		t.Fatalf("RefreshJira cycle 3: %v", err)
	}
	evts := winnerPub.nonSystemEvents()
	if len(evts) != 1 || evts[0].EventType != domain.EventJiraIssueStatusChanged {
		t.Fatalf("the winning cycle should emit the suppressed transition exactly once, got %v", eventTypes(evts))
	}
	rows, err := stores.EventQueue.ListForEntity(ctx, org, entity.ID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(rows) != len(evts) {
		t.Fatalf("event_queue has %d rows for the entity, want %d (one per published transition)", len(rows), len(evts))
	}
	for i, row := range rows {
		if row.EventID != evts[i].ID {
			t.Errorf("row %d: queued event_id = %q but the bus event carries %q", i, row.EventID, evts[i].ID)
		}
	}
}
