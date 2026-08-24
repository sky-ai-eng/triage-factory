package agentproc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// SecretsReader is the read-only slice of the per-org secret store
// that agentproc needs to resolve LLM credentials. Declared inside
// agentproc so the package doesn't import internal/db directly —
// callers pass their db.SecretStore (which satisfies this shape
// structurally) without creating a circular dependency.
//
// The shape matches db.SecretStore.Get exactly: a missing secret is
// returned as ("", nil), and "fetch failed" surfaces a real error
// the resolver propagates.
type SecretsReader interface {
	Get(ctx context.Context, orgID, key string) (string, error)
}

// SystemSecretGetter is the claims-free secret read door agentproc needs
// to build a SecretsReader without importing internal/db (the same
// no-import discipline SecretsReader documents above). db.SecretStore's
// GetSystem method satisfies this shape structurally.
type SystemSecretGetter interface {
	GetSystem(ctx context.Context, orgID, key string) (string, error)
}

// NewSystemSecretsReader adapts a store's claims-free GetSystem door to
// the SecretsReader.Get shape so background run paths (delegation,
// resume, scorer, classifier, profiler) can resolve per-org LLM
// credentials without a request JWT. These run claims-free, so they MUST
// use the system door, not the RLS-checked Get — otherwise the multi-mode
// nil-Secrets error just trades for an RLS denial.
//
// Invariant: the orgID passed to Get always originates from the
// run / entity / task, never user input — so the unauthenticated GetSystem
// read is safe.
func NewSystemSecretsReader(g SystemSecretGetter) SecretsReader {
	return systemSecretsReader{getter: g}
}

type systemSecretsReader struct{ getter SystemSecretGetter }

func (s systemSecretsReader) Get(ctx context.Context, orgID, key string) (string, error) {
	return s.getter.GetSystem(ctx, orgID, key)
}

// ErrNoCredentialsConfigured surfaces when multi-mode invokes Run
// against an org with no Anthropic / Bedrock secret in vault. Typed
// so the caller (delegate spawner, scorer, etc.) can decide whether
// to bubble a 5xx to the user, log + skip, or mark a run as
// errored without retrying.
//
// Local mode never produces this — empty OrgID maps to "use the
// host's Claude Code subscription" via the inherited env, which is
// the supported zero-config setup for single-user installs.
var ErrNoCredentialsConfigured = errors.New("agentproc: no LLM credentials configured for org")

// Secret key catalog. Strings match the names handlers use when
// writing to the vault / keychain via SecretStore.Put — keep these
// in sync with the admin-UI write path (D14, separate ticket).
const (
	// Direct Anthropic API auth.
	secretAnthropicAPIKey   = "anthropic_api_key"
	secretAnthropicAuthMode = "anthropic_auth_token" // optional bearer for proxy/gateway
	secretAnthropicBaseURL  = "anthropic_base_url"

	// AWS Bedrock auth — access-key triple path (Option B in the
	// Claude Code docs).
	secretAWSAccessKeyID  = "aws_access_key_id"
	secretAWSSecretKey    = "aws_secret_access_key"
	secretAWSSessionToken = "aws_session_token"
	secretAWSRegion       = "aws_region"

	// AWS Bedrock auth — Bedrock API key path (Option E in the
	// Claude Code docs). Simpler than the AWS triple — no IAM, no
	// SigV4 — but only valid for Bedrock model invocation.
	secretAWSBearerTokenBedrock = "aws_bearer_token_bedrock"

	// AWS Bedrock auth — assumed-role path. The org stores no Bedrock
	// secret at all under this shape: the ARN is the record that an
	// admin bound Bedrock, and internal/llmcred mints a short-lived STS
	// session credential against it per run. Read here only to answer
	// "is Bedrock configured", never to build an env map.
	secretAWSRoleARN = "aws_role_arn"

	// Bedrock model identification (inference profile ID or
	// application inference profile ARN). Maps to ANTHROPIC_MODEL
	// per the Claude Code env var contract.
	secretBedrockModelID = "bedrock_model_id"

	// Bedrock endpoint override (both auth paths). Maps to
	// ANTHROPIC_BEDROCK_BASE_URL per the Claude Code env var contract.
	// Unset means the standard regional endpoint
	// (https://bedrock-runtime.<aws_region>.amazonaws.com); orgs on a
	// VPC interface endpoint (PrivateLink), GovCloud, or the China
	// partition set the full https URL of their endpoint here. The
	// SigV4 signing scope still comes from aws_region — the override
	// changes only the host traffic is sent (and signed) for.
	secretBedrockBaseURL = "bedrock_base_url"
)

// credentialEnvKeys is the *credential-precedence* guard — NOT an
// isolation boundary. When the resolver returns per-org creds,
// every key in this list is filtered out of os.Environ before the
// subprocess inherits, so a misconfigured deployment where the
// operator set ANTHROPIC_API_KEY at the systemd-unit level can't
// override the org-scoped key via inheritance ordering.
//
// # What this does NOT do
//
// It does NOT hide credentials from the agent process itself. The
// resolver's output goes into the same process.env the SDK reads
// from, so a malicious or jailbroken agent inside the subprocess
// can still read its own ANTHROPIC_API_KEY. Provable protection
// against that requires a local credentials proxy
// (ANTHROPIC_BASE_URL pointing at a TF-controlled service that
// injects the real key server-side) — separate ticket, post-D10.
//
// It does NOT hide host-process env or filesystem from the agent
// in pre-sandbox multi mode. That state is unsafe to ship; D10 gVisor
// is the gate.
//
// # Where the strict allowlist actually lives
//
// The D10 gVisor sandbox builds the OCI bundle's process.env from scratch — see
// the docs/for-agents/specs/sky-254-runsc-validation/ probe, which curated
// process.env to {PATH, TERM, AGENT_CURATED_KEY} and verified via
// `env` inside the sandbox that nothing else leaked through. That
// is the strict env-curation layer; this list is the lighter-touch
// precedence guard that operates in front of it (and after it, as
// the resolver's output flows through both).
//
// Sourced from a SDK source-grep of @anthropic-ai/claude-agent-sdk
// (sdk.mjs / assistant.mjs / bridge.mjs) plus the official Claude
// Code on Bedrock docs at code.claude.com/docs/en/amazon-bedrock.
// When the SDK ships new env-driven auth surfaces, add them here
// AND, if they're persisted per-org, add the matching secret key
// in the catalog above + the resolver branch in resolveCredentials.
//
// Categories:
//   - ANTHROPIC_*: direct Anthropic API auth + Anthropic-side
//     workforce identity / federation tokens
//   - AWS_*: standard AWS credential-chain env vars consumed by the
//     downstream @aws-sdk Bedrock client + the Bedrock API key path
//   - CLAUDE_CODE_USE_*: provider-selection toggles (Bedrock,
//     Mantle, Vertex, Foundry, AnthropicAWS — leaving an operator's
//     toggle in place could route a request to the wrong provider)
//   - Endpoint overrides: ANTHROPIC_BEDROCK_BASE_URL etc — could
//     redirect traffic to an operator-controlled proxy
var credentialEnvKeys = []string{
	// Anthropic direct API auth + Anthropic-side identity.
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_FEDERATION_RULE_ID",
	"ANTHROPIC_IDENTITY_TOKEN",
	"ANTHROPIC_IDENTITY_TOKEN_FILE",
	"ANTHROPIC_ORGANIZATION_ID",
	"ANTHROPIC_PROFILE",
	"ANTHROPIC_SCOPE",
	"ANTHROPIC_SERVICE_ACCOUNT_ID",

	// Bedrock / Mantle endpoint URLs + service tier + model ID.
	// ANTHROPIC_MODEL crosses categories (used by direct API too)
	// but stripping it on resolved-creds is safer than leaking a
	// parent's pinned model.
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"ANTHROPIC_BEDROCK_SERVICE_TIER",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION",

	// Provider-selection toggles. Leaving CLAUDE_CODE_USE_VERTEX in
	// place when we resolved Bedrock creds could surface as the SDK
	// trying both and failing in a confusing way.
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_SKIP_MANTLE_AUTH",

	// AWS standard credential chain + Bedrock-specific API key.
	// The @aws-sdk client reads all of these by default; an
	// operator's AWS_PROFILE pointing at a parent ~/.aws/credentials
	// row would otherwise win over the resolver's injection.
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_REGION",
	"AWS_DEFAULT_REGION",
	"AWS_PROFILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_ROLE_ARN",
	"AWS_ROLE_SESSION_NAME",
	"AWS_BEARER_TOKEN_BEDROCK",
}

// The credential resolution below answers one question: which provider's
// material does THIS run authenticate with, and what env does that render as.
//
// The model decides the provider. A conversation minted on an Anthropic catalog
// key resolves the org's Anthropic key; one minted on a Bedrock key resolves the
// org's Bedrock material. An org holding both is not asked which it prefers,
// because its picker already asked — and a global preference applied over the
// model's own provider is how a Bedrock-only run ended up authenticating with an
// Anthropic key.
//
// A model this build does not offer — the empty string, a stored id from an
// older catalog — names no provider, so there is nothing for it to decide. Those
// resolve the org's single configured provider, and fall back to
// unnamedModelPrecedence when several are configured. That is not the
// model-blind global winner this replaced: it applies only where the caller
// expressed no model at all, and a run always carries one.
//
// Local-mode behavior:
//
//   - empty OrgID, or an org with NO provider configured → returns an empty map.
//     The subprocess inherits the host's env unchanged (preserves the existing
//     "Claude Code subscription handles auth" flow + the TRIAGE_FACTORY_*-style
//     env-overlay paths some users rely on). The model is not consulted: an org
//     that configured nothing has nothing to select between, and the host env is
//     the credential.
//   - an org with a provider configured → returns that provider's env map; Run
//     filters credentialEnvKeys from os.Environ so a stale shell var can't
//     override the keychain value.
//
// Multi-mode behavior:
//
//   - empty OrgID → error. Hard caller bug; refuse loudly rather than silently
//     leaking the parent process's env (which in a hosted container would be the
//     operator-supplied shared key, which is the exact cross-tenant bleed this
//     path exists to prevent).
//   - orgID set, no credentials configured at all → ErrNoCredentialsConfigured.
//     Caller surfaces however its UX dictates; we don't fall back to the parent
//     env.
//
// In BOTH modes, an org that configured one provider and a run whose model needs
// the other is ErrProviderNotConfigured — see that sentinel.

// ErrProviderNotConfigured is what a run gets when its model is served by a
// provider the org has not connected, while it HAS connected another. The
// message names the provider and where to connect it, because the fix is a
// specific act an admin performs in a specific place.
//
// It deliberately does NOT wrap ErrNoCredentialsConfigured. That sentinel means
// "this org cannot run anything", and its tolerant handlers (the bundle
// provisioner seals an empty LLM map and lets the run fail downstream) are the
// wrong response here: the org can run, this model can't, and the answer is to
// say so once rather than to hand the executor a bundle with nothing in it.
//
// There is no fallback behind it and no retry: no other model is substituted,
// because spending someone's money on a model they did not choose is worse than
// refusing.
var ErrProviderNotConfigured = errors.New("agentproc: no credential for this model's provider")

// unnamedModelPrecedence orders the providers consulted for a model that names
// no provider and an org that configured more than one. Anthropic first: it is
// the direct path, and the one this deployment's own tooling was built against.
//
// Two kinds of value land here. A stored id from an older catalog, which is a
// leftover. And every SDK alias, which is not: on that path the id deliberately
// names no provider — the harness resolves one from whichever credential its
// environment supplies — so a local org holding both credentials is picking by
// precedence rather than by an answer anybody gave.
//
// TODO(TFAC-888): make that an explicit active-path choice for the SDK
// environment, rather than a precedence order standing in for one.
var unnamedModelPrecedence = []string{modelcatalog.ProviderAnthropic, modelcatalog.ProviderBedrock}

// ResolveCredentialsForBundle is resolveCredentials, exported for the brain-side
// credential provisioner: it needs the exact same LLM env-var map this package
// injects into the sandbox subprocess, but resolved BY the brain (which still
// holds the real secret store) and sealed into a run's credential bundle rather
// than read by the executor directly.
//
// model is the conversation's model — the provider selector. The bundle stays
// single-provider per claim: one conversation, one model, one provider's
// material.
func ResolveCredentialsForBundle(ctx context.Context, secrets SecretsReader, orgID, model string) (map[string]string, error) {
	return resolveCredentials(ctx, secrets, orgID, model, nil)
}

// ProviderForRun reports which provider a run on model authenticates through for
// orgID, or the empty string when the org has configured none (local ambient).
//
// Exported for the one caller that has to know the provider WITHOUT resolving
// the material: internal/llmcred, which mints STS session credentials for a
// role-mode Bedrock org and must not mint for a run whose model is served by
// Anthropic. Keeping the choice here rather than restating it there is what
// stops the two from disagreeing about which provider a run is on.
func ProviderForRun(ctx context.Context, secrets SecretsReader, orgID, model string) (string, error) {
	if secrets == nil || orgID == "" {
		return "", nil
	}
	configured, err := configuredProviders(ctx, secrets, orgID)
	if err != nil {
		return "", err
	}
	return providerForRun(model, configured), nil
}

// providerForRun picks the provider from the model and what the org configured.
// Split from ProviderForRun so the decision is testable without a secret store,
// and so resolveCredentials can reuse it after ONE pass over the vault.
func providerForRun(model string, configured map[string]bool) string {
	if p, ok := modelcatalog.ProviderFor(model); ok {
		// The model names its provider. Whether the org holds that credential is
		// the caller's next question, not this one's — answering "the other one"
		// here is exactly the substitution that must not happen.
		return p
	}
	if len(configured) == 0 {
		return ""
	}
	for _, p := range unnamedModelPrecedence {
		if configured[p] {
			return p
		}
	}
	// A provider outside the precedence list — impossible today (it names both),
	// and a silent empty answer would read as "ambient" to the caller.
	for p := range configured {
		return p
	}
	return ""
}

// configuredProviders reports which providers the org holds usable material for.
//
// Bedrock counts as configured under any of its three shapes, INCLUDING role
// mode, which stores no Bedrock secret at all — the role ARN is the record that
// an admin bound it, and internal/llmcred mints against it per run. A caller that
// only asked "is there a key" would read a role-mode org as unconfigured and send
// its admin to connect something they already connected.
func configuredProviders(ctx context.Context, secrets SecretsReader, orgID string) (map[string]bool, error) {
	out := map[string]bool{}

	apiKey, err := secrets.Get(ctx, orgID, secretAnthropicAPIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_api_key: %w", err)
	}
	if apiKey != "" {
		out[modelcatalog.ProviderAnthropic] = true
	}

	for _, probe := range []struct{ key string }{
		{secretAWSBearerTokenBedrock},
		{secretAWSAccessKeyID},
		{secretAWSRoleARN},
	} {
		v, err := secrets.Get(ctx, orgID, probe.key)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", probe.key, err)
		}
		if v != "" {
			out[modelcatalog.ProviderBedrock] = true
			break
		}
	}
	return out, nil
}

// resolveCredentials resolves the LLM env map for a run on model. resolver, when
// non-nil (RunOptions.LLMResolver), is the brain-side role-aware resolver
// (internal/llmcred): it fully replaces the built-in raw-secret path so a
// role-mode Bedrock org mints STS session credentials rather than reading a key
// that doesn't exist.
func resolveCredentials(ctx context.Context, secrets SecretsReader, orgID, model string, resolver func(ctx context.Context, orgID, model string) (map[string]string, error)) (map[string]string, error) {
	// The executor never reaches here (agentproc.Run launches into the prebuilt
	// network and never resolves credentials — the per-run sidecar holds them),
	// so there is no ctx-carried bundle to read: this path is all/local + the
	// brain-side ResolveCredentialsForBundle only, resolving through the live
	// secret store or the role-aware resolver. The orchestrator STRUCTURALLY
	// cannot read a run credential from ctx — there is no bundle-first branch.

	// Brain-side / all-local role-aware resolution (internal/llmcred). When
	// wired, it owns resolution outright — it reproduces the raw-secret env
	// map for bearer/access_keys/anthropic/local-ambient orgs byte-for-byte
	// (it delegates to ResolveCredentialsForBundle) and mints for role orgs.
	if resolver != nil {
		return resolver(ctx, orgID, model)
	}

	multi := runmode.Current() == runmode.ModeMulti

	if orgID == "" {
		if multi {
			return nil, fmt.Errorf("%w: empty orgID in multi mode (caller bug — pass the request's org context)", ErrNoCredentialsConfigured)
		}
		return map[string]string{}, nil
	}

	if secrets == nil {
		if multi {
			return nil, fmt.Errorf("%w: SecretsReader is nil in multi mode", ErrNoCredentialsConfigured)
		}
		// Local with no SecretsReader plumbed (test path or pre-sweep
		// caller) — degrade to inherited-env subscription fallback.
		//
		// TODO(TFAC-888): in local this arm is taken for EVERY run, because
		// nothing builds a reader there — so an org that bound its own key
		// still authenticates from the operator's environment, and the key it
		// stored decides nothing.
		return map[string]string{}, nil
	}

	configured, err := configuredProviders(ctx, secrets, orgID)
	if err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		// Nothing to select between. Local mode: subscription fallback. Multi
		// mode: hard error so the caller surfaces it.
		if multi {
			return nil, fmt.Errorf("%w: org %s has no anthropic_api_key or AWS credentials in vault", ErrNoCredentialsConfigured, orgID)
		}
		return map[string]string{}, nil
	}

	provider := providerForRun(model, configured)
	if !configured[provider] {
		return nil, fmt.Errorf("%w: model %q is served by %s, which org %s has not connected — connect it in Settings → Claude credentials",
			ErrProviderNotConfigured, model, modelcatalog.ProviderDisplayName(provider), orgID)
	}

	switch provider {
	case modelcatalog.ProviderAnthropic:
		return anthropicEnv(ctx, secrets, orgID)
	case modelcatalog.ProviderBedrock:
		return bedrockEnv(ctx, secrets, orgID, model)
	default:
		// configuredProviders only ever reports providers this switch handles;
		// a new one reaching here means a resolver arm was not written for it.
		return nil, fmt.Errorf("%w: org %s is configured for %s, which this build cannot resolve credentials for", ErrProviderNotConfigured, orgID, provider)
	}
}

// anthropicEnv renders the org's direct-Anthropic material. The API key alone is
// sufficient; the optional base URL + auth-token bearer let advanced setups front
// the SDK through a gateway.
//
// A Get error on an optional field is a real backend failure (the primary key
// read already succeeded, so the backend is reachable). Propagate it so a
// transient vault hiccup surfaces at the call site instead of silently dropping
// the gateway URL or proxy bearer and continuing with partial config — which
// would manifest later as opaque SDK auth errors. Missing secrets come back as
// ("", nil) and are handled by the non-empty checks.
func anthropicEnv(ctx context.Context, secrets SecretsReader, orgID string) (map[string]string, error) {
	apiKey, err := secrets.Get(ctx, orgID, secretAnthropicAPIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_api_key: %w", err)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("%w: org %s has no anthropic_api_key in vault", ErrProviderNotConfigured, orgID)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": apiKey}
	baseURL, err := secrets.Get(ctx, orgID, secretAnthropicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_base_url: %w", err)
	}
	if baseURL != "" {
		env["ANTHROPIC_BASE_URL"] = baseURL
	}
	authToken, err := secrets.Get(ctx, orgID, secretAnthropicAuthMode)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_auth_token: %w", err)
	}
	if authToken != "" {
		env["ANTHROPIC_AUTH_TOKEN"] = authToken
	}
	return env, nil
}

// bedrockEnv renders the org's Bedrock material under whichever shape it stored.
//
// The Bedrock API key (Option E in the Claude Code docs) is a single bearer that
// skips the AWS SigV4 chain; the access-key triple is the IAM path. Role mode
// stores neither — internal/llmcred mints for it — so this path reports it as
// unconfigured, which is correct for the only caller that can reach it: raw
// resolution with no llmcred wired.
//
// Partial access-key credentials (access key set, secret missing) mean a
// malformed admin config. Treat them as not-configured rather than
// half-injecting, so the caller sees a TF error and not an AWS-SDK one from
// inside the Node subprocess.
func bedrockEnv(ctx context.Context, secrets SecretsReader, orgID, model string) (map[string]string, error) {
	env := map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}

	bedrockBearer, err := secrets.Get(ctx, orgID, secretAWSBearerTokenBedrock)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_bearer_token_bedrock: %w", err)
	}
	switch {
	case bedrockBearer != "":
		env["AWS_BEARER_TOKEN_BEDROCK"] = bedrockBearer
	default:
		accessKey, err := secrets.Get(ctx, orgID, secretAWSAccessKeyID)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_access_key_id: %w", err)
		}
		secretKey, err := secrets.Get(ctx, orgID, secretAWSSecretKey)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_secret_access_key: %w", err)
		}
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("%w: org %s has no usable Bedrock credential in vault (a role-mode org resolves through the STS minter, not here)", ErrProviderNotConfigured, orgID)
		}
		env["AWS_ACCESS_KEY_ID"] = accessKey
		env["AWS_SECRET_ACCESS_KEY"] = secretKey
		sessionTok, err := secrets.Get(ctx, orgID, secretAWSSessionToken)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_session_token: %w", err)
		}
		if sessionTok != "" {
			env["AWS_SESSION_TOKEN"] = sessionTok
		}
	}

	if err := addBedrockConfig(ctx, secrets, orgID, model, env); err != nil {
		return nil, err
	}
	return env, nil
}

// addBedrockConfig adds the non-secret Bedrock configuration both shapes share:
// the region (endpoint + SigV4 signing scope), the model, and the endpoint
// override a VPC-interface / GovCloud / China-partition org needs.
//
// ANTHROPIC_MODEL is the run's own model when the catalog names it as a Bedrock
// key — the model the caller chose is the model the org is billed for. The
// stored bedrock_model_id is the fallback for a run that named no Bedrock model:
// it is the org's single pinned inference profile, which is what an org had
// before it could pick per run.
//
// TODO(TFAC-888): delete the pin entirely. Claude Code resolves a family alias
// to a per-region inference profile itself, with its own availability check, so
// pinning a version here overrides a choice the harness is better placed to
// make; region and endpoint env stay.
func addBedrockConfig(ctx context.Context, secrets SecretsReader, orgID, model string, env map[string]string) error {
	region, err := secrets.Get(ctx, orgID, secretAWSRegion)
	if err != nil {
		return fmt.Errorf("resolve aws_region: %w", err)
	}
	if region != "" {
		env["AWS_REGION"] = region
	}
	modelID, err := secrets.Get(ctx, orgID, secretBedrockModelID)
	if err != nil {
		return fmt.Errorf("resolve bedrock_model_id: %w", err)
	}
	if p, ok := modelcatalog.ProviderFor(model); ok && p == modelcatalog.ProviderBedrock {
		modelID = model
	}
	if modelID != "" {
		env["ANTHROPIC_MODEL"] = modelID
	}
	baseURL, err := secrets.Get(ctx, orgID, secretBedrockBaseURL)
	if err != nil {
		return fmt.Errorf("resolve bedrock_base_url: %w", err)
	}
	if baseURL != "" {
		env["ANTHROPIC_BEDROCK_BASE_URL"] = baseURL
	}
	return nil
}

// mergeEnv composes the subprocess env. When per-org credentials are
// resolved (creds non-empty), every key in credentialEnvKeys is
// stripped from the inherited env first — so a shell-set
// ANTHROPIC_API_KEY can't override the org-scoped key via
// last-entry-wins semantics. When the resolver returned empty (no
// per-org credentials configured), the inherited env passes through
// unchanged so existing local-mode flows that rely on
// process-environment ANTHROPIC_API_KEY continue to work.
//
// extraEnv is appended after the (optionally filtered) parent env;
// resolved creds are appended last so they take precedence on
// platforms where exec.Cmd respects last-wins.
func mergeEnv(parentEnv, extraEnv []string, creds map[string]string) []string {
	out := make([]string, 0, len(parentEnv)+len(extraEnv)+len(creds))

	if len(creds) == 0 {
		out = append(out, parentEnv...)
	} else {
		// Filter credential keys out of the parent env.
		out = filterEnv(parentEnv, credentialEnvKeys)
	}

	out = append(out, extraEnv...)

	for k, v := range creds {
		out = append(out, k+"="+v)
	}
	return out
}

// filterEnv returns a copy of env with any KEY=... entry whose KEY
// is in remove dropped. Case-sensitive match on the key — matches
// POSIX env semantics (the load-bearing target since multi-mode
// + gVisor requires Linux). Windows env var names are case-
// insensitive at the OS layer, so a parent's "Anthropic_Api_Key"
// would slip past the leak guard there; that's acceptable because
// Windows is only a local-mode target where the leak guard's
// multi-tenant rationale doesn't apply.
func filterEnv(env, remove []string) []string {
	if len(remove) == 0 {
		return append([]string(nil), env...)
	}
	skip := make(map[string]struct{}, len(remove))
	for _, k := range remove {
		skip[k] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		// Split on the first '=' — env values may contain '='
		// (notably some PATHs and JSON-encoded vars).
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			out = append(out, e)
			continue
		}
		if _, found := skip[e[:eq]]; found {
			continue
		}
		out = append(out, e)
	}
	return out
}
