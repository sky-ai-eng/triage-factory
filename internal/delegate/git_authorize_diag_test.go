package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitAuthorizeDecision_ErrorNamesTheFailingRead pins the diagnostic half of
// the push gate. Every read here fails the decision closed, and the proxy turns
// that into a 403 whose body deliberately tells the agent nothing — so the
// error text is the whole explanation an operator ever gets, at the far end of
// a relay hop, in a different process from the store that produced it. A bare
// driver error ("column X does not exist") says what broke without saying which
// question was being asked, and these reads ask quite different ones.
func TestGitAuthorizeDecision_ErrorNamesTheFailingRead(t *testing.T) {
	boom := errors.New("boom")
	info := agenthost.ConversationInfo{
		OrgID:          runmode.LocalDefaultOrgID,
		TeamID:         runmode.LocalDefaultTeamID,
		ConversationID: "run-1",
	}

	for _, tc := range []struct {
		name   string
		stores db.Stores
		want   string
	}{
		{
			name: "github host read",
			stores: db.Stores{
				Orgs:                  stubOrgsStore{err: boom},
				TeamGitHubRepos:       failingTracksStore{tracks: true},
				ConversationWorktrees: stubWorktreesStore{},
				Repos:                 stubReposStore{},
			},
			want: "github host read",
		},
		{
			name: "tracked-set read",
			stores: db.Stores{
				Orgs:                  stubOrgsStore{},
				TeamGitHubRepos:       failingTracksStore{err: boom},
				ConversationWorktrees: stubWorktreesStore{},
				Repos:                 stubReposStore{},
			},
			want: "tracked-set read",
		},
		{
			name: "worktree ledger read",
			stores: db.Stores{
				Orgs:                  stubOrgsStore{},
				TeamGitHubRepos:       failingTracksStore{tracks: true},
				ConversationWorktrees: stubWorktreesStore{err: boom},
				Repos:                 stubReposStore{},
			},
			want: "worktree ledger read",
		},
		{
			name: "repository read",
			stores: db.Stores{
				Orgs:                  stubOrgsStore{},
				TeamGitHubRepos:       failingTracksStore{tracks: true},
				ConversationWorktrees: stubWorktreesStore{},
				Repos:                 stubReposStore{err: boom},
			},
			want: "repository read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := gitAuthorizeDecision(context.Background(), tc.stores, info, "acme", "api")
			if err == nil {
				t.Fatal("decision succeeded; want the read's error surfaced")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q; want it to name %q", err, tc.want)
			}
			if !errors.Is(err, boom) {
				t.Errorf("error = %q; want the cause still unwrappable", err)
			}
		})
	}
}

// failingTracksStore answers TracksRepoSystem and nothing else — every other
// method panics on the embedded nil, which is what keeps the test honest about
// which reads the gate actually makes.
type failingTracksStore struct {
	db.TeamGitHubReposStore
	tracks bool
	err    error
}

func (s failingTracksStore) TracksRepoSystem(context.Context, string, string, string, string) (bool, error) {
	return s.tracks, s.err
}

// stubOrgsStore answers the org's settings read the gate makes for its GitHub
// host: the default host, or err.
type stubOrgsStore struct {
	db.OrgsStore
	err error
}

func (s stubOrgsStore) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return domain.OrgSettings{}, s.err
}

type stubWorktreesStore struct {
	db.ConversationWorktreeStore
	err error
}

func (s stubWorktreesStore) ListSystem(context.Context, string, string) ([]domain.ConversationWorktree, error) {
	return nil, s.err
}

// stubReposStore answers the gate's read of the repository on the org's host
// with err; any other read panics on the embedded nil.
type stubReposStore struct {
	db.RepositoryStore
	err error
}

func (s stubReposStore) GetByRefSystem(context.Context, string, domain.RepoRef) (*domain.Repository, error) {
	return nil, s.err
}
