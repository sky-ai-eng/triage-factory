package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedBackfillServer answers the discovery listing with one open PR carrying
// the given requested reviewers, and reports the node inaccessible on the
// Phase-2 refresh so the discovery seed is the only thing that writes.
func seedBackfillServer(t *testing.T, reviewers ...string) *httptest.Server {
	t.Helper()
	if len(reviewers) == 0 {
		reviewers = []string{"bob", "carol"}
	}
	requested := make([]string, len(reviewers))
	for i, login := range reviewers {
		requested[i] = `{"login": ` + strconv.Quote(login) + `}`
	}
	requestedJSON := "[" + strings.Join(requested, ", ") + "]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.Contains(r.URL.Path, "/pulls/42"):
			// The snapshot-less stub resolve Phase 2 attempts when a seed lost
			// its CAS. Unresolvable is a skip, not a failure.
			http.Error(w, "not found", http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[{
				"number": 42, "node_id": "PR_42", "title": "Add widget", "state": "open",
				"html_url": "https://github.com/octo/repo/pull/42",
				"user": {"login": "alice"},
				"requested_reviewers": ` + requestedJSON + `,
				"head": {"sha": "sha1", "ref": "feat"}, "base": {"ref": "main"},
				"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-01T00:00:00Z"
			}]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingPreEnqueuedPublisher records only the post-commit fan-out, which is
// what a seed's backfill takes: the events ride the snapshot's transaction, so
// nothing reaches the bus through Publish.
type recordingPreEnqueuedPublisher struct {
	mu        sync.Mutex
	preQueued []domain.Event
	published []domain.Event
}

func (p *recordingPreEnqueuedPublisher) Publish(_ context.Context, evt domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, evt)
}

func (p *recordingPreEnqueuedPublisher) PublishPreEnqueued(_ context.Context, evt domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preQueued = append(p.preQueued, evt)
}

func (p *recordingPreEnqueuedPublisher) snapshot() (pre, pub []domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.Event(nil), p.preQueued...), append([]domain.Event(nil), p.published...)
}

// TestRefreshGitHub_SeedCommitsItsReviewBackfill is the durability property the
// seed's shape exists for: the stored snapshot is the sole re-emit guard, so a
// snapshot that commits without the review requests it implies retires them
// permanently — the next cycle diffs same-against-same and the reviewer never
// hears about a request that was already there when TF started watching.
//
// One snapshot and both backfilled events, one transaction. And the bus sees
// them only after that commit, through the pre-enqueued fan-out: routing
// consumes the queue, so a death between the commit and the fan-out costs a
// live update, never the task.
func TestRefreshGitHub_SeedCommitsItsReviewBackfill(t *testing.T) {
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	pub := &recordingPreEnqueuedPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)

	if _, _, err := tr.RefreshGitHub(ctx, newSeedClient(t), "", []string{"octo/repo"}, knownLogins{"bob": true, "carol": true}); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#42")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	if ent.SnapshotJSON == "" || ent.SnapshotJSON == "{}" {
		t.Fatal("the seed committed no snapshot")
	}

	var queued int
	if err := database.QueryRow(
		`SELECT count(*) FROM event_queue WHERE entity_id = ? AND event_type = ?`,
		ent.ID, domain.EventGitHubPRReviewRequested,
	).Scan(&queued); err != nil {
		t.Fatalf("count event_queue: %v", err)
	}
	if queued != 2 {
		t.Errorf("event_queue rows = %d, want 2 (one per known reviewer, committed with the seed)", queued)
	}
	var recorded int
	if err := database.QueryRow(
		`SELECT count(*) FROM events WHERE entity_id = ? AND event_type = ?`,
		ent.ID, domain.EventGitHubPRReviewRequested,
	).Scan(&recorded); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if recorded != 2 {
		t.Errorf("events rows = %d, want 2", recorded)
	}

	pre, published := pub.snapshot()
	if len(pre) != 2 {
		t.Errorf("post-commit fan-out carried %d events, want 2", len(pre))
	}
	for _, evt := range pre {
		if evt.ID == "" {
			t.Error("a fanned-out backfill event carries no id — the bus and the queue row disagree")
		}
	}
	for _, evt := range published {
		if evt.EventType == domain.EventGitHubPRReviewRequested {
			t.Error("a backfill event went through Publish, which would enqueue it a second time")
		}
	}
}

// TestRefreshGitHub_SeedCASMissCommitsNothing pins the loser contract: a
// concurrent seed won the snapshot, so this cycle writes neither the snapshot
// nor the backfill it was carrying, and does not re-attempt it — the winner's
// own seed carried the same events.
func TestRefreshGitHub_SeedCASMissCommitsNothing(t *testing.T) {
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	pub := &recordingPreEnqueuedPublisher{}
	queue := &failingBatchQueue{EventQueueStore: stores.EventQueue}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, queue, org)

	if _, _, err := tr.RefreshGitHub(ctx, newSeedClient(t), "", []string{"octo/repo"}, knownLogins{"bob": true, "carol": true}); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	if n := countQueueRows(t, tr); n != 0 {
		t.Errorf("event_queue rows = %d, want 0 (a lost CAS writes nothing)", n)
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#42")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	if ent.SnapshotJSON != "" && ent.SnapshotJSON != "{}" {
		t.Errorf("snapshot_json = %q, want empty (the loser's snapshot must not land either)", ent.SnapshotJSON)
	}
	if pre, _ := pub.snapshot(); len(pre) != 0 {
		t.Errorf("post-commit fan-out carried %d events after a lost CAS, want 0", len(pre))
	}
}

// knownLogins is a ReviewerResolver that knows the listed logins and no teams.
type knownLogins map[string]bool

func (k knownLogins) KnownUser(login string) bool { return k[login] }
func (k knownLogins) KnownTeam(_, _ string) bool  { return false }

// newSeedClient points a GitHub client at seedBackfillServer.
func newSeedClient(t *testing.T, reviewers ...string) *ghclient.Client {
	t.Helper()
	return ghclient.NewClient(seedBackfillServer(t, reviewers...).URL, "tok")
}

// TestRefreshGitHub_SeedBackfillsOnlyTheKnownReviewers is the counting half of
// the seed's contract: one event per TF-KNOWN requested reviewer, and none for
// the rest. A PR routinely carries reviewers TF has no identity for — an
// outside collaborator, a bot, a team nobody here belongs to — and an event
// for one of those is a task nobody can act on, keyed to a person the router
// cannot route to.
func TestRefreshGitHub_SeedBackfillsOnlyTheKnownReviewers(t *testing.T) {
	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	trackRepos(t, stores, org, []string{"octo/repo"})

	pub := &recordingPreEnqueuedPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	// Five requested reviewers, three of them TF-known — and the unknown ones
	// deliberately sit first, last and in the middle, so an off-by-one in the
	// skip would show up as a wrong id rather than only a wrong count.
	client := newSeedClient(t, "outsider", "bob", "dependabot", "carol", "dave", "nobody")
	known := knownLogins{"bob": true, "carol": true, "dave": true}

	if _, _, err := tr.RefreshGitHub(ctx, client, "", []string{"octo/repo"}, known); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#42")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	rows, err := database.Query(
		`SELECT dedup_key FROM events WHERE entity_id = ? AND event_type = ? ORDER BY dedup_key`,
		ent.ID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		t.Fatalf("read backfilled events: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, key)
	}
	want := []string{reviewerDedupKey("bob"), reviewerDedupKey("carol"), reviewerDedupKey("dave")}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("backfilled %d events (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backfilled event %d keyed %q, want %q", i, got[i], want[i])
		}
	}
	if pre, _ := pub.snapshot(); len(pre) != len(want) {
		t.Errorf("post-commit fan-out carried %d events, want %d", len(pre), len(want))
	}
}
