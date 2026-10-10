// Package integrations bundles the well-known integration secrets
// (GitHub, Jira, Linear) into the auth.Credentials transport shape every
// downstream consumer already deconstructs.
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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

// credsLog carries this package's one warning: an org whose stored GitHub
// credential class this build does not understand.
var credsLog = logging.Component("integrations")

// The four well-known integration secret keys. The local SQLite shim
// uses these names verbatim as keychain entry keys.
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

// Linear service-credential keys. KeyLinearAuthMethod is the linear.AuthMethod
// marker naming the shape the org's credential takes: an API key stored under
// KeyLinearAPIKey, or an app install whose refresh-token envelope is stored
// under KeyLinearAppInstall. An absent marker with a key reads as the API key
// shape: an app install stores its envelope under its own key, never this one.
// internal/linear's resolver keeps its own copies of the keys it reads (it
// cannot import this package); its keys_drift_test pins the agreement.
//
// KeyLinearBoundAs is not a credential: it holds who the bound credential
// validated as, which the access status read displays. It is stored as a
// secret so it is written and cleared with the credential it describes.
const (
	KeyLinearAPIKey     = "linear_api_key"
	KeyLinearAuthMethod = "linear_auth_method"
	KeyLinearAppInstall = "linear_app_install"
	KeyLinearBoundAs    = "linear_bound_as"
)

// legacyJiraDisplayName is the legacy key that held the Jira display
// name in the keychain. Jira identity now lives in the host-scoped
// user_jira_identities table, but ClearJira and Clear still
// sweep this key so an upgrade from an older install leaves no orphan
// keychain row.
const legacyJiraDisplayName = "jira_display_name"

// legacyGitHubUsername is the legacy key that held the org bot's GitHub login
// in the keychain. GitHub identity moved into the users.github_username
// DB column, so nothing writes this key anymore — but AllKeys still sweeps it so
// an upgrade from an older install leaves no orphan keychain row (same
// rationale as legacyJiraDisplayName). It has no companion DB ref, so it's safe
// to sweep with the rest of the integration-credential keys.
const legacyGitHubUsername = "github_username"

// Org-level secret keys that are NOT GitHub/Jira service credentials but DO
// live in the same keychain/vault, so a full local uninstall must still sweep
// them. They are deliberately excluded from AllKeys: each of these keys has
// companion DB state (org_settings.anthropic_api_key_ref / the org_jira_apps
// row) that only its own credential route reconciles — sweeping the secret
// through the shared key set would dangle the ref (the UI would report
// "configured" against a key that's gone). They belong only in the full-wipe
// AllLocalSweepKeys (uninstall / clean-slate, where the whole DB is removed
// anyway).
//
// Literals are duplicated here because integrations can't import server /
// agentproc (server imports integrations — cycle), the same constraint that
// forces the hand-copied Jira keys above. Each duplicate is drift-pinned from
// the consuming package (which can import integrations) against these exports:
//   - KeyAnthropicAPIKey: internal/server.secretKeyAnthropicAPIKey (the write
//     path, pinned by server.TestSecretKeyLiteralsMatchIntegrations) and
//     internal/agentproc.secretAnthropicAPIKey (the read path, pinned by
//     agentproc.TestAnthropicKeyMatchesIntegrations).
//   - KeyJiraOAuthClientSecret: internal/server.jiraOAuthClientSecretKey
//     (pinned by server.TestSecretKeyLiteralsMatchIntegrations).
//   - KeyLinearOAuthClientSecret: internal/server.linearOAuthClientSecretKey
//     (pinned by the same test), whose companion is the org_linear_apps row.
//
// agentproc's anthropic_auth_token / anthropic_base_url are intentionally NOT
// here: they are read-only resolver inputs with no local-mode write path (no
// SecretStore.Put), so there's never a keychain row to sweep.
//
// The Bedrock keys ARE here: POST /api/bedrock/connect (internal/server) is a
// local-mode write path for all seven, so a full local uninstall must sweep
// them. Read-path literals live in internal/agentproc's catalog; write-path
// literals in internal/server; both are drift-pinned against these exports
// (agentproc.TestBedrockKeysMatchIntegrations /
// server.TestSecretKeyLiteralsMatchIntegrations).
const (
	KeyAnthropicAPIKey         = "anthropic_api_key"
	KeyJiraOAuthClientSecret   = "jira_oauth_client_secret"
	KeyLinearOAuthClientSecret = "linear_oauth_client_secret"

	KeyAWSAccessKeyID        = "aws_access_key_id"
	KeyAWSSecretAccessKey    = "aws_secret_access_key"
	KeyAWSSessionToken       = "aws_session_token"
	KeyAWSRegion             = "aws_region"
	KeyAWSBearerTokenBedrock = "aws_bearer_token_bedrock"
	KeyBedrockModelID        = "bedrock_model_id"
	KeyBedrockBaseURL        = "bedrock_base_url"

	// Bedrock IAM-role auth (the short-lived-credential method): the
	// customer role the control process assumes (aws_role_arn, non-secret
	// but stored in the same bag for uniformity) and the TF-generated
	// confused-deputy External ID (aws_external_id). Neither is a
	// long-lived credential — the org stores no Bedrock secret at all in
	// this mode; the brain mints short-lived STS session creds per run.
	KeyAWSRoleARN    = "aws_role_arn"
	KeyAWSExternalID = "aws_external_id"
)

// BedrockKeys returns every Bedrock-related secret key the connect flow
// manages: both auth methods' credentials plus the non-secret config
// riding the same vault. One list so the write path (replace/clear), the
// uninstall sweep, and the drift tests all agree on the full set. Writers take
// the keys in this order, for the reason GitHubKeys gives.
func BedrockKeys() []string {
	return []string{
		KeyAWSAccessKeyID, KeyAWSSecretAccessKey, KeyAWSSessionToken,
		KeyAWSRegion, KeyAWSBearerTokenBedrock, KeyBedrockModelID, KeyBedrockBaseURL,
		KeyAWSRoleARN, KeyAWSExternalID,
	}
}

// AllKeys returns the integration-credential keys the SecretStore manages for a
// tenant — the GitHub, Jira and Linear well-known keys plus the legacy keys
// still swept alongside them. It is the per-credential unbind routes' shared
// vocabulary and the base of the uninstall sweep, and it stays scoped to
// credentials whose companion DB state those routes reconcile.
//
// It is NOT the full uninstall sweep: org-level secrets that live in the same
// keychain but aren't integration credentials (the Anthropic API key, the
// Atlassian OAuth client secret) and the dynamic per-GitHub-App keys are
// deliberately excluded — see AllLocalSweepKeys, which a full local uninstall /
// clean-slate uses instead.
func AllKeys() []string {
	return []string{
		KeyGitHubURL, KeyGitHubPAT,
		KeyJiraURL, KeyJiraPAT, KeyJiraEmail, KeyJiraAPIToken, KeyJiraAuthMethod,
		KeyLinearAPIKey, KeyLinearAuthMethod, KeyLinearAppInstall, KeyLinearBoundAs,
		legacyJiraDisplayName, legacyGitHubUsername,
	}
}

// AllLocalSweepKeys returns every STATIC keychain key a full local uninstall /
// clean-slate must remove: the per-org integration set (AllKeys) plus the
// org-level secrets that share the keychain but aren't integration credentials
// (the Anthropic API key, the Atlassian OAuth client secret). It is a SUPERSET
// of AllKeys, used only by the full-wipe paths (cmd/uninstall, clean-slate.sh)
// — never by the per-org integrations.Clear, which can't reconcile those keys'
// companion DB refs (see the KeyAnthropicAPIKey/KeyJiraOAuthClientSecret note).
//
// Dynamic per-GitHub-App keys (github_app_<id>_{pem,client_secret,webhook_secret})
// can't live in a static list; uninstall enumerates configured App ids from
// org_github_apps and appends GitHubAppKeysFor(id).All() for each before
// sweeping. Keep scripts/clean-slate.sh's hardcoded keychain list in sync with
// this function.
func AllLocalSweepKeys() []string {
	return append(append(AllKeys(), KeyAnthropicAPIKey, KeyJiraOAuthClientSecret, KeyLinearOAuthClientSecret), BedrockKeys()...)
}

// GitHubAppKeyset is the trio of keychain/vault keys one registered GitHub App
// custodies its secrets under: the PEM private key, the OAuth client secret, and
// the webhook secret. Composed per App id (no static list possible).
type GitHubAppKeyset struct {
	PEM           string
	ClientSecret  string
	WebhookSecret string
}

// GitHubAppKeysFor composes an App's secret-key names from its id. This is the
// single source of truth for the github_app_<id>_* shape: the write paths that
// store the secrets (internal/server/github_app_import.go and
// github_app_register.go) and the uninstall sweep both compose through here, so
// the names they write and the names they later remove can't drift.
func GitHubAppKeysFor(appID string) GitHubAppKeyset {
	return GitHubAppKeyset{
		PEM:           "github_app_" + appID + "_pem",
		ClientSecret:  "github_app_" + appID + "_client_secret",
		WebhookSecret: "github_app_" + appID + "_webhook_secret",
	}
}

// All returns the keyset as a slice (PEM, client secret, webhook secret) for the
// uninstall keychain sweep, which deletes all three regardless of which the App
// actually populated — a Delete of an absent key is a no-op.
func (k GitHubAppKeyset) All() []string {
	return []string{k.PEM, k.ClientSecret, k.WebhookSecret}
}

// SlackWorkspaceKeyset is the trio of org_secrets key names one connected
// (Slack workspace, Slack app) pair custodies its credentials under: the bot
// token (always present — every transport needs it to call the Slack API),
// the signing secret (events_api transport), and the app-level token
// (socket transport). Composed per (workspace id, app id) — Slack's team ID
// and app ID — the dynamic-keyset analog of GitHubAppKeyset. Keyed on the
// pair, not the workspace id alone, because the binding is (workspace,
// app): two different apps installed in the same workspace (one per TF org,
// say) must never share a credential slot.
type SlackWorkspaceKeyset struct {
	BotToken      string
	SigningSecret string
	AppToken      string
}

// SlackWorkspaceKeysFor composes a connected (workspace, app) pair's
// secret-key names from its workspace id and app id. This is the single
// source of truth for the slack_ws_<ws>_<app>_* shape: ee/slack's connect
// handler (which writes these secrets) and its delete handler (which sweeps
// them) both compose through here, so the names written and the names later
// removed can't drift — mirrors GitHubAppKeysFor.
func SlackWorkspaceKeysFor(workspaceID, apiAppID string) SlackWorkspaceKeyset {
	prefix := "slack_ws_" + workspaceID + "_" + apiAppID + "_"
	return SlackWorkspaceKeyset{
		BotToken:      prefix + "bot_token",
		SigningSecret: prefix + "signing_secret",
		AppToken:      prefix + "app_token",
	}
}

// All returns the keyset as a slice (bot token, signing secret, app token)
// for the disconnect sweep, which deletes all three regardless of which the
// workspace actually populated — a Delete of an absent key is a no-op.
func (k SlackWorkspaceKeyset) All() []string {
	return []string{k.BotToken, k.SigningSecret, k.AppToken}
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
// keychain, or the encrypted file when no keychain is available.
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
	get(KeyLinearAPIKey, &creds.LinearAPIKey)
	get(KeyLinearAuthMethod, &creds.LinearAuthMethod)
	var install string
	get(KeyLinearAppInstall, &install)
	creds.LinearAppInstalled = install != ""
	if len(errs) > 0 {
		return creds, errors.Join(errs...)
	}
	return creds, nil
}

// LoadSystem is the system/background twin of Load: it reads the exact same
// integration secrets for orgID into the same auth.Credentials shape, but
// through the claims-free SecretStore.GetSystem door instead of the
// claims-checked Get. It exists for callers that hold no request JWT — the
// multi-mode pollers being the motivating case: their goroutines have no
// request.jwt.claims, so the Postgres vault wrapper would reject Get (RLS
// enforces org_id == current_org_id()). GetSystem runs on the system/admin
// pool, trusts the passed orgID, and performs no current_org_id() check.
//
// System-code-only — same discipline as SecretStore.GetSystem. Request
// handlers must use the claims-checked Load; reaching for LoadSystem inside a
// request handler bypasses the per-tenant RLS gate. In local mode GetSystem
// forwards to the keychain exactly as Get does, so LoadSystem is identical to
// Load at N=1.
func LoadSystem(ctx context.Context, secrets db.SecretStore, orgID string) (auth.Credentials, error) {
	var (
		creds auth.Credentials
		errs  []error
	)
	get := func(key string, dst *string) {
		v, err := secrets.GetSystem(ctx, orgID, key)
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
	get(KeyLinearAPIKey, &creds.LinearAPIKey)
	get(KeyLinearAuthMethod, &creds.LinearAuthMethod)
	var install string
	get(KeyLinearAppInstall, &install)
	creds.LinearAppInstalled = install != ""
	if len(errs) > 0 {
		return creds, errors.Join(errs...)
	}
	return creds, nil
}

// Save writes the GitHub and Jira halves of the bundle. Empty strings are
// skipped (not written as "") — handlers that want to clear a field call the
// targeted Clear* helpers instead.
//
// The Linear fields are not written here: an app install's Linear credential is
// an envelope the bundle does not carry, so the bundle cannot express every
// Linear credential and writes none of them.
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

// LinearSystemConfig builds the linear.Config for an org's stored Linear
// service credential from an already-loaded Credentials bundle, for handlers
// that hold one. ok is true only for the api_key shape (or no marker) with a
// key: an app install's credential is an access token the resolver mints,
// which the bundle does not carry, so those callers go through
// linear.Resolver.ForSystem.
func LinearSystemConfig(c auth.Credentials) (linear.Config, bool) {
	switch linear.AuthMethod(c.LinearAuthMethod) {
	case linear.AuthMethodAPIKey, "":
		if c.LinearAPIKey != "" {
			return linear.APIKey(c.LinearAPIKey), true
		}
	}
	return linear.Config{}, false
}

// LinearSystemConfigured reports whether the org has a Linear service
// credential: a key under an api_key marker or no marker, or an install
// envelope under an app_install marker. An app_install marker with no envelope
// is an install Linear revoked, which reads as not configured. Whether a
// stored envelope still mints is the resolver's to answer, so a configured
// org can still fail to resolve.
func LinearSystemConfigured(c auth.Credentials) bool {
	switch linear.AuthMethod(c.LinearAuthMethod) {
	case linear.AuthMethodAPIKey, "":
		return c.LinearAPIKey != ""
	case linear.AuthMethodAppInstall:
		return c.LinearAppInstalled
	default:
		return false
	}
}

// GitHubReady reports whether orgID's GitHub access resolves to a usable
// credential. One derivation stands behind both the setup gate and the org's
// github event-source availability, so the two cannot drift into disagreeing
// about whether GitHub is connected.
//
// The org's credential CLASS decides, and it decides FIRST — a stored PAT is
// not an answer on its own. The question this function is asked is whether a
// resolution would succeed, so the only honest way to answer it is the way a
// resolution decides: every arm below mirrors github.activeApp, and the PAT
// signal stands exactly where that would actually borrow a PAT.
//
//	pat          the PAT is the credential. It answers.
//	byo_app      a live App answers; a registered-but-staged one is the middle
//	             of a PAT→App switch, where the PAT is still what resolves, so
//	             there the PAT answers.
//	managed_app  the deployment's shared App, and NEVER a PAT — resolution
//	             mints from that App or fails, so a PAT sitting in the secret
//	             store is a credential this org will never act on and must not
//	             be read as readiness.
//	unknown      nothing resolves at all (the resolver refuses the class
//	             outright), so neither does this.
//
// Reading credential presence ahead of the class is the same inference the
// class column exists to remove, one layer up: it answers "a credential exists"
// when the question was "would this workspace's credential system use it".
//
// The managed arm asks the one question that has an answer here: has this
// workspace bound the shared App to any account? The App itself is a deployment
// fact — configured in the operator's environment, not in anything this
// function can read — so what makes it READY FOR THIS ORG is the bind, which is
// also the only thing the org can do about it. Zero installations is a
// workspace that has connected nothing, which is exactly what the setup step
// exists to prompt.
//
// A failed read is an ERROR, never a false. Reporting one as "not connected"
// is indistinguishable to the caller from the real answer, and a caller that
// would rather degrade than fail can say so at its own call site — the setup
// gate does exactly that, falling back to the PAT signal on a store blip.
func GitHubReady(ctx context.Context, orgs db.OrgsStore, apps db.GitHubAppsStore, orgID string, creds auth.Credentials) (bool, error) {
	orgSet, err := orgs.GetSettings(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read org settings: %w", err)
	}
	switch orgSet.GitHubCredentialClass {
	case domain.GitHubCredentialClassBYOApp:
		app, err := apps.GetForOrg(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("read org github app: %w", err)
		}
		if app != nil && app.Active && app.ClientID != "" {
			return true, nil
		}
		// No App, or one still staged behind a live PAT: the PAT is what this
		// org resolves through until the cutover flips it.
		return creds.GitHubPAT != "", nil
	case domain.GitHubCredentialClassManagedApp:
		insts, err := apps.ListInstallationsForOrg(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("read org github app installations: %w", err)
		}
		return len(insts) > 0, nil
	case domain.GitHubCredentialClassPAT:
		return creds.GitHubPAT != "", nil
	default:
		credsLog.WarnContext(ctx, "unknown github credential class; reporting github unconfigured",
			"org", orgID, "class", orgSet.GitHubCredentialClass)
		return false, nil
	}
}

// GitHubReadySystem is the system/background twin of GitHubReady, for callers
// that hold no request JWT — same derivation, read through the claims-free
// GetSettingsSystem / GetForOrgSystem doors instead of the claims-checked
// reads. The creds bundle it takes was loaded with LoadSystem by the same
// caller. System-code-only, same discipline as LoadSystem.
func GitHubReadySystem(ctx context.Context, orgs db.OrgsStore, apps db.GitHubAppsStore, orgID string, creds auth.Credentials) (bool, error) {
	orgSet, err := orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("read org settings: %w", err)
	}
	switch orgSet.GitHubCredentialClass {
	case domain.GitHubCredentialClassBYOApp:
		app, err := apps.GetForOrgSystem(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("read org github app: %w", err)
		}
		if app != nil && app.Active && app.ClientID != "" {
			return true, nil
		}
		// No App, or one still staged behind a live PAT: the PAT is what this
		// org resolves through until the cutover flips it.
		return creds.GitHubPAT != "", nil
	case domain.GitHubCredentialClassManagedApp:
		insts, err := apps.ListInstallationsForOrgSystem(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("read org github app installations: %w", err)
		}
		return len(insts) > 0, nil
	case domain.GitHubCredentialClassPAT:
		return creds.GitHubPAT != "", nil
	default:
		credsLog.WarnContext(ctx, "unknown github credential class; reporting github unconfigured",
			"org", orgID, "class", orgSet.GitHubCredentialClass)
		return false, nil
	}
}

// GitHubKeys returns the org GitHub credential's keys in the order ClearGitHub
// deletes them. A writer of these keys takes them in this order too: Postgres
// holds each row's lock until commit, so two writers taking the same rows in
// different orders can deadlock.
func GitHubKeys() []string {
	return []string{KeyGitHubURL, KeyGitHubPAT}
}

// JiraKeys returns the org Jira credential's keys in the order ClearJira
// deletes them, legacy jira_display_name included (see legacyJiraDisplayName
// above). Writers take them in this order, for the reason GitHubKeys gives.
func JiraKeys() []string {
	return []string{KeyJiraURL, KeyJiraPAT, KeyJiraEmail, KeyJiraAPIToken, KeyJiraAuthMethod, legacyJiraDisplayName}
}

// LinearKeys returns the org Linear credential's keys in the order ClearLinear
// deletes them. Writers take them in this order, for the reason GitHubKeys
// gives.
func LinearKeys() []string {
	return []string{KeyLinearAPIKey, KeyLinearAuthMethod, KeyLinearAppInstall, KeyLinearBoundAs}
}

// ClearGitHub removes GitHub credentials for orgID.
func ClearGitHub(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, GitHubKeys()...)
}

// ClearJira removes Jira credentials for orgID, legacy key included.
func ClearJira(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, JiraKeys()...)
}

// ClearLinear removes the org's Linear service credential: both shapes, the
// marker naming which one is in use, and the record of who it validated as.
func ClearLinear(ctx context.Context, secrets db.SecretStore, orgID string) error {
	return clearKeys(ctx, secrets, orgID, LinearKeys()...)
}

// ClearLinearOtherScheme deletes the stored credential for the Linear shape
// NOT in use, so an org moving between an API key and an app install never
// keeps the other shape's secret beside the marker. Pass the shape now in use.
// The marker and the bound-as record are shared and left intact. A no-op for a
// shape this build does not know.
func ClearLinearOtherScheme(ctx context.Context, secrets db.SecretStore, orgID string, inUse linear.AuthMethod) error {
	switch inUse {
	case linear.AuthMethodAPIKey:
		return clearKeys(ctx, secrets, orgID, KeyLinearAppInstall)
	case linear.AuthMethodAppInstall:
		return clearKeys(ctx, secrets, orgID, KeyLinearAPIKey)
	default:
		return nil
	}
}

func clearKeys(ctx context.Context, secrets db.SecretStore, orgID string, keys ...string) error {
	for _, k := range keys {
		if _, err := secrets.Delete(ctx, orgID, k); err != nil {
			return fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return nil
}
