package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// quietPRListing is one open pull request with no body, so the refresh gate
// compares only updated_at and the head sha against the stored snapshot.
const quietPRListing = `[
  {
    "number": 42,
    "node_id": "PR_quiet",
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
    "requested_teams": []
  }
]`

// TestRefreshGitHub_GatesAnEntityWhoseKeySpellsAnotherName: the Phase-2 gate
// finds a stored pull request in this cycle's discovery by its node id, and
// finds its repository's listing quiet by the registry row its entity
// references. Neither join reads a name, so an entity whose key or snapshot
// spells its repository differently from the tracked name — another casing,
// or the name it had before a rename — still keeps its stored snapshot
// instead of costing a GraphQL refresh every cycle.
func TestRefreshGitHub_GatesAnEntityWhoseKeySpellsAnotherName(t *testing.T) {
	cases := []struct {
		name string
		// key and snapRepo are how the stored entity spells the pull
		// request and its repository.
		key, snapRepo string
		// renamed renames the entity's registry row to octo/repo before it
		// is tracked, leaving the key on the old name.
		renamed bool
		// notModified answers the cycle's listing with a 304, so the gate
		// reads the quiet set; otherwise the listing is a 200 carrying the
		// pull request and the gate reads discovery.
		notModified bool
	}{
		{name: "snapshot in GitHub's casing, quiet listing", key: "octo/repo#42", snapRepo: "Octo/Repo", notModified: true},
		{name: "key on a pre-rename name, listed", key: "octo/old#42", snapRepo: "octo/old", renamed: true},
		{name: "key on a pre-rename name, quiet listing", key: "octo/old#42", snapRepo: "octo/old", renamed: true, notModified: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID
			database := newMigratedSQLite(t)
			stores := sqlitestore.New(database)

			var refreshes, conditionalHits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/graphql"):
					atomic.AddInt32(&refreshes, 1)
					_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
				case strings.HasSuffix(r.URL.Path, "/repos/octo/repo/pulls"):
					if r.Header.Get("If-None-Match") == `"etag-1"` {
						atomic.AddInt32(&conditionalHits, 1)
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Header().Set("ETag", `"etag-2"`)
					_, _ = w.Write([]byte(quietPRListing))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			// The stored pull request: open, refreshed moments ago, CI
			// settled, and identical to the listing in everything the gate
			// compares.
			ent, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", cursorTestHost, tc.key, "PR_quiet", "pr", "Add widget", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if tc.renamed {
				dbseed.RenameRepositoryOnly(t, database, ent.RepositoryID, "octo", "repo")
			}
			trackRepos(t, stores, org, []string{"octo/repo"})
			if row, err := stores.Repos.GetByRefSystem(ctx, org, domain.RepoRefFromSlug(cursorTestHost, "octo/repo")); err != nil || row == nil || row.ID != ent.RepositoryID {
				t.Fatalf("tracked row = %+v, %v; want the entity's row %s", row, err, ent.RepositoryID)
			}
			snap := domain.PRSnapshot{
				NodeID: "PR_quiet", Number: 42, Title: "Add widget", Author: "alice", Repo: tc.snapRepo,
				State: "OPEN", HeadRef: "feature", BaseRef: "main", HeadSHA: "deadbeef",
				CreatedAt: "2026-05-20T10:00:00Z", UpdatedAt: "2026-05-29T12:00:00Z",
				CheckRuns: []domain.CheckRun{},
			}
			raw, _ := json.Marshal(snap)
			if ok, err := stores.Entities.UpdateSnapshotCASSystem(ctx, org, ent.ID, string(raw), ent.PollSeq); err != nil || !ok {
				t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
			}
			if tc.notModified {
				if err := stores.Repos.SetPullsPollStateSystem(ctx, org, ent.RepositoryID, `"etag-1"`, ent.CreatedAt); err != nil {
					t.Fatalf("seed listing cursor: %v", err)
				}
			}

			names, err := stores.Repos.ListTrackedNamesSystem(ctx, org, cursorTestHost)
			if err != nil {
				t.Fatalf("ListTrackedNamesSystem: %v", err)
			}
			tr := New(database, &recordingPublisher{}, stores.Tasks, stores.Entities, stores.Repos, stores.JiraProjects, stores.Artifacts, stores.EventQueue, org)
			if _, _, err := tr.RefreshGitHub(ctx, cursorTestHost, ghclient.NewClient(srv.URL, "tok"), "", names, nil); err != nil {
				t.Fatalf("RefreshGitHub: %v", err)
			}

			if tc.notModified && atomic.LoadInt32(&conditionalHits) != 1 {
				t.Fatalf("conditional hits = %d, want 1 — the listing must go out under the row's stored ETag", conditionalHits)
			}
			if got := atomic.LoadInt32(&refreshes); got != 0 {
				t.Errorf("GraphQL refreshes = %d, want 0 — the gate must match the entity by id and keep its snapshot", got)
			}
			entities, err := stores.Entities.ListActiveSystem(ctx, org, "github")
			if err != nil {
				t.Fatalf("ListActiveSystem: %v", err)
			}
			if len(entities) != 1 || entities[0].ID != ent.ID {
				t.Errorf("active entities = %+v, want only the seeded one", entities)
			}
		})
	}
}
