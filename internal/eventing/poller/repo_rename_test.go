package poller

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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
			{Source: "github", Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "1"},
			{Source: "github", Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "web", ExternalID: "2"},
		},
	}
	m := &Manager{repos: repos}

	n, _ := m.applyRepoRenames(context.Background(), "org-1", dbtest.TestGitHubHost, []ghclient.UserRepo{
		{ID: 1, FullName: "octo/platform-api", Owner: ghclient.RepoOwner{ID: 583231}}, // renamed — TF still tracks octo/api
		{ID: 2, FullName: "octo/web"},       // unchanged
		{ID: 3, FullName: "octo/untracked"}, // granted, no row
		{FullName: "octo/idless"},           // no id in the payload
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
	if repos.renamed[0].OwnerExternalID != "583231" {
		t.Errorf("owner id = %q, want 583231 — the rename records the owner the grant names", repos.renamed[0].OwnerExternalID)
	}
	if repos.renamed[0].Source != domain.RepoSourceGitHub {
		t.Errorf("source = %q, want %q", repos.renamed[0].Source, domain.RepoSourceGitHub)
	}
	if repos.renamed[0].Host != dbtest.TestGitHubHost {
		t.Errorf("host = %q, want %q — the host the grant was read from", repos.renamed[0].Host, dbtest.TestGitHubHost)
	}
}

// A grant with nothing renamed in it is the steady state, which is every cycle
// after the first. It must not open a transaction.
func TestApplyRepoRenames_SteadyStateWritesNothing(t *testing.T) {
	repos := &renamePollStore{stored: []domain.RepoRef{{Source: "github", Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "1"}}}
	m := &Manager{repos: repos}

	if n, _ := m.applyRepoRenames(context.Background(), "org-1", dbtest.TestGitHubHost,
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
	if n, _ := m.applyRepoRenames(context.Background(), "org-1", dbtest.TestGitHubHost,
		[]ghclient.UserRepo{{ID: 1, FullName: "octo/api"}}); n != 0 {
		t.Errorf("applied = %d, want 0", n)
	}
}

// A tracked name the grant lists under another id is contested while GitHub
// cannot be asked about the stored row's own id. The poller still counts it
// as granted but does not poll it, because the pull requests under that name
// are another repository's.
func TestApplyRepoRenames_AnUnresolvedHolderIsContested(t *testing.T) {
	repos := &renamePollStore{stored: []domain.RepoRef{
		{Source: "github", Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "api", ExternalID: "1"},
		{Source: "github", Host: dbtest.TestGitHubHost, Owner: "octo", Repo: "web", ExternalID: "2"},
	}}
	m := &Manager{repos: repos, resolver: &perAccountFailResolver{failAccounts: map[string]bool{"octo": true}}}

	grant := []ghclient.UserRepo{{ID: 9, FullName: "Octo/API"}, {ID: 2, FullName: "octo/web"}}
	n, contested := m.applyRepoRenames(context.Background(), "org-1", dbtest.TestGitHubHost, grant)
	if n != 0 || len(contested) != 1 || contested[0] != "octo/api" {
		t.Fatalf("applied = %d, contested = %v; want 0 and [octo/api]", n, contested)
	}

	covered := map[string]bool{}
	scoped := withoutSlugs(intersectConfigured([]string{"octo/api", "octo/web"}, grant, covered), contested)
	if len(scoped) != 1 || scoped[0] != "octo/web" {
		t.Errorf("polled = %v, want only octo/web", scoped)
	}
	if !covered["octo/api"] {
		t.Error("octo/api is not covered; the grant lists the name, so it must not be reported as outside every installation")
	}
}

// After a rename the tracked set the cycle read at the top is stale by exactly
// one name, so the intersection below would skip the repository until the next
// cycle. Re-reading closes that gap; a failed re-read keeps the list it had.
func TestReloadConfiguredRepos(t *testing.T) {
	repos := &renamePollStore{configured: []string{"octo/platform-api", "octo/web"}}
	m := &Manager{repos: repos}

	got := m.reloadConfiguredRepos(context.Background(), "org-1", dbtest.TestGitHubHost, []string{"octo/api", "octo/web"})
	if len(got) != 2 || got[0] != "octo/platform-api" {
		t.Errorf("reloaded = %v, want the refreshed tracked set", got)
	}

	repos.configuredErr = context.DeadlineExceeded
	current := []string{"octo/api", "octo/web"}
	if got := m.reloadConfiguredRepos(context.Background(), "org-1", dbtest.TestGitHubHost, current); len(got) != 2 || got[0] != "octo/api" {
		t.Errorf("reloaded = %v on a read failure, want the caller's list unchanged", got)
	}
}

// renamePollStore embeds the interface (nil) so any method this path does not
// use panics loudly rather than silently returning a zero value.
type renamePollStore struct {
	db.RepositoryStore
	stored        []domain.RepoRef
	renamed       []domain.RepoRef
	configured    []string
	configuredErr error
}

func (s *renamePollStore) ListIdentitiesSystem(context.Context, string, string) ([]domain.RepoIdentity, error) {
	out := make([]domain.RepoIdentity, len(s.stored))
	for i, ref := range s.stored {
		out[i] = domain.RepoIdentity{RepoRef: ref}
	}
	return out, nil
}

func (s *renamePollStore) RenameSystem(_ context.Context, _ string, observed domain.RepoRef) (domain.RepoRenameOutcome, error) {
	s.renamed = append(s.renamed, observed)
	return domain.RepoRenameOutcome{Renamed: true, From: "octo/api", To: observed.Slug()}, nil
}

func (s *renamePollStore) ListTrackedNamesSystem(context.Context, string, string) ([]string, error) {
	if s.configuredErr != nil {
		return nil, s.configuredErr
	}
	return s.configured, nil
}
