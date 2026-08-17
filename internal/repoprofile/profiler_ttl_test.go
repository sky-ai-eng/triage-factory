package repoprofile

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// TestRunOrg_TTLSkipsFreshUnlessForced pins the TTL gate the per-poll
// "profiler" subscriber relies on: a repo profiled within the 3-day window
// is skipped on a normal (force=false) cycle — no GitHub client is resolved,
// no row is written, and the cycle reports changed=false so the bare-clone
// bootstrap is skipped. The explicit re-profile (force=true) bypasses the
// TTL and resolves the client (proving it would re-fetch).
func TestRunOrg_TTLSkipsFreshUnlessForced(t *testing.T) {
	fresh := time.Now()
	repos := &ttlRepositoryStore{
		names:   []string{"own/fresh"},
		profile: &domain.Repository{ID: "repo-id-fresh", Owner: "own", Repo: "fresh", ProfiledAt: &fresh},
	}

	t.Run("force=false skips a fresh repo", func(t *testing.T) {
		res := &countingResolver{}
		p := NewProfiler(res, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
		changed, err := p.RunOrg(context.Background(), "org-1", false)
		if err != nil {
			t.Fatalf("RunOrg: %v", err)
		}
		if changed {
			t.Error("changed=true on an all-skipped cycle; want false")
		}
		if n := res.calls.Load(); n != 0 {
			t.Errorf("resolver.ClientFor called %d times for a TTL-skipped repo; want 0", n)
		}
		if n := repos.upserts.Load(); n != 0 {
			t.Errorf("UpsertSystem called %d times for a TTL-skipped repo; want 0", n)
		}
	})

	t.Run("force=true bypasses the TTL", func(t *testing.T) {
		res := &countingResolver{}
		p := NewProfiler(res, nil, nil, repos, oneOrgStore{}, nil, nil, nil)
		// force=true must reach client resolution (it skips the GetSystem TTL
		// read entirely). The resolver errors, so the repo is then skipped
		// before any fetch — enough to prove the TTL gate was bypassed.
		if _, err := p.RunOrg(context.Background(), "org-1", true); err != nil {
			t.Fatalf("RunOrg: %v", err)
		}
		if n := res.calls.Load(); n != 1 {
			t.Errorf("resolver.ClientFor called %d times under force; want 1 (TTL bypassed)", n)
		}
	})
}

// ttlRepositoryStore returns a single configured repo whose GetSystem reports a
// recently-profiled row, so the TTL branch is exercised. UpsertSystem and
// the GetSystem consult are counted.
type ttlRepositoryStore struct {
	db.RepositoryStore
	names   []string
	profile *domain.Repository
	upserts atomic.Int64
}

func (s *ttlRepositoryStore) ListTrackedNamesSystem(context.Context, string) ([]string, error) {
	return s.names, nil
}

func (s *ttlRepositoryStore) GetByRefSystem(context.Context, string, domain.RepoRef) (*domain.Repository, error) {
	return s.profile, nil
}

func (s *ttlRepositoryStore) UpsertSystem(_ context.Context, _ string, p domain.Repository) (domain.Repository, error) {
	s.upserts.Add(1)
	return storedRepoRow(p), nil
}

// countingResolver counts ClientFor calls and always errors, so a forced
// cycle reaches client resolution but skips the repo before any GitHub
// fetch — isolating the "did the TTL gate let us through" signal.
type countingResolver struct {
	github.Resolver
	calls atomic.Int64
}

func (r *countingResolver) ClientFor(context.Context, string, string) (*github.Client, error) {
	r.calls.Add(1)
	return nil, errResolverDown
}

var (
	errResolverDown                    = stubErr("resolver unavailable in ttl test")
	_               db.RepositoryStore = (*ttlRepositoryStore)(nil)
	_               github.Resolver    = (*countingResolver)(nil)
)
