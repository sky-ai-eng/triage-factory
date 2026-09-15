package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRefreshGitHub_MirrorsPRBodyIntoDescription covers the discovery arm:
// a newly discovered PR seeds entities.description from its body (capped
// like a Jira description), a later listing with an edited body updates it,
// and the body never lands in snapshot_json.
func TestRefreshGitHub_MirrorsPRBodyIntoDescription(t *testing.T) {
	var (
		mu   sync.Mutex
		body = "  " + strings.Repeat("é", 3000) + "\n"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			// Phase-2 refresh reports the node inaccessible so the
			// discovery snapshot stands and only the discovery arm writes.
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			mu.Lock()
			b := body
			mu.Unlock()
			_, _ = w.Write([]byte(`[{
				"number": 42, "node_id": "PR_42", "title": "Add widget", "state": "open",
				"body": ` + strconv.Quote(b) + `,
				"html_url": "https://github.com/octo/repo/pull/42",
				"user": {"login": "bob"},
				"head": {"sha": "sha1", "ref": "feat"}, "base": {"ref": "main"},
				"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-01T00:00:00Z"
			}]`))
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

	if _, _, err := tr.RefreshGitHub(ctx, client, "", []string{"octo/repo"}, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 1: %v", err)
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#42")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	if n := utf8.RuneCountInString(ent.Description); n != descriptionStoreMaxRunes {
		t.Errorf("seeded description is %d runes; want the %d-rune store cap", n, descriptionStoreMaxRunes)
	}
	if !strings.HasPrefix(ent.Description, "é") || !strings.HasSuffix(ent.Description, "…") {
		t.Errorf("seeded description should be trimmed and end in an ellipsis: %q…%q", ent.Description[:4], ent.Description[len(ent.Description)-4:])
	}
	if strings.Contains(ent.SnapshotJSON, "éé") {
		t.Errorf("snapshot_json carries the PR body; it must stay in-memory only")
	}

	// The author edits the description; the next listing mirrors it.
	mu.Lock()
	body = "Now with a short body."
	mu.Unlock()
	if _, _, err := tr.RefreshGitHub(ctx, client, "", []string{"octo/repo"}, nil); err != nil {
		t.Fatalf("RefreshGitHub cycle 2: %v", err)
	}
	ent, err = stores.Entities.GetBySource(ctx, org, "github", "octo/repo#42")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource after cycle 2: ent=%v err=%v", ent, err)
	}
	if ent.Description != "Now with a short body." {
		t.Errorf("description after edit = %q; want the edited body", ent.Description)
	}
}

// TestRefreshGitHub_PhaseTwoMirrorsPRBodyIntoDescription covers the refresh
// arm: a snapshot-less stub seeded from the GraphQL refresh gets the body,
// and a subsequent diffed refresh with an edited body updates it.
func TestRefreshGitHub_PhaseTwoMirrorsPRBodyIntoDescription(t *testing.T) {
	var (
		mu   sync.Mutex
		body = "Refreshed body"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			mu.Lock()
			b := body
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"nodes":[
				{"id":"PR_node7","number":7,"title":"Stub PR","body":` + strconv.Quote(b) + `,
				 "author":{"login":"bob"},"state":"OPEN","merged":false,
				 "url":"https://github.com/octo/repo/pull/7",
				 "repository":{"nameWithOwner":"octo/repo"},
				 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-10T00:00:00Z"}
			]}}`))
		case strings.Contains(r.URL.Path, "/pulls/7"):
			_, _ = w.Write([]byte(`{
				"number": 7, "node_id": "PR_node7", "title": "Stub PR",
				"state": "open", "merged": false,
				"html_url": "https://github.com/octo/repo/pull/7",
				"user": {"login": "bob"},
				"head": {"sha": "sha7", "ref": "feat"}, "base": {"ref": "main"}
			}`))
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
	if _, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#7", "pr", "", ""); err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	client := ghclient.NewClient(srv.URL, "tok")

	// repos=nil → no discovery; the stub is reached only via Phase-2.
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub seed cycle: %v", err)
	}
	ent, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#7")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
	}
	if ent.Description != "Refreshed body" {
		t.Errorf("stub seed description = %q; want the refreshed body", ent.Description)
	}

	mu.Lock()
	body = "Edited after seed"
	mu.Unlock()
	if _, _, err := tr.RefreshGitHub(ctx, client, "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub diff cycle: %v", err)
	}
	ent, err = stores.Entities.GetBySource(ctx, org, "github", "octo/repo#7")
	if err != nil || ent == nil {
		t.Fatalf("GetBySource after diff cycle: ent=%v err=%v", ent, err)
	}
	if ent.Description != "Edited after seed" {
		t.Errorf("post-diff description = %q; want the edited body", ent.Description)
	}
	if strings.Contains(ent.SnapshotJSON, "Edited after seed") {
		t.Errorf("snapshot_json carries the PR body; it must stay in-memory only")
	}
}
