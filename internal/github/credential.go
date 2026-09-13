package github

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CredentialForRepo names the org credential a GitHub write on owner/repo is
// made under (a domain.Credential* value), for an audit row's credential
// column. It asks the same resolver the write's own client comes from, because
// that is the only thing that knows: an installation token and a PAT are
// indistinguishable strings once minted, so the tier has to come from the
// decision that picked one. The resolver shares its coverage probe and token
// cache with that resolution, so at a site that has already built a client this
// is a cache read rather than a second round trip.
//
// Anything unresolved reports the App — a nil resolver, a resolver that doesn't
// classify, a malformed target, a credential backend that just failed. The row
// is about a GitHub write that happened or is about to; the App is what this
// column has said since it existed, and asserting a PAT nobody observed would
// be worse than the status quo it replaces.
//
// Call it BEFORE opening the transaction the row is composed into: it can reach
// GitHub, and a credential probe inside a write tx would hold that tx open for
// a network round trip.
func CredentialForRepo(ctx context.Context, resolver Resolver, orgID, owner, repo string) string {
	ir, ok := resolver.(RepoIdentityResolver)
	if !ok {
		return domain.CredentialGitHubApp
	}
	_, identity, err := ir.ClientForRepoWithIdentity(ctx, orgID, owner, repo)
	if err != nil {
		ghResolverLog.Warn("classify github credential for audit row failed; recording as the app",
			"org", orgID, "owner", owner, "repo", repo, "error", err)
		return domain.CredentialGitHubApp
	}
	return identity.Credential()
}
