package dbtest

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TrackRepos makes teamID's tracked set on host exactly the named
// repositories. Each is brought into the registry by name through
// RepositoryStore.FindOrCreateByRefSystem, and the set is then written by the
// registry ids that returns, since TeamGitHubReposStore.ReplaceForTeam takes
// nothing else. Only Owner and Repo of each entry are read.
//
// It is the fixture for tests whose subject is something downstream of a
// tracked set (a gate, a poll, a push) and that state the set by name. Pass the
// two stores of one backend, or of one transaction to compose inside it.
func TrackRepos(ctx context.Context, repos db.RepositoryStore, tracking db.TeamGitHubReposStore, orgID, teamID, host string, names []domain.TeamGitHubRepo) error {
	ids := make([]string, 0, len(names))
	for _, n := range names {
		row, err := repos.FindOrCreateByRefSystem(ctx, orgID, domain.RepoRef{Host: host, Owner: n.Owner, Repo: n.Repo})
		if err != nil {
			return fmt.Errorf("register %s: %w", n.Slug(), err)
		}
		ids = append(ids, row.ID)
	}
	return tracking.ReplaceForTeam(ctx, orgID, teamID, host, ids)
}
