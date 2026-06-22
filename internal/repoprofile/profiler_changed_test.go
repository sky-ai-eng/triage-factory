package repoprofile

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
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

// TestRunOrg_ChangedTracksBatchUpserts covers the changed accounting on the
// with-docs paths (Copilot's batch-fallback and final-upsert comments): a
// repo carrying docs flows into the Haiku batch, so its row is written by
// either the fallback upsert (batch errored) or the final per-repo upsert
// (batch succeeded). In every subtest the docs-flags upsert is forced to
// fail, isolating whether the *batch-path* upsert is what flips changed.
func TestRunOrg_ChangedTracksBatchUpserts(t *testing.T) {
	// README present (non-404) so the repo carries docs and enters the batch.
	// The GitHub contents API returns a base64 envelope, not raw bytes.
	readmeBody := `{"content":"` + base64.StdEncoding.EncodeToString([]byte("# readme")) + `","encoding":"base64"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/README.md"):
			_, _ = w.Write([]byte(readmeBody))
		case strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/own/withdocs.git"}`))
		}
	}))
	defer srv.Close()

	batchOK := func(_ context.Context, _ string, batch []repoWithDocs, _ agentproc.SecretsReader) ([]repoProfileResult, error) {
		out := make([]repoProfileResult, len(batch))
		for i, d := range batch {
			out[i] = repoProfileResult{Repo: d.profile.ID, Profile: "a profile"}
		}
		return out, nil
	}
	batchFail := func(context.Context, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		return nil, stubErr("simulated batch failure")
	}

	cases := []struct {
		name       string
		batchFn    func(context.Context, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error)
		failUpsert func(call int) bool // docs-flags is call 1; the batch-path upsert is call 2
		want       bool
	}{
		{"batch fails, fallback upsert succeeds", batchFail, func(c int) bool { return c == 1 }, true},
		{"batch succeeds, final upsert succeeds", batchOK, func(c int) bool { return c == 1 }, true},
		{"batch succeeds, every upsert fails", batchOK, func(int) bool { return true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := &batchRepoStore{names: []string{"own/withdocs"}, failUpsert: tc.failUpsert}
			p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil)
			p.batchFn = tc.batchFn
			changed, err := p.RunOrg(context.Background(), "org-1", true)
			if err != nil {
				t.Fatalf("RunOrg: %v", err)
			}
			if changed != tc.want {
				t.Errorf("changed = %v; want %v", changed, tc.want)
			}
		})
	}
}

// batchRepoStore serves a fixed configured-name list with a fresh
// (never-profiled) row and fails UpsertSystem calls selected by failUpsert,
// so a test can fail the docs-flags write while letting a later batch-path
// write through.
type batchRepoStore struct {
	db.RepoStore
	names      []string
	failUpsert func(call int) bool
	calls      atomic.Int64
}

func (s *batchRepoStore) ListConfiguredNamesSystem(context.Context, string) ([]string, error) {
	return s.names, nil
}

func (s *batchRepoStore) GetSystem(context.Context, string, string) (*domain.RepoProfile, error) {
	return nil, nil
}

func (s *batchRepoStore) UpsertSystem(context.Context, string, domain.RepoProfile) error {
	n := int(s.calls.Add(1))
	if s.failUpsert != nil && s.failUpsert(n) {
		return errUpsertDown
	}
	return nil
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
