// Package credbundle defines the sealed per-run credential bundle content
// (TFAC-614) and the context-threading helper that carries an unsealed
// bundle from the executor's awaiting-credentials wait down to the
// existing credential seams (agentproc.SecretsReader, gitproxy.TokenSource,
// the jira/github resolvers) without changing any of their signatures —
// every one of them already takes a context.Context.
//
// A Bundle is the fully-resolved credential material for exactly one run:
// the same shape agentproc.resolveCredentials produces for LLM auth, plus
// repo-scoped GitHub installation tokens (or a PAT) for the run's
// authorized repo set, plus the org's Jira service credential. It is what
// the brain seals (credseal.Seal) to the claiming executor's public key and
// writes to run_credentials; the executor unseals it once and threads it
// through ctx for the remainder of that run's setup and execution.
package credbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Bundle is the resolved credential material for one run. BootEpoch is the
// executor boot epoch this bundle was sealed for — the executor compares it
// against its own current epoch BEFORE attempting to unseal (never Open a
// bundle sealed for an earlier boot; see the run_credentials store doc).
type Bundle struct {
	BootEpoch int64 `json:"boot_epoch"`

	// LLM is the resolveCredentials env-var map (ANTHROPIC_API_KEY,
	// AWS_*, etc.) — nil when the org has no LLM credential configured.
	LLM map[string]string `json:"llm,omitempty"`

	GitHub *GitHubCreds `json:"github,omitempty"`
	Jira   *JiraCreds   `json:"jira,omitempty"`
}

// GitHubCreds carries either a set of repo-scoped installation tokens (an
// active GitHub App) or a single PAT (tier-3 borrow, unscoped — a PAT
// cannot be narrowed). Mode selects which. RepoTokens is keyed by
// "owner/repo".
type GitHubCreds struct {
	Mode string `json:"mode"` // "app" | "pat"

	PAT string `json:"pat,omitempty"`

	IdentityName  string `json:"identity_name,omitempty"`
	IdentityEmail string `json:"identity_email,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`

	RepoTokens map[string]RepoToken `json:"repo_tokens,omitempty"`
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

// contextKey is unexported so no other package can collide with it or
// forge a bundle into a context it doesn't own.
type contextKey struct{}

// WithBundle returns a copy of ctx carrying bundle. Called once, at the top
// of the executor's per-run dispatch after the awaiting-credentials wait
// unseals it — every credential seam invoked for the rest of that run's
// setup and execution reads it back via FromContext.
func WithBundle(ctx context.Context, bundle *Bundle) context.Context {
	return context.WithValue(ctx, contextKey{}, bundle)
}

// FromContext returns the bundle carried on ctx, if any. Every executor-
// role credential seam (resolveCredentials, the git proxy TokenSource, the
// GitHub/Jira resolvers' on-executor call sites) checks this first; ok is
// false on TF_ROLE=all, local mode, and the control role, where credentials
// resolve directly against the live secret store exactly as before —
// nothing in this package changes that path.
func FromContext(ctx context.Context) (*Bundle, bool) {
	b, ok := ctx.Value(contextKey{}).(*Bundle)
	return b, ok && b != nil
}
