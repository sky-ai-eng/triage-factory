package agenthost

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// errGitHubNotConfigured / errJiraNotConfigured mirror the resolver-path
// messages (mapGithubResolveErr, jiraSystemClient's ErrNoJiraSystemCredential
// mapping) so an agent sees the identical guidance whether the daemon
// resolved credentials through a run's sealed bundle or the live secret
// store.
var (
	errGitHubNotConfigured = errors.New("GitHub not configured; run triagefactory and complete setup first")
	errJiraNotConfigured   = errors.New("no Jira credential configured; run triagefactory and complete setup first")
)

// bundleRepoClient builds a *ghclient.Client straight from a run's sealed
// GitHub credential — the TF_ROLE=executor mirror of scopedRepoClient's
// resolver path, with no secret-store read: the token was already minted
// host-side by the brain and sealed into this run's bundle. Token
// resolution is shared with the git proxy's TokenSource
// (credbundle.ResolveRepoToken — see internal/delegate/bundle_github.go) so
// the two credential paths never drift.
//
// A repo the bundle carries no token for (and no PAT to fall back to) is a
// provisioning gap, not a missing-credential one, so it gets its own clear
// error rather than the org-wide "not configured" message — the AC this
// ticket sets: never surface a secret-store error on this path, because the
// secret store is never consulted.
//
// Identity is derived from the RESOLVED source, not gh.Mode: the
// provisioner can fall back to the org PAT for one repo in an otherwise
// App-tier bundle (and flips Mode to "pat" when it does, even though other
// repos in the same bundle still hold real App-tier RepoTokens entries), so
// Mode alone can disagree with which credential this specific repo actually
// resolved to. Identity must describe the credential on the wire.
func bundleRepoClient(gh *credbundle.GitHubCreds, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if gh == nil {
		return nil, ghclient.IdentityUnknown, errGitHubNotConfigured
	}
	token, _, source := credbundle.ResolveRepoToken(gh, owner, repo)
	var identity ghclient.Identity
	switch source {
	case credbundle.RepoTokenApp:
		identity = ghclient.IdentityApp
	case credbundle.RepoTokenPAT:
		identity = ghclient.IdentityPAT
	default:
		return nil, ghclient.IdentityUnknown, fmt.Errorf("repo %s/%s not provisioned for this run", owner, repo)
	}
	base := ghclient.DefaultBaseURL
	if gh.BaseURL != "" {
		base = gh.BaseURL
	}
	return ghclient.NewClient(base, token), identity, nil
}

// bundleJiraClient builds a *jiraclient.Client straight from a run's sealed
// Jira credential — the shape jira.SystemCredential / ResolveSystemCredential
// already documents as designed for this exact reconstruction
// (jira.NewClient(CloudAPIToken(...)) / jira.NewClient(DataCenterPAT(...))),
// with no further secret-store read.
func bundleJiraClient(jc *credbundle.JiraCreds) (*jiraclient.Client, error) {
	if jc == nil {
		return nil, errJiraNotConfigured
	}
	if jc.AuthMethod == "cloud" {
		if jc.Email == "" || jc.APIToken == "" {
			return nil, errJiraNotConfigured
		}
		return jiraclient.NewClient(jiraclient.CloudAPIToken(jc.URL, jc.Email, jc.APIToken)), nil
	}
	if jc.PAT == "" {
		return nil, errJiraNotConfigured
	}
	return jiraclient.NewClient(jiraclient.DataCenterPAT(jc.URL, jc.PAT)), nil
}
