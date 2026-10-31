package agenthost

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// errNoRegistryRow refuses a branch deliverable whose repository no registry row
// answers for: a branch is keyed on its repository's row id
// (domain.BranchDedupKey), so without a row there is nothing to key it on.
var errNoRegistryRow = errors.New("no repository registry row answers to the branch's repository")

// registryRepo returns the registry row for the repository owner/repo names,
// on the org's current GitHub host, or nil when none answers for it.
//
// A verb's owner/repo is whatever the agent's checkout or argv spells, and a
// checkout made before a repository rename (a parked run restored from its
// snapshot) spells the old name, which GitHub still follows to the repository.
// The registry stores one name per repository, and a rename moves it when the
// rename is applied.
//
// A name the registry holds under a provider id is its own answer. Any other
// name goes to GitHub, which answers the repository's id however it is
// addressed, and the id finds the row. That settles both directions: an old
// name lands on the row the rename moved, and a new name the registry has not
// caught up with lands on the row the rename will move. A name no identified
// row answers to falls back to the row that holds it, which carries no
// provider id, or to nil. So does a failed GitHub read: GitHub is asked only
// for what the registry cannot answer alone.
//
// The error is a failed registry read that left no row to answer with. A
// caller that cannot do without the row (a branch's key) returns it, and one
// that can keeps the name it was given.
func (c *LocalClient) registryRepo(ctx context.Context, owner, repo string) (*domain.Repository, error) {
	slug := owner + "/" + repo
	row, err := c.rt.GetRepo(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("read the registry row for %s: %w", slug, err)
	}
	if row != nil && row.ExternalID != "" {
		return row, nil
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("registry name: no GitHub client to resolve the repository with; keeping the row the name holds",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
		return row, nil
	}
	meta, err := client.GetRepoMeta(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("registry name: reading the repository from GitHub failed; keeping the row the name holds",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
		return row, nil
	}
	current, err := c.rt.RepoSlugByExternalID(ctx, meta.ExternalID())
	if err == nil && current != "" {
		var held *domain.Repository
		if held, err = c.rt.GetRepo(ctx, current); err == nil && held != nil {
			return held, nil
		}
	}
	if err != nil && row == nil {
		return nil, fmt.Errorf("read the registry row GitHub's id for %s names: %w", slug, err)
	}
	if err != nil {
		agenthostLog.Warn("registry name: reading the repository by id failed; keeping the row the name holds",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
	}
	return row, nil
}

// onRegistryRow records a GitHub or git deliverable built under the name a verb
// spelled against its repository's registry row (registryRepo). The target
// takes the registry's name, so the row reads like every other row of the
// repository. A branch is keyed on the row's id (domain.BranchDedupKey), so
// one whose repository has no row is refused with errNoRegistryRow, and one
// whose row could not be read with the read's error. A pull request or review
// is keyed on the pull request's node id already, and one whose repository's
// row is missing or unreadable keeps the name it was built under. A deliverable
// whose target does not lead with an owner/repo is returned as it is.
func (c *LocalClient) onRegistryRow(ctx context.Context, a domain.Deliverable) (domain.Deliverable, error) {
	if a.Provider != domain.ProviderGitHub && a.Provider != domain.ProviderGit {
		return a, nil
	}
	from, _, _ := strings.Cut(a.Target, "#")
	owner, repo, ok := strings.Cut(from, "/")
	var row *domain.Repository
	var err error
	if ok && owner != "" && repo != "" && !strings.Contains(repo, "/") {
		row, err = c.registryRepo(ctx, owner, repo)
	}
	if row == nil {
		if a.Kind != domain.DeliverableKindBranch {
			if err != nil {
				agenthostLog.Warn("registry name: keeping the name as spelled",
					"conversation", c.info.ConversationID, "repo", from, "error", err)
			}
			return a, nil
		}
		if err != nil {
			return a, err
		}
		return a, fmt.Errorf("%w: %s", errNoRegistryRow, from)
	}
	a.Target, _ = domain.RewriteRepoSlugPrefix(a.Target, from, row.Slug())
	if a.Kind == domain.DeliverableKindBranch {
		a.DedupKey = domain.BranchDedupKey(row.ID, a.ExternalID)
	}
	return a, nil
}
