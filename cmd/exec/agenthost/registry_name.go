package agenthost

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// registryRepoSlug returns the "owner/repo" under which the repository registry
// holds the repository owner/repo names, on the org's current GitHub host.
//
// A verb's owner/repo is whatever the agent's checkout or argv spells, and a
// checkout made before a repository rename (a parked run restored from its
// snapshot) spells the old name, which GitHub still follows to the repository.
// TF's stored keys spell the registry's name, and a rename moves the ones it
// finds when it is applied. A key written under the old name after that is
// never moved: readers matching the current name miss it, and the same object
// recorded under both names is two rows.
//
// A name the registry holds under a provider id is its own answer. Any other
// name goes to GitHub, which answers the repository's id however it is
// addressed, and the id finds the row. That settles both directions: an old
// name lands on the row the rename moved, and a new name the registry has not
// caught up with lands on the row the rename will move, so its key moves with
// the others instead of colliding with them. A name no identified row answers
// to is kept as spelled: a rename keys on the provider id, so it never moves
// such a row and there is no other name for the key to agree with.
//
// Best-effort, like the recording it feeds: a failed read keeps the name as
// spelled.
func (c *LocalClient) registryRepoSlug(ctx context.Context, owner, repo string) string {
	slug := owner + "/" + repo
	row, err := c.rt.GetRepo(ctx, slug)
	if err != nil {
		agenthostLog.Warn("registry name: reading the repository by name failed; asking GitHub",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
	}
	if row != nil && row.ExternalID != "" {
		return row.Slug()
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("registry name: no GitHub client to resolve the repository with; keeping the name as spelled",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
		return slug
	}
	meta, err := client.GetRepoMeta(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("registry name: reading the repository from GitHub failed; keeping the name as spelled",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
		return slug
	}
	current, err := c.rt.RepoSlugByExternalID(ctx, meta.ExternalID())
	if err != nil {
		agenthostLog.Warn("registry name: reading the repository by id failed; keeping the name as spelled",
			"conversation", c.info.ConversationID, "repo", slug, "error", err)
		return slug
	}
	if current == "" {
		return slug
	}
	return current
}

// inRegistryName moves a GitHub or git artifact built under the name a verb
// spelled onto the registry's name for the repository (registryRepoSlug). Its
// target and dedup key both lead with the repository's name, and a rename
// rewrites the two together, so they move together here too. An artifact
// whose target does not lead with an owner/repo is returned as it is.
func (c *LocalClient) inRegistryName(ctx context.Context, a domain.Artifact) domain.Artifact {
	if a.Provider != domain.ArtifactProviderGitHub && a.Provider != domain.ArtifactProviderGit {
		return a
	}
	from, _, _ := strings.Cut(a.Target, "#")
	owner, repo, ok := strings.Cut(from, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return a
	}
	to := c.registryRepoSlug(ctx, owner, repo)
	if to == from {
		return a
	}
	a.Target, _ = domain.RewriteRepoSlugPrefix(a.Target, from, to)
	a.DedupKey, _ = domain.RewriteArtifactDedupKey(a.DedupKey, from, to)
	return a
}
