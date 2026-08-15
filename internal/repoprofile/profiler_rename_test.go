package repoprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// A PAT org has no installation grant listing, so the poller's per-cycle
// rename signal does not exist for it. What every org has is this: GitHub
// answers a request for a renamed repository by redirecting to its current
// location, so the meta response the profiler already fetches carries the name
// the repository goes by now.
//
// Two things must follow from that. The rename is applied — so every stored
// reference moves — and the profile row this pass writes lands on the MOVED
// row, under the new slug, rather than re-creating the old one.
func TestProfiler_AppliesARenameTheMetaResponseRevealed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		if strings.Contains(path, "/contents/") {
			w.WriteHeader(http.StatusNotFound) // docs-less: straight to the upsert
			return
		}
		switch {
		case strings.HasPrefix(path, "octo/api"):
			// The redirect GitHub serves for a renamed repository, as the Go
			// client sees it: the body of the repository's current location.
			_, _ = w.Write([]byte(`{"id":1296269,"full_name":"octo/platform-api","default_branch":"main","clone_url":"https://x/octo/platform-api.git"}`))
		case strings.HasPrefix(path, "octo/web"):
			_, _ = w.Write([]byte(`{"id":555,"full_name":"octo/web","default_branch":"main","clone_url":"https://x/octo/web.git"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	repos := &renameProfileStore{
		fetchRepoStore: fetchRepoStore{names: []string{"octo/api", "octo/web"}},
	}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
	if err := p.Run(context.Background(), true /* force: skip the TTL read */); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(repos.renames) != 1 {
		t.Fatalf("renames = %+v, want exactly the repository whose name moved", repos.renames)
	}
	if got := repos.renames[0]; got.Slug() != "octo/platform-api" || got.ExternalID != "1296269" {
		t.Errorf("rename = %+v, want octo/platform-api keyed on 1296269", got)
	}

	upserts := repos.upsertedByID()
	if _, stale := upserts["octo/api"]; stale {
		t.Error("the profile was written under the old slug; the upsert must follow the rename, not undo it")
	}
	moved, ok := upserts["octo/platform-api"]
	if !ok {
		t.Fatalf("no profile written under the new slug; got %v", keysOf(upserts))
	}
	if moved.Owner != "octo" || moved.Repo != "platform-api" {
		t.Errorf("profile owner/repo = %s/%s, want octo/platform-api", moved.Owner, moved.Repo)
	}
	if _, ok := upserts["octo/web"]; !ok {
		t.Error("the unrenamed repository was not profiled")
	}
}

// The ordinary case: the name GitHub answers with is the name that was asked
// for. Nothing is a rename, and the store is never asked to open a
// transaction — this runs on every profiled repository.
func TestProfiler_MatchingNameIsNotARename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Casing differs from the tracked spelling, which is not a rename:
		// GitHub identifiers are case-insensitive and stored casing is sticky.
		_, _ = w.Write([]byte(`{"id":1296269,"full_name":"Octo/API","default_branch":"main","clone_url":"https://x/octo/api.git"}`))
	}))
	defer srv.Close()

	repos := &renameProfileStore{fetchRepoStore: fetchRepoStore{names: []string{"octo/api"}}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
	if err := p.Run(context.Background(), true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(repos.renames) != 0 {
		t.Errorf("renames = %+v, want none", repos.renames)
	}
	if _, ok := repos.upsertedByID()["octo/api"]; !ok {
		t.Errorf("profile was not written under the tracked slug; got %v", keysOf(repos.upsertedByID()))
	}
}

// A GHES or mock that omits full_name leaves the profiler with nothing to
// compare, and "no answer" must never read as "renamed to nothing."
func TestProfiler_AbsentFullNameIsNotARename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":1296269,"default_branch":"main","clone_url":"https://x/octo/api.git"}`))
	}))
	defer srv.Close()

	repos := &renameProfileStore{fetchRepoStore: fetchRepoStore{names: []string{"octo/api"}}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
	if err := p.Run(context.Background(), true); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(repos.renames) != 0 {
		t.Errorf("renames = %+v, want none", repos.renames)
	}
	if _, ok := repos.upsertedByID()["octo/api"]; !ok {
		t.Error("profile was not written under the tracked slug")
	}
}

// renameProfileStore is fetchRepoStore plus the two rename methods, recording
// what the profiler asked the store to move.
type renameProfileStore struct {
	fetchRepoStore
	mu      sync.Mutex
	renames []domain.RepoRef
}

func (s *renameProfileStore) ListIdentitiesSystem(context.Context, string) ([]domain.RepoRef, error) {
	// The stored identities the observation is compared against: TF tracks
	// octo/api under id 1296269, which is what makes the meta response's
	// different name a rename rather than a new repository.
	return []domain.RepoRef{
		{Source: domain.RepoSourceGitHub, Owner: "octo", Repo: "api", ExternalID: "1296269"},
		{Source: domain.RepoSourceGitHub, Owner: "octo", Repo: "web", ExternalID: "555"},
	}, nil
}

func (s *renameProfileStore) RenameSystem(_ context.Context, _ string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renames = append(s.renames, observed)
	return domain.RepoRenameOutcome{Renamed: true, From: "octo/api", To: observed.Slug()}, nil
}

func keysOf(m map[string]domain.RepoProfile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ db.RepoStore = (*renameProfileStore)(nil)
