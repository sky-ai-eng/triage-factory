package reporename

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var testLog = logging.Component("reporename-test")

// testHost is the org's current GitHub host in these tests, and otherHost a
// second deployment whose repository ids are a separate sequence.
const (
	testHost  = dbtest.TestGitHubHost
	otherHost = "https://ghe.example.com"
)

func TestApply_RenamesOnlyWhatMoved(t *testing.T) {
	store := &renameStore{
		stored: []domain.RepoRef{
			{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
			{Source: "github", Host: testHost, Owner: "octo", Repo: "web", ExternalID: "2"},
		},
		outcomes: map[string]domain.RepoRenameOutcome{
			"1": {Renamed: true, From: "octo/api", To: "octo/platform-api"},
		},
	}
	resolver := &coverageResolver{}

	n := Apply(context.Background(), store, resolver, testLog, "org-1", testHost, []domain.RepoRef{
		{Owner: "octo", Repo: "platform-api", ExternalID: "1"},
		{Owner: "octo", Repo: "web", ExternalID: "2"},     // unchanged
		{Owner: "octo", Repo: "unknown", ExternalID: "9"}, // TF has no row
	})
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	if len(store.attempts) != 1 || store.attempts[0].ExternalID != "1" {
		t.Fatalf("attempts = %+v, want only the moved repository", store.attempts)
	}
	// The cache keys on the slug, so both entries now vouch for the wrong
	// repository — the old slug's for one that no longer answers to that name,
	// the new slug's for whatever was called that before — and both are evicted.
	want := []string{"octo/api", "octo/platform-api"}
	if len(resolver.forgotten) != 2 || resolver.forgotten[0] != want[0] || resolver.forgotten[1] != want[1] {
		t.Errorf("evicted %v, want %v", resolver.forgotten, want)
	}
	// The observation is stamped with the host it was read from, which is the
	// host the store renames on and the host whose cache entries are evicted.
	if store.attempts[0].Host != testHost {
		t.Errorf("rename attempted on host %q, want %q", store.attempts[0].Host, testHost)
	}
	for i, h := range resolver.hosts {
		if h != testHost {
			t.Errorf("eviction %d on host %q, want %q", i, h, testHost)
		}
	}
	if len(store.listedHosts) != 1 || store.listedHosts[0] != testHost {
		t.Errorf("identities read for hosts %v, want [%s]", store.listedHosts, testHost)
	}
}

// Repository ids are per-deployment sequences, so the same external id on two
// hosts names two different repositories. An observation read from one host
// must never rename the other host's row, even when that row is the only one
// carrying the id. The fake hands back every stored row whatever host is asked
// for, so this holds on Apply's own comparison and not only on the store's
// filter.
func TestApply_NeverMatchesARepositoryOnAnotherHost(t *testing.T) {
	t.Run("the id is held only on another host", func(t *testing.T) {
		store := &renameStore{
			stored: []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
			outcomes: map[string]domain.RepoRenameOutcome{
				"1": {Renamed: true, From: "octo/api", To: "acme/platform"},
			},
		}
		resolver := &coverageResolver{}
		if n := Apply(context.Background(), store, resolver, testLog, "org-1", otherHost,
			[]domain.RepoRef{{Owner: "acme", Repo: "platform", ExternalID: "1"}}); n != 0 {
			t.Errorf("applied = %d, want 0", n)
		}
		if len(store.attempts) != 0 {
			t.Errorf("attempted %+v; an id observed on %s matched the row on %s", store.attempts, otherHost, testHost)
		}
		if len(resolver.forgotten) != 0 {
			t.Errorf("evicted %v with nothing renamed", resolver.forgotten)
		}
		if len(store.listedHosts) != 1 || store.listedHosts[0] != otherHost {
			t.Errorf("identities read for hosts %v, want [%s]", store.listedHosts, otherHost)
		}
	})

	t.Run("both hosts hold the id", func(t *testing.T) {
		store := &renameStore{
			stored: []domain.RepoRef{
				{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"},
				{Source: "github", Host: otherHost, Owner: "acme", Repo: "api", ExternalID: "1"},
			},
			outcomes: map[string]domain.RepoRenameOutcome{
				"1": {Renamed: true, From: "octo/api", To: "octo/platform-api"},
			},
		}
		resolver := &coverageResolver{}

		// otherHost's repository still answers to the name it is stored under,
		// so nothing on otherHost moved — even though testHost's row with the
		// same id is called something else.
		if n := Apply(context.Background(), store, resolver, testLog, "org-1", otherHost,
			[]domain.RepoRef{{Owner: "acme", Repo: "api", ExternalID: "1"}}); n != 0 {
			t.Errorf("applied = %d on %s's steady state, want 0", n, otherHost)
		}
		if len(store.attempts) != 0 {
			t.Fatalf("attempted %+v on %s's steady state", store.attempts, otherHost)
		}

		// testHost's repository did move, and only its row is renamed.
		if n := Apply(context.Background(), store, resolver, testLog, "org-1", testHost,
			[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		if len(store.attempts) != 1 || store.attempts[0].Host != testHost || store.attempts[0].Slug() != "octo/platform-api" {
			t.Fatalf("attempts = %+v, want one rename to octo/platform-api on %s", store.attempts, testHost)
		}
		for i, h := range resolver.hosts {
			if h != testHost {
				t.Errorf("eviction %d on host %q, want %q", i, h, testHost)
			}
		}
	})
}

func TestApply_SteadyStateCostsNoTransaction(t *testing.T) {
	// Every cycle after the rename observes the same thing the store already
	// holds. Detection is a map lookup, so the steady state must not open a
	// transaction per repository per cycle.
	store := &renameStore{stored: []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}}}

	if n := Apply(context.Background(), store, &coverageResolver{}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "Octo", Repo: "API", ExternalID: "1"}}); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
	if len(store.attempts) != 0 {
		t.Errorf("opened %d rename transactions in the steady state, want 0", len(store.attempts))
	}
}

func TestApply_LosingIsNotAnError(t *testing.T) {
	// A candidate that went stale between the read and the transaction — the
	// other detector got there first — reports Renamed=false. That is the
	// loser contract, and it must not be counted or retried.
	store := &renameStore{
		stored:   []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
		outcomes: map[string]domain.RepoRenameOutcome{"1": {Renamed: false}},
	}
	resolver := &coverageResolver{}

	if n := Apply(context.Background(), store, resolver, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 0 {
		t.Errorf("applied = %d, want 0 — losing is terminal, not a rename", n)
	}
	if len(resolver.forgotten) != 0 {
		t.Errorf("evicted %v on a no-op; nothing moved", resolver.forgotten)
	}
}

func TestApply_RefusalAndFailureAreSurvivable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"occupied target", db.ErrRepoSlugOccupied},
		{"ambiguous identity", db.ErrRepoIdentityAmbiguous},
		{"transient store failure", errors.New("db is having a day")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &renameStore{
				stored:  []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
				failure: tc.err,
			}
			if n := Apply(context.Background(), store, &coverageResolver{}, testLog, "org-1", testHost,
				[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 0 {
				t.Errorf("applied = %d, want 0", n)
			}
			// Reaching here is the assertion: the caller's real work — a poll
			// cycle, a profiling pass — is never failed by this.
		})
	}
}

func TestApply_ReadFailureIsSurvivable(t *testing.T) {
	store := &renameStore{listErr: errors.New("db is having a day")}
	if n := Apply(context.Background(), store, &coverageResolver{}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
	if len(store.attempts) != 0 {
		t.Errorf("attempted a rename on an unreadable identity set: %+v", store.attempts)
	}
}

func TestApply_NoObservationsNoRead(t *testing.T) {
	store := &renameStore{}
	if n := Apply(context.Background(), store, &coverageResolver{}, testLog, "org-1", testHost, nil); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
	if store.listCalls != 0 {
		t.Errorf("read identities %d times with nothing observed, want 0", store.listCalls)
	}
	if n := Apply(context.Background(), nil, &coverageResolver{}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "api", ExternalID: "1"}}); n != 0 {
		t.Errorf("applied = %d with a nil store, want 0", n)
	}
}

func TestApply_ResolverWithoutTheExtensionIsFine(t *testing.T) {
	// The invalidator is an optional Resolver extension; a fake that doesn't
	// implement it (and a nil resolver) must not panic the rename.
	store := &renameStore{
		stored:   []domain.RepoRef{{Source: "github", Host: testHost, Owner: "octo", Repo: "api", ExternalID: "1"}},
		outcomes: map[string]domain.RepoRenameOutcome{"1": {Renamed: true, From: "octo/api", To: "octo/platform-api"}},
	}
	if n := Apply(context.Background(), store, plainResolver{}, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 1 {
		t.Errorf("applied = %d, want 1", n)
	}
	store.attempts = nil
	if n := Apply(context.Background(), store, nil, testLog, "org-1", testHost,
		[]domain.RepoRef{{Owner: "octo", Repo: "platform-api", ExternalID: "1"}}); n != 1 {
		t.Errorf("applied = %d with a nil resolver, want 1", n)
	}
}

// renameStore embeds the interface (nil) so any method this path does not use
// panics loudly rather than silently returning a zero value.
type renameStore struct {
	db.RepositoryStore
	stored    []domain.RepoRef
	outcomes  map[string]domain.RepoRenameOutcome
	failure   error
	listErr   error
	listCalls int
	// listedHosts is the host each identity read asked for. The read returns
	// every stored row whatever the host, so a test can tell Apply's own host
	// comparison from the store's filter.
	listedHosts []string
	attempts    []domain.RepoRef
}

func (s *renameStore) ListIdentitiesSystem(_ context.Context, _, host string) ([]domain.RepoRef, error) {
	s.listCalls++
	s.listedHosts = append(s.listedHosts, host)
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.stored, nil
}

func (s *renameStore) RenameSystem(_ context.Context, _ string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	s.attempts = append(s.attempts, observed)
	if s.failure != nil {
		return domain.RepoRenameOutcome{}, s.failure
	}
	return s.outcomes[observed.ExternalID], nil
}

// coverageResolver is a Resolver fake that also implements the optional
// coverage-invalidation extension, recording the slugs it was asked to evict
// and the host of each.
type coverageResolver struct {
	plainResolver
	forgotten []string
	hosts     []string
}

func (r *coverageResolver) InvalidateRepoCoverage(_, host, owner, repo string) {
	r.forgotten = append(r.forgotten, owner+"/"+repo)
	r.hosts = append(r.hosts, host)
}

var _ ghclient.RepoCoverageInvalidator = (*coverageResolver)(nil)

// plainResolver implements ghclient.Resolver and nothing else — the fake shape
// most of the product's tests use.
type plainResolver struct{}

func (plainResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return nil, errors.New("not wired")
}
func (plainResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return nil, errors.New("not wired")
}
func (plainResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("not wired")
}
func (plainResolver) BaseURLFor(context.Context, string) (string, error) {
	return "", errors.New("not wired")
}
func (plainResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}
