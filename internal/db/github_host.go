package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// NormalizeGitHubHost puts a GitHub host into the ghbase.CanonicalBaseURL form
// so the (user_id, github_base_url) key in user_github_identities matches
// whatever spelling a caller passes: "https://github.com/",
// "https://GitHub.com" and "https://github.com" are one key. Reads and writes
// both normalize, so they agree by construction. An empty host stays empty;
// EffectiveGitHubHost is the variant that resolves it to the deployment
// default.
//
// Shared by the SQLite and Postgres usersStore impls so the rule has one
// home. NormalizeJiraHost is the same canonicalizer under the Jira tables'
// name.
func NormalizeGitHubHost(host string) string { return ghbase.CanonicalBaseURL(host) }

// EffectiveGitHubHost resolves an org's configured github_base_url to the host
// identities are actually keyed under: an empty setting means the deployment's
// default GitHub (ghbase.DefaultBaseURL — github.com unless the operator named
// another), and anything else is its NormalizeGitHubHost form (what the stores
// key on). Read-side callers building a reverse identity lookup must use this
// rather than the raw setting — an empty setting would otherwise look up
// host="" and miss rows captured under the default.
//
// The GoTrue GitHub OAuth login identity is the one host-keyed row that does
// NOT resolve through here: that provider is github.com whatever the
// deployment default is, so the login claim binds under ghbase.GitHubCom
// literally.
//
// The derivation itself is domain.GitHubHost, which is also the scope GitHub
// entities are keyed under; this name is the identity stores' spelling of it.
func EffectiveGitHubHost(orgBase string) string {
	return domain.GitHubHost(orgBase)
}

// OrgGitHubHostSystem reads orgID's settings and returns its current GitHub
// host (EffectiveGitHubHost of github_base_url) — the host every GitHub-shaped
// row the org reads, polls or writes now is scoped to: repositories, tracking,
// team mappings, entities, the reachable-repo cache. Claims-free, for the
// background and system callers that hold an org id and no settings row.
func OrgGitHubHostSystem(ctx context.Context, orgs OrgsStore, orgID string) (string, error) {
	if orgs == nil {
		return "", errors.New("read github host: no org store")
	}
	set, err := orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read github host: %w", err)
	}
	return EffectiveGitHubHost(set.GitHubBaseURL), nil
}
