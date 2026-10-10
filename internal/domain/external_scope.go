package domain

// ExternalObjectScope is the scope an artifact or an audit-ledger row about
// an object of provider is recorded under, for an org with settings s: the
// value an entity of the same provider is scoped under (EntityScope), so a
// record about an entity and the entity itself name one namespace.
//
//   - github, jira, linear, slack: EntityScope of the same source.
//   - git: the GitHub host. A branch is pushed to a GitHub repository.
//   - network: NetworkScope.
//
// "" for a provider with no scope in the org (its source is not configured)
// and for any provider not listed. A store refuses a write with no scope, so
// a caller that gets "" has nothing it can record.
//
// This is the org's scope now. A write about an existing entity takes that
// entity's scope instead, and a write that updates a stored row keeps the
// row's own.
func ExternalObjectScope(provider string, s OrgSettings) string {
	switch provider {
	case ArtifactProviderGit:
		return EntityScope(ArtifactProviderGitHub, s)
	case ArtifactProviderNetwork:
		return NetworkScope
	}
	return EntityScope(provider, s)
}

// NetworkScope is the scope of an action whose target is a bare network
// destination (ArtifactProviderNetwork): a host and port, which DNS names
// without any provider namespace around it.
const NetworkScope = "internet"
