// Package integrations bundles the four well-known integration
// secrets (GitHub URL + PAT, Jira URL + PAT) into the auth.Credentials
// transport shape every downstream consumer already deconstructs.
// Every credential read in the binary routes through here so the
// SecretStore seam is the canonical credential path: local-mode taps
// the keychain via the SQLite store, multi-mode taps the Postgres
// vault wrapper. Callers pass the active orgID explicitly — the local
// store asserts runmode.LocalDefaultOrgID, the Postgres wrapper
// refuses if the JWT claim's org_id doesn't match.
package integrations

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// The four well-known integration secret keys. The local SQLite shim
// uses these names verbatim as keychain entry keys, and the env
// overlay (TRIAGE_FACTORY_*) is keyed off them — keep in sync with
// envKeys in internal/auth/keychain.go.
const (
	KeyGitHubURL = "github_url"
	KeyGitHubPAT = "github_pat"
	KeyJiraURL   = "jira_url"
	KeyJiraPAT   = "jira_pat"
)

// Jira Cloud service-credential keys. A Cloud org authenticates with an
// Atlassian API token (Basic auth: email + token) rather than a Data Center
// PAT (Bearer), so the Cloud halves are stored under their own keys; the
// auth-method marker records which scheme the org uses so the resolver reads
// the right pair. These mirror jira.resolver's (unexported, cycle-dodging)
// copies — keep them in sync; keys_drift_test pins the agreement.
const (
	KeyJiraEmail      = "jira_email"
	KeyJiraAPIToken   = "jira_api_token"
	KeyJiraAuthMethod = "jira_auth_method"
)

// legacyJiraDisplayName is the legacy key that held the Jira display
// name in the keychain. Jira identity now lives in the host-scoped
// user_jira_identities table (SKY-397), but ClearJira and Clear still
// sweep this key so an upgrade from an older install leaves no orphan
// keychain row.
const legacyJiraDisplayName = "jira_display_name"

// AllKeys returns every credential key the SecretStore manages for an
// integration tenant — the four well-known keys plus any legacy keys
// still swept on Clear. Exposed so callers without a SecretStore /
// DB context (e.g. cmd/uninstall) can wipe the same key set
// integrations.Clear would clear, without duplicating the literal
// list and risking drift as new keys land.
func AllKeys() []string {
	return []string{
		KeyGitHubURL, KeyGitHubPAT,
		KeyJiraURL, KeyJiraPAT, KeyJiraEmail, KeyJiraAPIToken, KeyJiraAuthMethod,
		legacyJiraDisplayName,
	}
}

// Load reads the four well-known integration secrets for orgID via
// the SecretStore and returns them in the auth.Credentials transport
// shape. The bundle exists because every downstream consumer
// (poller, dashboard, settings) wants all four strings at once;
// issuing four Get calls per handler is just noise.
//
// In multi mode orgID comes from the request context (OrgIDFrom); the
// SecretStore.Get path hits the Postgres vault wrapper which refuses
// if the claim's org_id doesn't match. In local mode orgID is
// runmode.LocalDefaultOrgID and the SecretStore reads from the
// keychain — env-overlay semantics preserved by auth.GetSecret.
func Load(ctx context.Context, secrets db.SecretStore, orgID string) (auth.Credentials, error) {
	var (
		creds auth.Credentials
		errs  []error
	)
	get := func(key string, dst *string) {
		v, err := secrets.Get(ctx, orgID, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("get %s: %w", key, err))
			return
		}
		*dst = v
	}
	get(KeyGitHubURL, &creds.GitHubURL)
	get(KeyGitHubPAT, &creds.GitHubPAT)
	get(KeyJiraURL, &creds.JiraURL)
	get(KeyJiraPAT, &creds.JiraPAT)
	get(KeyJiraEmail, &creds.JiraEmail)
	get(KeyJiraAPIToken, &creds.JiraAPIToken)
	get(KeyJiraAuthMethod, &creds.JiraAuthMethod)
	if len(errs) > 0 {
		return creds, errors.Join(errs...)
	}
	return creds, nil
}

// Save writes the four-string bundle. Empty strings are skipped (not
// written as "") — handlers that want to clear a field call the
// targeted Clear* helpers instead.
func Save(ctx context.Context, secrets db.SecretStore, orgID string, c auth.Credentials) error {
	pairs := []struct{ key, value string }{
		{KeyGitHubURL, c.GitHubURL},
		{KeyGitHubPAT, c.GitHubPAT},
		{KeyJiraURL, c.JiraURL},
		{KeyJiraPAT, c.JiraPAT},
		{KeyJiraEmail, c.JiraEmail},
		{KeyJiraAPIToken, c.JiraAPIToken},
		{KeyJiraAuthMethod, c.JiraAuthMethod},
	}
	for _, p := range pairs {
		if p.value == "" {
			continue
		}
		if err := secrets.Put(ctx, orgID, p.key, p.value, ""); err != nil {
			return fmt.Errorf("put %s: %w", p.key, err)
		}
	}
	return nil
}

// JiraSystemConfig builds the jira.Config for an org's stored Jira service
// credential from an already-loaded Credentials bundle, routed by the
// auth-method marker (Cloud API token vs DC PAT) via jira.DeploymentForMarker.
// It is the request-path analog of jira.Resolver.ForSystem for handlers that
// already hold a Credentials bundle (read under the caller's claims), so they
// don't re-read the secrets through the admin-pool resolver or re-implement the
// Cloud-vs-DC branch. The base URL is canonicalized so the client talks to the
// same origin the resolver would. ok=false when no usable credential is present
// — no URL, or the scheme's credential half is missing.
func JiraSystemConfig(c auth.Credentials) (jira.Config, bool) {
	host, ok := jira.CanonicalHost(c.JiraURL)
	if !ok {
		return jira.Config{}, false
	}
	if jira.DeploymentForMarker(jira.AuthMethod(c.JiraAuthMethod), host) == jira.DeploymentCloud {
		if c.JiraEmail == "" || c.JiraAPIToken == "" {
			return jira.Config{}, false
		}
		return jira.CloudAPIToken(host, c.JiraEmail, c.JiraAPIToken), true
	}
	if c.JiraPAT == "" {
		return jira.Config{}, false
	}
	return jira.DataCenterPAT(host, c.JiraPAT), true
}

// ClearGitHub removes GitHub credentials for orgID.
func ClearGitHub(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, KeyGitHubURL, KeyGitHubPAT)
}

// ClearJira removes Jira credentials for orgID. Also sweeps the legacy
// jira_display_name key — see legacyJiraDisplayName above.
func ClearJira(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, KeyJiraURL, KeyJiraPAT, KeyJiraEmail, KeyJiraAPIToken, KeyJiraAuthMethod, legacyJiraDisplayName)
}

// Clear removes both GitHub and Jira credentials for orgID. Includes
// the legacy jira_display_name sweep — see ClearJira.
func Clear(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, AllKeys()...)
}

func clearKeys(ctx context.Context, secrets db.SecretStore, orgID string, keys ...string) error {
	for _, k := range keys {
		if _, err := secrets.Delete(ctx, orgID, k); err != nil {
			return fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return nil
}
