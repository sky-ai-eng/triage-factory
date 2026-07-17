package agenthost

import (
	"errors"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// errGitHubNotConfigured / errJiraNotConfigured mirror the resolver-path
// messages (mapGithubResolveErr, jiraSystemClient's ErrNoJiraSystemCredential
// mapping) so an agent sees the identical guidance whether the daemon resolved
// credentials through the run's sidecar proxies or the live secret store.
var (
	errGitHubNotConfigured = errors.New("GitHub not configured; run triagefactory and complete setup first")
	errJiraNotConfigured   = errors.New("no Jira credential configured; run triagefactory and complete setup first")
)

// ProxyCredentials points the executor's agenthost at the run's per-run
// credential sidecar REST proxies instead of resolving real tokens from a
// secret store or a sealed bundle. Each client presents only the per-run
// placeholder; the sidecar authenticates it and injects the real GitHub / Jira
// credential on the upstream hop, so this host-side daemon — which parses
// hostile agent traffic — holds no durable credential (Property B, TFAC-631).
//
// Non-nil only on TF_ROLE=executor; the delegate builds it from the sidecar
// bring-up's returned coordinates. nil on all/local, where the daemon resolves
// through the live secret store (githubResolver / the Jira resolver).
type ProxyCredentials struct {
	// GitHubAPIURL is the sidecar's GitHub-REST proxy base (a bare host URL —
	// the proxy prepends the real REST base path, api.github.com or a GHES
	// /api/v3). GitHubAPIToken is the per-run placeholder the client presents.
	GitHubAPIURL   string
	GitHubAPIToken string

	// JiraAPIURL / JiraAPIToken are the same for the Jira-REST proxy.
	JiraAPIURL   string
	JiraAPIToken string

	// GitProxyURL / GitProxyToken are the run's git-over-HTTPS proxy address and
	// the per-run placeholder a host-side clone/fetch presents. `workspace add`'s
	// materialization routes through this proxy (worktree.CloneAuthViaGitProxy)
	// so the real installation token — held only in the sidecar's bundle-backed
	// proxy — is injected on the upstream hop and never enters this daemon. Empty
	// when the run started no git proxy (a prompt-only / Jira-only run); a clone
	// then injects nothing and surfaces its own auth error on a private repo.
	GitProxyURL   string
	GitProxyToken string
}

// proxyRepoClient builds a GitHub client pointed at the sidecar's GitHub-REST
// proxy — the TF_ROLE=executor mirror of scopedRepoClient's resolver path, with
// no secret-store read. The client's REST base IS the proxy URL (verbatim, via
// NewProxyClient) and it carries only the placeholder token; the sidecar
// injects the real repo-scoped token upstream, and its per-repo TokenSource
// owns the App/PAT tier selection.
//
// Identity is reported as App: the executor provisions installation tokens for
// tracked repos, and — since every production exec-gh verb funnels through
// githubClientForRepo, which discards the identity — the value is descriptive
// only, never behavioral. A repo the sidecar can't resolve surfaces as the
// proxy's own 502 on first use, not here.
func proxyRepoClient(pc *ProxyCredentials, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if pc == nil || pc.GitHubAPIURL == "" {
		return nil, ghclient.IdentityUnknown, errGitHubNotConfigured
	}
	return ghclient.NewProxyClient(pc.GitHubAPIURL, pc.GitHubAPIToken), ghclient.IdentityApp, nil
}

// proxyJiraClient builds a Jira client pointed at the sidecar's Jira-REST
// proxy. The client presents the placeholder as a Bearer regardless of the real
// deployment flavor — the proxy authenticates the placeholder and injects the
// real Cloud-Basic / DC-Bearer auth on the upstream hop, so the client never
// needs to know which. BaseURL is the proxy URL; the proxy prepends the real
// Jira base path (a Cloud gateway /ex/jira/{id} or a DC host root). REST v2 is
// used for both deployments deliberately — see jira.ProxyPlaceholder for why
// matching a Cloud org's v3 is gated on ADF-aware comment bodies.
func proxyJiraClient(pc *ProxyCredentials) (*jiraclient.Client, error) {
	if pc == nil || pc.JiraAPIURL == "" {
		return nil, errJiraNotConfigured
	}
	return jiraclient.NewClient(jiraclient.ProxyPlaceholder(pc.JiraAPIURL, pc.JiraAPIToken)), nil
}
