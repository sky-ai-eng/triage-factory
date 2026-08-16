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

// TestProfiler_FetchErrorLeavesRowUntouched pins TFAC-331 root cause 2: a repo
// whose doc fetch ERRORS (auth / network / 5xx — anything that isn't a clean
// 404) is skipped entirely — no UpsertSystem, so no docs classification — and
// its row is left for the next run to retry, while a repo with genuine 404s is
// persisted with false has_* flags exactly as before. A no-upsert proves the
// erroring repo lands in neither withDocs nor withoutDocs (both end in an
// UpsertSystem call).
func TestProfiler_FetchErrorLeavesRowUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// github.NewClient routes through APIBase → requests land under
		// /api/v3/repos/{owner}/{repo}[/contents/{file}].
		path := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		switch {
		case strings.HasPrefix(path, "bad/errors"):
			// README 500s; CLAUDE/AGENTS still get requested and 404 (all four
			// fetches always run). The errors.Join skip fires after they
			// complete but before any upsert, so those 404 results never matter
			// — one non-nil error is enough to skip the whole repo.
			if strings.HasSuffix(path, "/contents/README.md") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if strings.Contains(path, "/contents/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/bad/errors.git"}`))
		case strings.HasPrefix(path, "good/nodocs"):
			if strings.Contains(path, "/contents/") {
				w.WriteHeader(http.StatusNotFound) // genuine absence — a real 404
				return
			}
			_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/good/nodocs.git"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	repos := &fetchRepositoryStore{names: []string{"bad/errors", "good/nodocs"}}
	p := NewProfiler(
		fixedResolver{client: github.NewClient(srv.URL, "tok")},
		nil, // secrets unused: no with-docs repo reaches profileBatch
		nil, // llmResolve nil: tests use Run's built-in resolution
		repos,
		oneOrgStore{},
		nil, // recorder nil: a nil recorder makes Record a no-op
		nil, // limiter nil: a nil limiter is an unlimited no-op
		nil, // ws nil: the profiler guards every Broadcast on non-nil
	)

	if err := p.Run(context.Background(), true /* force: bypass the TTL/GetSystem read */); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := repos.upsertedByID()

	// Mixed batch: only the erroring repo is skipped. Its row is left
	// untouched so the next run retries — never overwritten with false flags.
	if p, ok := got["bad/errors"]; ok {
		t.Errorf("erroring repo was upserted (%+v); want skipped (row left for retry)", p)
	}

	// The genuine-404 repo IS persisted, with all has_* flags false (correct —
	// it really has no docs) and metadata from the successful repo-meta fetch.
	nodocs, ok := got["good/nodocs"]
	if !ok {
		t.Fatal("genuine-404 repo was not upserted; want persisted with false flags")
	}
	if nodocs.HasReadme || nodocs.HasClaudeMd || nodocs.HasAgentsMd {
		t.Errorf("404 repo flags = readme:%v claude:%v agents:%v; want all false",
			nodocs.HasReadme, nodocs.HasClaudeMd, nodocs.HasAgentsMd)
	}
	if nodocs.DefaultBranch != "main" {
		t.Errorf("404 repo default_branch = %q; want %q (from repo meta)", nodocs.DefaultBranch, "main")
	}
	// A successfully-inspected docs-less repo is stamped profiled_at so the
	// TTL gate skips it next cycle instead of re-fetching all four files on
	// every poll (the profiler runs per-cycle now, not just on config change).
	if nodocs.ProfiledAt == nil {
		t.Error("404 repo profiled_at is nil; want stamped so the TTL gate skips it next cycle")
	}
}

// --- test doubles (distinct from profiler_perorg_test.go's fakes) ---

// oneOrgStore drives the per-org outer loop with a single active org and stub
// settings — enough to reach runOrg's per-repo body without a real DB.
type oneOrgStore struct {
	db.OrgsStore // embed nil: any unexpected method call panics loudly
}

func (oneOrgStore) ListActiveSystem(context.Context) ([]string, error) {
	return []string{"org-1"}, nil
}

func (oneOrgStore) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, nil // GitHubCloneProtocol "" → HTTPS
}

// fetchRepositoryStore returns a fixed configured-name list and captures every
// UpsertSystem so the test can assert which repos were persisted vs skipped.
type fetchRepositoryStore struct {
	db.RepositoryStore
	names   []string
	mu      sync.Mutex
	upserts []domain.Repository
}

func (s *fetchRepositoryStore) ListTrackedNamesSystem(context.Context, string) ([]string, error) {
	return s.names, nil
}

// GetByRefSystem answers with a row carrying a registry id — the real store's
// shape, since a tracked repo has a row before the profiler ever sees its
// name. A nil here would read as "untracked mid-pass" and skip the repo
// before any fetch, and every test built on this double would assert against
// an empty run. ProfiledAt stays unset, so the TTL gate never skips.
func (s *fetchRepositoryStore) GetByRefSystem(_ context.Context, _ string, ref domain.RepoRef) (*domain.Repository, error) {
	return &domain.Repository{ID: "repo-id-" + ref.Repo, Owner: ref.Owner, Repo: ref.Repo}, nil
}

func (s *fetchRepositoryStore) UpsertSystem(_ context.Context, _ string, p domain.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, p)
	return nil
}

func (s *fetchRepositoryStore) upsertedByID() map[string]domain.Repository {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]domain.Repository, len(s.upserts))
	for _, p := range s.upserts {
		m[p.Slug()] = p
	}
	return m
}

// fixedResolver hands every ClientFor call the same httptest-backed client.
// The profiler only ever calls ClientFor in this path.
type fixedResolver struct {
	github.Resolver
	client *github.Client
}

func (f fixedResolver) ClientFor(context.Context, string, string) (*github.Client, error) {
	return f.client, nil
}

var (
	_ db.OrgsStore       = oneOrgStore{}
	_ db.RepositoryStore = (*fetchRepositoryStore)(nil)
	_ github.Resolver    = fixedResolver{}
)
