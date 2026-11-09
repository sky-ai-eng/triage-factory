package tracker

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// GitHubReach is the set of repositories a RefreshGitHub pass's credential can
// read: an App installation's grant, or every repository for a personal
// token. A pass refreshes only the pull requests in repositories its reach
// holds. GitHub answers a node id the credential cannot read with null, and
// prices the batch from the query's text rather than its results, so a node id
// sent under the wrong credential costs that credential's budget a full
// request's points for nothing.
//
// The zero value holds no repository, so a pass handed none refreshes only the
// pull requests in the repositories it was handed.
type GitHubReach struct {
	all bool
	// ids is GitHub's repository ids, the ones a registry row records as its
	// external id.
	ids map[string]bool
	// names is the grant's owner/repo names, folded, for a row that records no
	// id yet.
	names map[string]bool
}

// ReachAll is a personal token's reach: every repository on the host.
func ReachAll() GitHubReach {
	return GitHubReach{all: true}
}

// ReachOfGrant is an App installation's reach: the repositories its grant
// lists.
func ReachOfGrant(grant []ghclient.UserRepo) GitHubReach {
	r := GitHubReach{ids: make(map[string]bool, len(grant)), names: make(map[string]bool, len(grant))}
	for _, g := range grant {
		if id := g.ExternalID(); id != "" {
			r.ids[id] = true
		}
		if g.FullName != "" {
			r.names[strings.ToLower(g.FullName)] = true
		}
	}
	return r
}

// HoldsAny reports whether the reach holds any of repos, registry rows, by the
// rule a pass's Phase 2 applies to each pull request's repository (holds). The
// poller asks it before it runs a RefreshHeldGitHub pass under a credential,
// so a credential that reads none of the repositories TF holds pull requests
// in runs none.
func (r GitHubReach) HoldsAny(repos []domain.Repository) bool {
	for i := range repos {
		if r.holds(&repos[i]) {
			return true
		}
	}
	return false
}

// passShare is how one RefreshGitHub pass treats one of the org's active pull
// requests.
type passShare int

const (
	// shareOwn is a pull request in a tracked repository the pass's credential
	// reads: refreshed under the gate (shouldSkipRefresh).
	shareOwn passShare = iota
	// shareUntracked is a pull request in a repository no team tracks that the
	// pass's credential reads. Nothing lists the repository, so the gate has no
	// fresh state to compare with, and an open one is refreshed once its last
	// refresh is older than staleRefreshFloor. That keeps its state current
	// enough for the tasks left on it to close when it merges, and for the
	// dashboard, until it closes.
	shareUntracked
	// shareNone is a pull request the pass's credential does not read: another
	// installation's, or in a repository no installation grants. The pass
	// sends GitHub nothing about it.
	shareNone
)

// githubPass decides each active pull request's share of one RefreshGitHub
// pass.
type githubPass struct {
	t     *Tracker
	reach GitHubReach
	// handed is the registry row ids of the repositories the pass was handed
	// (githubDiscovery.passRepos).
	handed map[string]bool
	// tracked is every tracked repository on the host by registry row id
	// (githubDiscovery.tracked).
	tracked map[string]domain.Repository
	// untracked holds the rows read this pass of repositories outside the
	// tracked set, nil for one that could not be read, so each is read once.
	untracked map[string]*domain.Repository
}

// share is e's share of the pass. listed is whether the pass's own discovery
// returned it, which only a repository the pass reads can do.
//
// A tracked repository the pass's credential reads but was not handed is the
// pass's own all the same: one GitHub renamed before TF applied the rename,
// which the poller's intersection, matching by name, did not hand to any pass
// this cycle.
func (p *githubPass) share(ctx context.Context, e domain.Entity, listed bool) passShare {
	if listed || p.handed[e.RepositoryID] {
		return shareOwn
	}
	if row, ok := p.tracked[e.RepositoryID]; ok {
		if p.reach.holds(&row) {
			return shareOwn
		}
		return shareNone
	}
	if !p.reach.all {
		row, seen := p.untracked[e.RepositoryID]
		if !seen {
			row = p.t.entityRepository(ctx, e)
			p.untracked[e.RepositoryID] = row
		}
		if !p.reach.holds(row) {
			return shareNone
		}
	}
	return shareUntracked
}

// holds reports whether repo, a registry row, is in the reach. A row that
// records GitHub's id is matched by the id alone: a grant entry under the
// row's name with another id is another repository, which GitHub gave the name
// to (reporename), and a renamed repository keeps its id. A row with no id yet
// is matched by its name. A nil row is held only by a reach that holds
// everything.
func (r GitHubReach) holds(repo *domain.Repository) bool {
	if r.all {
		return true
	}
	if repo == nil {
		return false
	}
	if repo.ExternalID != "" {
		return r.ids[repo.ExternalID]
	}
	return r.names[strings.ToLower(repo.Slug())]
}
