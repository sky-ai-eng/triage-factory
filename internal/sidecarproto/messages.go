package sidecarproto

import "encoding/json"

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
	// run) and no git proxy is bound. Which host it forwards to is not asked
	// here: every GitHub lane derives that from the sealed bundle's own
	// BaseURL, so the token and the host it belongs to arrive together.
	GitEnabled bool `json:"git_enabled"`

	// IdentityConfigPairs are the org commit-identity git config (key,value)
	// pairs (user.name/user.email) folded into the sandbox's GIT_CONFIG_*
	// block alongside the proxy routing. Non-secret. A [2]string per pair.
	IdentityConfigPairs [][2]string `json:"identity_config_pairs,omitempty"`

	// GitHubAPIEnabled requests the GitHub REST credential proxy — the one
	// the orchestrator's own GetPR + agenthost gh verbs route through so they
	// hold only a placeholder, never the real token. The REST mount is derived
	// sidecar-side from the bundle's BaseURL.
	GitHubAPIEnabled bool `json:"github_api_enabled,omitempty"`

	// JiraAPIEnabled requests the Jira REST credential proxy for the
	// orchestrator's agenthost jira verbs. Upstream is the org's Jira base;
	// the sidecar resolves the injected auth (Cloud Basic vs DC Bearer) from
	// the bundle's Jira credential, so no auth material crosses here.
	JiraAPIEnabled  bool   `json:"jira_api_enabled,omitempty"`
	JiraAPIUpstream string `json:"jira_api_upstream,omitempty"`

	// GHChannelEnabled requests the real-gh credential-injector proxy — the
	// TLS listener the sandboxed `gh` binary reaches via GH_HOST, holding only a
	// per-run placeholder while the sidecar injects the team-set-scoped token
	// upstream. It shares the REST mount the GitHub proxy derives from the
	// bundle. HostVethIP is reused as the injector's TLS SAN so gh's
	// forced-https verification passes.
	GHChannelEnabled bool `json:"gh_channel_enabled,omitempty"`

	// AgentHost, when non-nil, asks the sidecar to ALSO host the agenthost
	// socket server for this run — moving the hostile-input exec verb parser
	// off the orchestrator (where it sits beside the all-orgs db.Stores) and
	// into this capless per-run jail. It carries the run's non-secret identity
	// (what the socket server reports to the agent); every DB effect the verbs
	// perform relays back to the orchestrator, which binds identity from its
	// OWN ConversationInfo, so nothing here is security-load-bearing.
	AgentHost *AgentHostInfo `json:"agent_host,omitempty"`
}

// AgentHostInfo is the run's non-secret identity the sidecar-hosted agenthost
// reports to the agent (LookupConversation) and stamps on the verbs' relay calls. It is
// NOT the authority for org-scoping — the orchestrator binds that from its own
// ConversationInfo on every relayed op — so a sidecar cannot escalate by lying here.
type AgentHostInfo struct {
	OrgID          string `json:"org_id"`
	UserID         string `json:"user_id,omitempty"`
	TeamID         string `json:"team_id"`
	ConversationID string `json:"conversation_id"`
	EventTriggered bool   `json:"event_triggered,omitempty"`
}

// StartProxiesResult is KindStartProxies' response payload: the non-secret
// sandbox env entries the orchestrator stamps into the OCI spec so the agent
// reaches the sidecar's proxies (proxy URLs + per-run placeholder tokens),
// exactly the slice startProxiesForSandbox used to return in-process, plus
// the coordinates the orchestrator needs to route its OWN GetPR/agenthost
// REST calls through the sidecar's API proxies (empty when not requested).
type StartProxiesResult struct {
	Env []string `json:"env"`

	// LLMEnv is the LLM proxy's address plus this run's placeholder, in the
	// same provider vocabulary Env would have carried them in. It is here
	// unconditionally, for the caller whose model calls originate in the
	// executor process (the native engine) — no jail is ever pointed at the
	// LLM proxy. Non-secret for the reason every placeholder here is: the
	// real provider key never leaves the sidecar.
	LLMEnv []string `json:"llm_env,omitempty"`

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

	// JiraDeployment classifies the org's Jira backend (a jira.Deployment
	// value) so the orchestrator's proxy client picks the REST version the
	// non-proxy path would — Cloud's v3 needs Atlassian Document Format on a
	// write, Data Center's v2 needs wiki markup, and guessing wrong fails the
	// write. It travels because only the sidecar holds the credential the
	// classification comes from; the value itself is a closed classification,
	// not credential material, and the orchestrator maps anything it does not
	// recognize to the Data Center default both backends serve.
	JiraDeployment string `json:"jira_deployment,omitempty"`

	// GitHubCredential names the tier of the GitHub credential these proxies
	// inject (a domain.Credential* value), so the orchestrator's audit rows
	// record the credential that actually made each write. It travels for the
	// same reason JiraDeployment does and only that reason: the sealed bundle
	// opens in the sidecar alone, the executor's secret store is disabled, and
	// an installation token and a PAT are indistinguishable strings on the
	// wire — so nothing on the receiving side could re-derive it. The value is
	// a closed classification, never credential material.
	GitHubCredential string `json:"github_credential,omitempty"`

	// GHChannelHost is the injector's bound "host:port" (no scheme) — the value
	// the orchestrator stamps into the sandbox as GH_HOST. GHChannelToken is the
	// per-run placeholder the agent's gh presents (GH_ENTERPRISE_TOKEN). Both
	// empty when GHChannelEnabled was false. The injector's per-run cert is
	// written by the sidecar to a deterministic path and mounted by the
	// orchestrator, so it does not travel on this result.
	GHChannelHost  string `json:"gh_channel_host,omitempty"`
	GHChannelToken string `json:"gh_channel_token,omitempty"`
}

// RelayCallBody is the payload of both KindRelayCall and KindRelayNotify: the
// generic sidecar → orchestrator relay envelope. Namespace + Op name the op
// (core reads/authz/records under "core"; a provider's policy ops under its
// namespace); Args is the op-specific request, opaque to this transport (the
// op's registered handler owns the shape on both ends). A Call's response is
// the op's marshaled result carried in the Frame body; a Notify awaits none.
//
// The envelope deliberately carries NO org id — the orchestrator binds
// identity from the supervised run's ConversationInfo, so a sidecar cannot steer an op
// at another org's data. This is the same property the retired per-op Kinds
// had, now expressed once for every op instead of per message type.
type RelayCallBody struct {
	Namespace string          `json:"ns"`
	Op        string          `json:"op"`
	Args      json.RawMessage `json:"args,omitempty"`
}
