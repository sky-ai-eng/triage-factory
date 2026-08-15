package poller

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// The installation grant is the poller's rename signal: it carries every
// granted repo's id alongside the name GitHub uses for it right now, so the
// condition — same id, different name — is readable with no request of its
// own.
//
// Scope is the thing to get right, and it is the OPPOSITE of recordRepoIDs'.
// The id fill follows the tracked∩granted intersection because it needs a row
// to write; rename detection must run over the whole grant, because the
// renamed repository is exactly the one the intersection can no longer see.
func TestApplyRepoRenames_ReadsTheWholeGrant(t *testing.T) {
	repos := &renamePollStore{
		stored: []domain.RepoRef{
			{Source: "github", Owner: "octo", Repo: "api", ExternalID: "1"},
			{Source: "github", Owner: "octo", Repo: "web", ExternalID: "2"},
		},
	}
	m := &Manager{repos: repos}

	n := m.applyRepoRenames(context.Background(), "org-1", []ghclient.UserRepo{
		{ID: 1, FullName: "octo/platform-api"}, // renamed — TF still tracks octo/api
		{ID: 2, FullName: "octo/web"},          // unchanged
		{ID: 3, FullName: "octo/untracked"},    // granted, no row
		{FullName: "octo/idless"},              // no id in the payload
	})
	if n != 1 {
		t.Fatalf("applied = %d, want 1", n)
	}
	if len(repos.renamed) != 1 || repos.renamed[0].Slug() != "octo/platform-api" {
		t.Fatalf("renames = %+v, want only octo/api -> octo/platform-api", repos.renamed)
	}
	if repos.renamed[0].ExternalID != "1" {
		t.Errorf("external id = %q, want 1 — the identity a rename does not move", repos.renamed[0].ExternalID)
	}
	if repos.renamed[0].Source != domain.RepoSourceGitHub {
		t.Errorf("source = %q, want %q", repos.renamed[0].Source, domain.RepoSourceGitHub)
	}
}

// A grant with nothing renamed in it is the steady state, which is every cycle
// after the first. It must not open a transaction.
func TestApplyRepoRenames_SteadyStateWritesNothing(t *testing.T) {
	repos := &renamePollStore{stored: []domain.RepoRef{{Source: "github", Owner: "octo", Repo: "api", ExternalID: "1"}}}
	m := &Manager{repos: repos}

	if n := m.applyRepoRenames(context.Background(), "org-1",
		[]ghclient.UserRepo{{ID: 1, FullName: "octo/api"}}); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
	if len(repos.renamed) != 0 {
		t.Errorf("wrote %+v in the steady state, want nothing", repos.renamed)
	}
}

// A poller wired without a repo store (the shape some tests construct) must not
// panic on the way through.
func TestApplyRepoRenames_NilStore(t *testing.T) {
	m := &Manager{}
	if n := m.applyRepoRenames(context.Background(), "org-1",
		[]ghclient.UserRepo{{ID: 1, FullName: "octo/api"}}); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
}

// After a rename the tracked set the cycle read at the top is stale by exactly
// one name, so the intersection below would skip the repository until the next
// cycle. Re-reading closes that gap; a failed re-read keeps the list it had.
func TestReloadConfiguredRepos(t *testing.T) {
	repos := &renamePollStore{configured: []string{"octo/platform-api", "octo/web"}}
	m := &Manager{repos: repos}

	got := m.reloadConfiguredRepos(context.Background(), "org-1", []string{"octo/api", "octo/web"})
	if len(got) != 2 || got[0] != "octo/platform-api" {
		t.Errorf("reloaded = %v, want the refreshed tracked set", got)
	}

	repos.configuredErr = context.DeadlineExceeded
	current := []string{"octo/api", "octo/web"}
	if got := m.reloadConfiguredRepos(context.Background(), "org-1", current); len(got) != 2 || got[0] != "octo/api" {
		t.Errorf("reloaded = %v on a read failure, want the caller's list unchanged", got)
	}
}

// renamePollStore embeds the interface (nil) so any method this path does not
// use panics loudly rather than silently returning a zero value.
type renamePollStore struct {
	db.RepoStore
	stored        []domain.RepoRef
	renamed       []domain.RepoRef
	configured    []string
	configuredErr error
}

func (s *renamePollStore) ListIdentitiesSystem(context.Context, string) ([]domain.RepoRef, error) {
	return s.stored, nil
}

func (s *renamePollStore) RenameSystem(_ context.Context, _ string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	s.renamed = append(s.renamed, observed)
	return domain.RepoRenameOutcome{Renamed: true, From: "octo/api", To: observed.Slug()}, nil
}

func (s *renamePollStore) ListConfiguredNamesSystem(context.Context, string) ([]string, error) {
	if s.configuredErr != nil {
		return nil, s.configuredErr
	}
	return s.configured, nil
}
