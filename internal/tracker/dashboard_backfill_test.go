package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// recordingPublisher captures every published event so a test can assert the
// backfill emits none (seed-only, no retroactive tasks).
type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *recordingPublisher) Publish(evt domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, evt)
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// TestBackfillDashboardHistory_SeedsTerminalEntity_NoEvents is the TFAC-396
// multi-mode backfill acceptance: a merged PR returned by the per-login search
// becomes a closed entity carrying its snapshot, the entity does NOT sit in the
// active refresh set, no events are emitted (it's history, not an ask), and a
// repeated pass dedups against the existing entity.
func TestBackfillDashboardHistory_SeedsTerminalEntity_NoEvents(t *testing.T) {
	// One merged PR by octocat. The stub returns it for every search query; the
	// 4 base queries (author/reviewed-by × merged/closed) all resolve to it, so
	// dedup must collapse them to a single seeded entity.
	const searchResp = `{"data":{"search":{"nodes":[
		{"id":"PR_merged7","number":7,"title":"Old merged PR","author":{"login":"octocat"},
		 "state":"MERGED","merged":true,"url":"https://github.com/octo/repo/pull/7",
		 "repository":{"nameWithOwner":"octo/repo"},
		 "createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-20T00:00:00Z","mergedAt":"2026-05-20T00:00:00Z"}
	]}}}`

	var graphqlCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			atomic.AddInt32(&graphqlCalls, 1)
			_, _ = w.Write([]byte(searchResp))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
	client := ghclient.NewClient(srv.URL, "tok")

	n, err := tr.BackfillDashboardHistory(ctx, client, "octocat", []string{"octo/repo"})
	if err != nil {
		t.Fatalf("BackfillDashboardHistory: %v", err)
	}
	if n != 1 {
		t.Fatalf("seeded %d entities; want 1 (deduped across the scoped queries)", n)
	}
	if got := atomic.LoadInt32(&graphqlCalls); got != 4 {
		t.Errorf("graphql calls = %d; want 4 (one per base query for a single repo)", got)
	}

	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#7")
	if err != nil {
		t.Fatalf("GetBySource: %v", err)
	}
	if ent == nil {
		t.Fatal("entity octo/repo#7 was not seeded")
	}
	if ent.State != "closed" {
		t.Errorf("entity state = %q; want closed (terminal backfill)", ent.State)
	}
	if !strings.Contains(ent.SnapshotJSON, `"author":"octocat"`) {
		t.Errorf("snapshot missing author octocat: %s", ent.SnapshotJSON)
	}

	active, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active entities = %d; want 0 (a terminal backfill entity must not sit in the refresh set)", len(active))
	}

	if pub.count() != 0 {
		t.Errorf("published %d events; want 0 (backfill seeds entities, emits nothing)", pub.count())
	}

	// Idempotent: a second pass finds the entity already present and seeds none.
	n2, err := tr.BackfillDashboardHistory(ctx, client, "octocat", []string{"octo/repo"})
	if err != nil {
		t.Fatalf("BackfillDashboardHistory (2nd pass): %v", err)
	}
	if n2 != 0 {
		t.Errorf("2nd pass seeded %d entities; want 0 (already known)", n2)
	}
}

// TestBackfillDashboardHistory_HonorsContextCancellation is the TFAC-396 issue-2
// guard: the backfill runs inside a request-detached goroutine on a bounded
// deadline, so a cancelled/timed-out context must stop it before it issues
// further searches or seed writes rather than running the full query set.
func TestBackfillDashboardHistory_HonorsContextCancellation(t *testing.T) {
	var graphqlCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&graphqlCalls, 1)
		_, _ = w.Write([]byte(`{"data":{"search":{"nodes":[]}}}`))
	}))
	t.Cleanup(srv.Close)

	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, org)
	client := ghclient.NewClient(srv.URL, "tok")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — the loop must bail before the first query

	n, err := tr.BackfillDashboardHistory(ctx, client, "octocat", []string{"octo/repo"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if n != 0 {
		t.Errorf("seeded %d entities under a cancelled context; want 0", n)
	}
	if got := atomic.LoadInt32(&graphqlCalls); got != 0 {
		t.Errorf("graphql calls = %d under a cancelled context; want 0 (bail before issuing searches)", got)
	}
}

// TestDashboardBackfillQueries_ScopesPerRepo pins the shared query construction
// used by both the local per-cycle backfill and the multi-mode on-bind backfill:
// four base queries (author/reviewed-by × merged/closed), each scoped to the
// tracked repos with the explicit login.
func TestDashboardBackfillQueries_ScopesPerRepo(t *testing.T) {
	qs := dashboardBackfillQueries("octocat", []string{"octo/repo"})
	if len(qs) != 4 {
		t.Fatalf("got %d queries; want 4 (one repo fits each base in a single scoped query)", len(qs))
	}
	for _, q := range qs {
		if !strings.Contains(q, "repo:octo/repo") {
			t.Errorf("query %q missing repo scope", q)
		}
		if !strings.Contains(q, "octocat") {
			t.Errorf("query %q missing login", q)
		}
	}
	joined := strings.Join(qs, "\n")
	for _, want := range []string{"author:octocat", "reviewed-by:octocat", "is:merged", "is:closed is:unmerged"} {
		if !strings.Contains(joined, want) {
			t.Errorf("query set missing %q; got:\n%s", want, joined)
		}
	}
}
