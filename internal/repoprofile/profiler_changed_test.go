package repoprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// TestRunOrg_ChangedTracksUpsertSuccess pins that the cycle's changed flag
// reflects whether a row was *successfully persisted*, not merely attempted:
// a genuine-404 repo whose UpsertSystem succeeds yields changed=true, while
// the same repo with a failing UpsertSystem yields changed=false (so the
// Runner skips the bare-clone bootstrap for a clone_url that never landed).
func TestRunOrg_ChangedTracksUpsertSuccess(t *testing.T) {
	// All docs 404 (genuine absence) but repo meta succeeds, so the repo
	// reaches the docs-flags upsert without entering the AI-batch path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/contents/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/own/nodocs.git"}`))
	}))
	defer srv.Close()

	newProfiler := func(repos db.RepoStore) *Profiler {
		return NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil)
	}

	t.Run("successful upsert → changed=true", func(t *testing.T) {
		repos := &changeRepoStore{names: []string{"own/nodocs"}}
		changed, err := newProfiler(repos).RunOrg(context.Background(), "org-1", true)
		if err != nil {
			t.Fatalf("RunOrg: %v", err)
		}
		if !changed {
			t.Error("changed=false after a successful upsert; want true")
		}
	})

	t.Run("failed upsert → changed=false", func(t *testing.T) {
		repos := &changeRepoStore{names: []string{"own/nodocs"}, upsertErr: errUpsertDown}
		changed, err := newProfiler(repos).RunOrg(context.Background(), "org-1", true)
		if err != nil {
			t.Fatalf("RunOrg: %v", err)
		}
		if changed {
			t.Error("changed=true even though every upsert failed; want false")
		}
	})
}

// changeRepoStore serves a fixed configured-name list and lets the test make
// UpsertSystem fail, to exercise the changed/touched accounting.
type changeRepoStore struct {
	db.RepoStore
	names     []string
	upsertErr error
	upserts   atomic.Int64
}

func (s *changeRepoStore) ListConfiguredNamesSystem(context.Context, string) ([]string, error) {
	return s.names, nil
}

func (s *changeRepoStore) GetSystem(context.Context, string, string) (*domain.RepoProfile, error) {
	return nil, nil
}

func (s *changeRepoStore) UpsertSystem(context.Context, string, domain.RepoProfile) error {
	s.upserts.Add(1)
	return s.upsertErr
}

var (
	errUpsertDown              = stubErr("simulated repo-store upsert outage")
	_             db.RepoStore = (*changeRepoStore)(nil)
)
