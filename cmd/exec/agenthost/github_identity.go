package agenthost

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
)

// errNoRegistryRow refuses a branch deliverable whose repository no registry row
// answers for: a branch is keyed on its repository's row id
// (domain.BranchDedupKey), so without a row there is nothing to key it on.
var errNoRegistryRow = errors.New("no registry row the run may record against answers to the branch's repository")

// identityReadTimeout bounds the read an exec verb makes for what GitHub calls
// the object it just acted on. The agent waits on the verb, and the act has
// already landed, so past this it is recorded unlinked rather than held.
const identityReadTimeout = 10 * time.Second

// identities is the reader this client's identity reads go through: the
// Server's, shared by every request of the run, or one of its own.
func (c *LocalClient) identities() *ghclient.IdentityReader {
	if c.ghIdentities == nil {
		c.ghIdentities = ghclient.NewIdentityReader()
	}
	return c.ghIdentities
}

// githubObject is what GitHub calls owner/repo#number — its node id, whether it
// is a pull request or an issue, and its repository's id — read through
// client, the client the act it is recorded for went through, so the read is
// made with the credential the act was. The recording funnel resolves the act's
// entity and repository from it alone. A failed read is logged and answered
// with the zero identity, which records the act unlinked.
func (c *LocalClient) githubObject(ctx context.Context, client *ghclient.Client, owner, repo string, number int) domain.GitHubIdentity {
	if client == nil {
		return domain.GitHubIdentity{}
	}
	ctx, cancel := context.WithTimeout(ctx, identityReadTimeout)
	defer cancel()
	id, err := c.identities().Object(ctx, client, owner, repo, number)
	if err != nil {
		agenthostLog.Warn("reading what GitHub calls the object an act names failed; recording it unlinked",
			"conversation", c.info.ConversationID, "repo", owner+"/"+repo, "number", number, "error", err)
		return domain.GitHubIdentity{}
	}
	return id
}

// githubRepository is githubObject for an act on owner/repo that names no
// pull request or issue: the repository's id and current name.
func (c *LocalClient) githubRepository(ctx context.Context, client *ghclient.Client, owner, repo string) domain.GitHubIdentity {
	if client == nil {
		return domain.GitHubIdentity{}
	}
	ctx, cancel := context.WithTimeout(ctx, identityReadTimeout)
	defer cancel()
	id, err := c.identities().Repository(ctx, client, owner, repo)
	if err != nil {
		agenthostLog.Warn("reading what GitHub calls the repository an act is in failed; recording it unlinked",
			"conversation", c.info.ConversationID, "repo", owner+"/"+repo, "error", err)
		return domain.GitHubIdentity{}
	}
	return id
}

// githubPullRequest is the identity of the pull request whose node id the act
// already holds (its create's response, its deliverable's key): the node id as
// given, and the repository read beside it, which a create of its entity
// needs.
func (c *LocalClient) githubPullRequest(ctx context.Context, client *ghclient.Client, owner, repo, nodeID string) domain.GitHubIdentity {
	id := c.githubRepository(ctx, client, owner, repo)
	if nodeID != "" {
		id.NodeID, id.Kind = nodeID, domain.EntityKindPullRequest
	}
	return id
}

// targetIdentity reads the identity of whatever a GitHub target names, an
// owner/repo#N or an owner/repo, through the run's client for its repository.
// A target that names neither, or a repository the run cannot reach, answers
// the zero identity.
func (c *LocalClient) targetIdentity(ctx context.Context, target string) domain.GitHubIdentity {
	owner, repo, ok := targetRepository(target)
	if !ok {
		return domain.GitHubIdentity{}
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		agenthostLog.Warn("no GitHub client to read the identity of a recorded object with; recording it unlinked",
			"conversation", c.info.ConversationID, "repo", owner+"/"+repo, "error", err)
		return domain.GitHubIdentity{}
	}
	if _, _, number, ok := domain.ParsePRTarget(target); ok {
		return c.githubObject(ctx, client, owner, repo, number)
	}
	return c.githubRepository(ctx, client, owner, repo)
}

// IdentifyPushedRepository is push with GitHub's id for its repository read
// through this client's credential: the local-mode git proxy's push observer,
// which records the push in process. The sidecar reads the same id for a multi
// push itself, beside the credential it holds. A failed read leaves push as it
// is, which records the push unlinked.
func (c *LocalClient) IdentifyPushedRepository(ctx context.Context, push gitproxy.PushedRef) gitproxy.PushedRef {
	id := c.targetIdentity(ctx, push.Repo)
	push.RepositoryID, push.RepositoryName = id.RepositoryID, id.Repository
	return push
}
