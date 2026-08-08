package tracker

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const openPRBody = `[
  {
    "number": 42,
    "node_id": "PR_kwDOabc",
    "title": "Add widget",
    "state": "open",
    "draft": false,
    "html_url": "https://github.com/octo/repo/pull/42",
    "created_at": "2026-05-20T10:00:00Z",
    "updated_at": "2026-05-29T12:00:00Z",
    "user": {"login": "alice"},
    "head": {"sha": "deadbeef", "ref": "feature", "repo": {"full_name": "octo/repo"}},
    "base": {"ref": "main", "repo": {"full_name": "octo/repo"}},
    "requested_reviewers": [],
    "requested_teams": [],
    "labels": []
  }
]`

// TestRefreshGitHub_RESTDiscovery_SeedsEntityAndConditionalSkips is the
// integration-level acceptance for the discovery rewrite:
//
//   - PAT-perspective discovery enumerates the configured repo set via REST
//     state=open and a new PR becomes an entity (not "PRs involving me").
//   - The repo's ETag is persisted for the next cycle.
//   - The next cycle replays If-None-Match; a quiet repo returns 304 and is
//     skipped (no new entity, no re-list of the body).
//
// Phase-2 GraphQL refresh is stubbed to return null nodes (the backstop is
// exercised by the tracker's other tests); this test isolates the Phase-1
// REST enumeration + conditional-request plumbing.
func TestRefreshGitHub_RESTDiscovery_SeedsEntityAndConditionalSkips(t *testing.T) {
	var listCalls, conditionalHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			// Phase-2 RefreshPRs: report the node as inaccessible so the
			// refresh is a no-op and the seeded discovery snapshot stands.
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			atomic.AddInt32(&listCalls, 1)
			if r.Header.Get("If-None-Match") == `"etag-1"` {
				atomic.AddInt32(&conditionalHits, 1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"etag-1"`)
			_, _ = w.Write([]byte(openPRBody))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	if err := stores.Repos.SetConfigured(ctx, org, []string{"octo/repo"}); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	bus := eventbus.New()
	t.Cleanup(bus.Close)

	tr := New(database, busPublisher{bus: bus}, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")

	// Cycle 1: REST 200 → the PR becomes an entity. username="" exercises
	// the App-parity path (no user-perspective search, no dashboard backfill).
	if _, _, err := tr.RefreshGitHub(context.Background(), client, "", []string{"octo/repo"}, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 1: %v", err)
	}

	ents, err := stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("after cycle 1: %d active entities; want 1 (the discovered open PR)", len(ents))
	}
	if ents[0].SourceID != "octo/repo#42" {
		t.Errorf("entity SourceID = %q; want octo/repo#42", ents[0].SourceID)
	}

	// ETag persisted for conditional re-poll next cycle.
	etag, polledAt, err := stores.Repos.GetPullsPollStateSystem(ctx, org, "octo/repo")
	if err != nil {
		t.Fatalf("GetPullsPollStateSystem: %v", err)
	}
	if etag != `"etag-1"` {
		t.Errorf("persisted etag = %q; want %q", etag, `"etag-1"`)
	}
	if polledAt == nil {
		t.Errorf("pulls_polled_at not stamped after a 200 list")
	}

	// Cycle 2: REST replays If-None-Match → 304 → quiet repo, no new entity.
	if _, _, err := tr.RefreshGitHub(context.Background(), client, "", []string{"octo/repo"}, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 2: %v", err)
	}
	if got := atomic.LoadInt32(&conditionalHits); got != 1 {
		t.Errorf("conditional 304 hits = %d; want 1 (cycle 2 must send If-None-Match and get 304)", got)
	}
	if got := atomic.LoadInt32(&listCalls); got != 2 {
		t.Errorf("listing calls = %d; want 2 (one per cycle)", got)
	}
	ents, err = stores.Entities.ListActiveSystem(ctx, org, "github")
	if err != nil {
		t.Fatalf("ListActiveSystem after cycle 2: %v", err)
	}
	if len(ents) != 1 {
		t.Errorf("after cycle 2: %d active entities; want 1 (304 must not duplicate)", len(ents))
	}
}

func newMigratedSQLite(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}
