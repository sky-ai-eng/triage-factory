// Package credbundle defines the sealed per-claim credential bundle content:
// the fully-resolved credential material for exactly one engagement — the LLM
// auth env map, repo-scoped GitHub installation tokens (or a PAT) for the
// engagement's authorized repo set, the org's Jira service credential, and any
// first-class provider's own opaque set.
//
// This package defines the CONTENT and nothing about its custody. The control
// plane resolves and seals a bundle (credseal.Seal) to the claim's published
// sidecar key; the per-run credential sidecar is the only process that opens
// one, and the only one that ever holds the plaintext. Everything else — the
// orchestrator included — holds ciphertext or a per-run placeholder pointing at
// one of that sidecar's proxies. A helper that handed the plaintext to another
// process would be a hole in exactly that arrangement, which is why there is
// none.
package credbundle

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Bundle is the resolved credential material for one run. BootEpoch is the
// executor boot epoch this bundle was sealed for — the executor compares it
// against its own current epoch BEFORE attempting to unseal (never Open a
// bundle sealed for an earlier boot; see the claim_credentials store doc).
type Bundle struct {
	BootEpoch int64 `json:"boot_epoch"`

	// LLM is the resolveCredentials env-var map (ANTHROPIC_API_KEY,
	// AWS_*, etc.) — nil when the org has no LLM credential configured.
	LLM map[string]string `json:"llm,omitempty"`

	// LLMExpiryUnix is the Unix-second expiry of the LLM material in LLM,
	// for role-mode Bedrock orgs whose STS session credentials are
	// short-lived. 0 / omitted for every non-expiring passthrough mode
	// (Anthropic key, Bedrock bearer / access-keys) — omitempty keeps those
	// bundles byte-for-byte identical. The executor's live LLM source
	// (internal/delegate) returns this so the SigV4 proxy re-reads the
	// newest sealed bundle before the session triple expires.
	LLMExpiryUnix int64 `json:"llm_expiry_unix,omitempty"`

	GitHub *GitHubCreds `json:"github,omitempty"`
	Jira   *JiraCreds   `json:"jira,omitempty"`

	// Providers carries the sealed credential set for each first-class
	// provider beyond the built-in GitHub/Jira (Slack, and any future one),
	// keyed by provider namespace. Each value is that provider's own opaque
	// keyed set — core seals and threads the bytes without understanding
	// them; only the provider's own sidecar-half handler (which owns the
	// shape) unmarshals and selects a member. This is what keeps core free of
	// provider-specific credential symbols: the brain resolves a provider's
	// set through its registered resolver, and the sidecar selects through its
	// registered handler, with only the JSON envelope crossing core.
	Providers map[string]json.RawMessage `json:"providers,omitempty"`
}

// ProviderCreds returns the sealed credential set for the named provider and
// whether one is present. The bytes are the provider's own keyed set — the
// caller (a provider's sidecar-half handler) unmarshals them into its own
// shape.
func (b *Bundle) ProviderCreds(namespace string) (json.RawMessage, bool) {
	raw, ok := b.Providers[namespace]
	return raw, ok && len(raw) > 0
}

// LLMExpiry returns the LLM material's expiry as a time.Time (zero when
// non-expiring). Convenience for the executor's live source and the refresh
// sweep, which reason in time.Time.
func (b *Bundle) LLMExpiry() time.Time {
	if b.LLMExpiryUnix == 0 {
		return time.Time{}
	}
	return time.Unix(b.LLMExpiryUnix, 0)
}

// GitHub credential tiers a bundle can carry, the two values of
// GitHubCreds.Mode. Named so the provisioner that writes them and the
// consumers that read them agree by symbol rather than by spelling.
const (
	GitHubModeApp = "app"
	GitHubModePAT = "pat"
)

// GitHubCreds carries either a set of repo-scoped installation tokens (an
// active GitHub App) or a single PAT (tier-3 borrow, unscoped — a PAT
// cannot be narrowed). Mode selects which. RepoTokens is keyed by
// "owner/repo".
type GitHubCreds struct {
	Mode string `json:"mode"` // GitHubModeApp | GitHubModePAT

	PAT string `json:"pat,omitempty"`

	IdentityName  string `json:"identity_name,omitempty"`
	IdentityEmail string `json:"identity_email,omitempty"`

	// BaseURL is the org's configured GitHub web base — github.com, a GHES
	// host, or a *.ghe.com data-residency tenant. It travels sealed beside the
	// tokens because it is half of the same credential: a token is only
	// meaningful against the host it was resolved for. Every GitHub lane an
	// executor binds takes its upstream from here (the git proxy verbatim, the
	// REST proxy and the gh injector through ghbase.APIBase), so nothing can
	// point a token at a host it does not belong to. Empty means the deployment default.
	BaseURL string `json:"base_url,omitempty"`

	RepoTokens map[string]RepoToken `json:"repo_tokens,omitempty"`

	// CLIToken is the single credential the real-gh channel's injector proxy
	// injects upstream on every request. For an App org it is one installation
	// token scoped to the run's authorized repos under the primary owner (minted
	// with nil permissions = the App's full grant on those repos); for a PAT org
	// it is the org PAT. The injector injects it on every request it forwards —
	// the token's own repo scope is what bounds WHICH repositories are
	// reachable, and there is no path allowlist on top of it. What the injector
	// does apply is a refusal policy on two families of write (see
	// internal/ghinjector); that is about the act, not the repository, so the
	// two bounds compose rather than overlap. Distinct from RepoTokens, which the
	// per-repo exec-verb/SDK channel and git proxy consume until their P4
	// retirement. Nil when the org has no GitHub credential.
	CLIToken *RepoToken `json:"cli_token,omitempty"`
}

// Credential names the audit-log credential the run's GitHub writes act under
// (a domain.Credential* value). The bundle is the only thing that knows this on
// an executor — the secret store is disabled there and the tier cannot be
// re-derived from an opaque bearer token — so it is read here and reported
// outward rather than guessed at each recording site.
//
// It follows Mode, which the provisioner flips to PAT the moment ANY repo in
// the run's authorized set resolves without an expiry. That is deliberately
// coarser than ResolveRepoToken's per-repo answer: a run in a mixed org can
// spend both tiers, and naming the weaker one is the honest summary — a PAT was
// in play — where naming the stronger would understate what a row's write could
// reach. A nil bundle half reports the App, which is what this column has always
// said and the only tier a deployment with no PAT can have.
func (g *GitHubCreds) Credential() string {
	if g != nil && g.Mode == GitHubModePAT {
		return domain.CredentialGitHubPAT
	}
	return domain.CredentialGitHubApp
}

// RepoToken is one repo-scoped installation token minted for the bundle.
type RepoToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// JiraCreds is the org's resolved Jira service credential — enough to
// reconstruct a *jira.Client without any further secret-store read.
type JiraCreds struct {
	URL        string `json:"url"`
	AuthMethod string `json:"auth_method"` // "cloud" | "datacenter"
	Email      string `json:"email,omitempty"`
	APIToken   string `json:"api_token,omitempty"`
	PAT        string `json:"pat,omitempty"`
}

// Marshal renders the bundle to the JSON plaintext that gets sealed.
func (b *Bundle) Marshal() ([]byte, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("credbundle: marshal: %w", err)
	}
	return data, nil
}

// Unmarshal parses a bundle from unsealed plaintext.
func Unmarshal(data []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("credbundle: unmarshal: %w", err)
	}
	return &b, nil
}
