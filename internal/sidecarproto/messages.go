package sidecarproto

// This file defines the typed payloads carried in a Frame.Body for each
// Kind. They are plain serializable structs — no credential material ever
// travels here in cleartext: the bundle crosses as opaque sealed bytes
// (only the sidecar's private key opens it) and everything returned upward
// is a URL or a per-run placeholder token, never a real key.

// HelloBody is KindHello's payload: the sidecar's per-run X25519 public key,
// base64 (std) encoded, minted fresh at sidecar startup. The orchestrator
// publishes it so the brain seals this run's bundle to it.
type HelloBody struct {
	PubKey string `json:"pubkey"`
}

// SealedBundleBody is KindSealedBundle's payload: one run's opaque sealed
// credential bundle (credseal output) plus the claiming boot epoch it was
// sealed under. The orchestrator relays the ciphertext verbatim; it cannot
// read it. BootEpoch lets the sidecar ignore a bundle from a stale claim.
type SealedBundleBody struct {
	Sealed    []byte `json:"sealed"`
	BootEpoch int64  `json:"boot_epoch"`
}

// StartProxiesBody is KindStartProxies' request payload: the host-side veth
// IP the sidecar binds its proxies on, and which optional proxies this run
// needs. The LLM + egress proxies are always started; git/github-REST/
// jira-REST are started only when the run touches those surfaces (a
// Jira-only run needs no git proxy; a GitHub-only run needs no Jira proxy).
type StartProxiesBody struct {
	// HostVethIP is the 10.42.<idx>.1 address the sandbox's netns reaches via
	// its default route — the proxies bind here, never loopback, so the
	// jailed agent (which can't reach 127.0.0.1) can talk to them.
	HostVethIP string `json:"host_veth_ip"`

	// GitEnabled requests the git-over-HTTPS credential proxy. When false the
	// run pre-clones nothing and pushes nowhere (a prompt-only or Jira-only
	// run) and no git proxy is bound.
	GitEnabled bool `json:"git_enabled"`

	// GitUpstream is the real git host base (GHES override or github.com) the
	// git proxy forwards to; empty defaults to github.com sidecar-side.
	GitUpstream string `json:"git_upstream,omitempty"`

	// IdentityConfigPairs are the org commit-identity git config (key,value)
	// pairs (user.name/user.email) folded into the sandbox's GIT_CONFIG_*
	// block alongside the proxy routing. Non-secret. A [2]string per pair.
	IdentityConfigPairs [][2]string `json:"identity_config_pairs,omitempty"`

	// GitHubAPIEnabled requests the GitHub REST credential proxy — the one
	// the orchestrator's own GetPR + agenthost gh verbs route through so they
	// hold only a placeholder, never the real token. Upstream is the REST API
	// base (api.github.com or the GHES /api/v3 base); empty defaults host-side.
	GitHubAPIEnabled  bool   `json:"github_api_enabled,omitempty"`
	GitHubAPIUpstream string `json:"github_api_upstream,omitempty"`

	// JiraAPIEnabled requests the Jira REST credential proxy for the
	// orchestrator's agenthost jira verbs. Upstream is the org's Jira base;
	// the sidecar resolves the injected auth (Cloud Basic vs DC Bearer) from
	// the bundle's Jira credential, so no auth material crosses here.
	JiraAPIEnabled  bool   `json:"jira_api_enabled,omitempty"`
	JiraAPIUpstream string `json:"jira_api_upstream,omitempty"`
}

// StartProxiesResult is KindStartProxies' response payload: the non-secret
// sandbox env entries the orchestrator stamps into the OCI spec so the agent
// reaches the sidecar's proxies (proxy URLs + per-run placeholder tokens),
// exactly the slice startProxiesForSandbox used to return in-process, plus
// the coordinates the orchestrator needs to route its OWN GetPR/agenthost
// REST calls through the sidecar's API proxies (empty when not requested).
type StartProxiesResult struct {
	Env []string `json:"env"`

	// GitProxyURL / GitProxyToken are the sidecar's git-over-HTTPS proxy
	// address and the per-run placeholder the orchestrator presents to it, so
	// the orchestrator's OWN pre-sandbox clone (GetPR's worktree materialize)
	// routes through the same proxy the jailed agent uses — holding only the
	// placeholder while the real GitHub token stays in the sidecar. Empty when
	// GitEnabled was false (a prompt-only / Jira-only run pre-clones nothing).
	GitProxyURL   string `json:"git_proxy_url,omitempty"`
	GitProxyToken string `json:"git_proxy_token,omitempty"`

	// GitHubAPIURL / GitHubAPIToken are the sidecar's GitHub-REST proxy
	// address and the per-run placeholder the orchestrator presents to it —
	// the orchestrator builds its ghclient against these, so the real token
	// stays in the sidecar. Empty when GitHubAPIEnabled was false.
	GitHubAPIURL   string `json:"github_api_url,omitempty"`
	GitHubAPIToken string `json:"github_api_token,omitempty"`

	// JiraAPIURL / JiraAPIToken are the same for the Jira-REST proxy.
	JiraAPIURL   string `json:"jira_api_url,omitempty"`
	JiraAPIToken string `json:"jira_api_token,omitempty"`
}

// AuthorizeRepoBody is KindAuthorizeRepo's request payload (sidecar →
// orchestrator): the repo a transiting git op targets. The orchestrator
// makes the DB-backed decision and returns AuthorizeRepoResult.
type AuthorizeRepoBody struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// AuthorizeRepoResult mirrors gitproxy.Decision across the wire: whether the
// repo is pushable and, if so, the exact refs allowed. An empty AllowedRefs
// with Allowed=true means fetch-only (no push ref authorized).
type AuthorizeRepoResult struct {
	Allowed     bool     `json:"allowed"`
	AllowedRefs []string `json:"allowed_refs,omitempty"`
}

// RecordDenialBody is KindRecordDenial's payload (sidecar → orchestrator,
// one-way): a denied git op for the orchestrator to audit through the same
// host-side recording path the push backstop uses.
type RecordDenialBody struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

// RecordPushBody is KindRecordPush's payload (sidecar → orchestrator,
// one-way): one branch ref a receive-pack transited, plus the upstream's
// final HTTP status, mirroring gitproxy.PushedRef across the wire. The
// orchestrator reshapes it into the same branch artifact / push-failed row
// the in-process backstop writes. Repo is the "owner/repo" the proxy parsed
// from the request path; Created marks a newly-created ref; Status is the
// receive-pack response code (a 2xx means the push landed).
type RecordPushBody struct {
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	NewSHA  string `json:"new_sha"`
	Created bool   `json:"created"`
	Status  int    `json:"status"`
}
