package agenthost

import (
	"errors"
	"fmt"
	"strings"
	"time"

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

// bundleRepoToken resolves the GitHub credential a run's sealed bundle
// carries for (owner, repo): an exact repo-scoped installation token when
// repo is known, falling through to the single org-wide PAT (a PAT can't be
// narrowed, so it answers for every repo) if no repo-scoped entry matches.
//
// repo == "" (the caller has no specific repo in view) resolves ONLY when
// owner has exactly one RepoTokens entry — map iteration order is otherwise
// undefined, and a bundle's RepoTokens are scoped per-repo, so picking an
// arbitrary sibling repo's token would 403 every call it's used for. Mirrors
// the git proxy's TokenSource resolution (internal/delegate's
// bundleGitProxyConfigFor) so the two credential paths agree on which token
// answers for an ambiguous owner.
func bundleRepoToken(gh *credbundle.GitHubCreds, owner, repo string) (token string, expiresAt time.Time, ok bool) {
	if gh == nil {
		return "", time.Time{}, false
	}
	if repo != "" {
		for repoID, rt := range gh.RepoTokens {
			o, r, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
				return rt.Token, rt.ExpiresAt, true
			}
		}
	} else {
		var match credbundle.RepoToken
		matches := 0
		for repoID, rt := range gh.RepoTokens {
			o, _, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) {
				matches++
				match = rt
			}
		}
		if matches == 1 {
			return match.Token, match.ExpiresAt, true
		}
	}
	if gh.PAT != "" {
		return gh.PAT, time.Time{}, true
	}
	return "", time.Time{}, false
}

// bundleRepoClient builds a *ghclient.Client straight from a run's sealed
// GitHub credential — the TF_ROLE=executor mirror of scopedRepoClient's
// resolver path, with no secret-store read: the token was already minted
// host-side by the brain and sealed into this run's bundle. A repo the
// bundle carries no token for (and no PAT to fall back to) is a
// provisioning gap, not a missing-credential one, so it gets its own clear
// error rather than the org-wide "not configured" message — the AC this
// ticket sets: never surface a secret-store error on this path, because the
// secret store is never consulted.
func bundleRepoClient(gh *credbundle.GitHubCreds, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if gh == nil {
		return nil, ghclient.IdentityUnknown, errGitHubNotConfigured
	}
	token, _, ok := bundleRepoToken(gh, owner, repo)
	if !ok {
		return nil, ghclient.IdentityUnknown, fmt.Errorf("repo %s/%s not provisioned for this run", owner, repo)
	}
	base := ghclient.DefaultBaseURL
	if gh.BaseURL != "" {
		base = gh.BaseURL
	}
	identity := ghclient.IdentityPAT
	if gh.Mode == "app" {
		identity = ghclient.IdentityApp
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
