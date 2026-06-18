package agentproc

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// resume, scorer, classifier, profiler, curator) can resolve per-org LLM
// credentials without a request JWT. These run claims-free, so they MUST
// use the system door, not the RLS-checked Get — otherwise the multi-mode
// nil-Secrets error just trades for an RLS denial (SKY-385/SKY-389).
//
// Invariant (SKY-389): the orgID passed to Get always originates from the
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

	// Bedrock model identification (inference profile ID or
	// application inference profile ARN). Maps to ANTHROPIC_MODEL
	// per the Claude Code env var contract.
	secretBedrockModelID = "bedrock_model_id"
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
// in pre-sandbox multi mode. That state is unsafe to ship; SKY-254
// (D10 gVisor) is the gate.
//
// # Where the strict allowlist actually lives
//
// SKY-254 builds the OCI bundle's process.env from scratch — see
// the docs/specs/sky-254-runsc-validation/ probe, which curated
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

// resolveCredentials looks up the env vars to inject into the Node
// subprocess for a given orgID. Anthropic and Bedrock paths are
// mutually exclusive: Anthropic key wins when both are configured
// (defensive — the admin-UI write path is expected to enforce
// exclusivity at config time, but resolver order matters if a
// malformed install ends up with both).
//
// Local-mode behavior:
//
//   - empty OrgID OR OrgID == LocalDefaultOrgID with no Anthropic /
//     Bedrock secret set → returns an empty map. The subprocess
//     inherits the host's env unchanged (preserves the existing
//     "Claude Code subscription handles auth" flow + the
//     TRIAGE_FACTORY_*-style env-overlay paths some users rely on).
//   - LocalDefaultOrgID with a configured Anthropic / Bedrock key →
//     returns the matching env map; Run filters credentialEnvKeys
//     from os.Environ so a stale shell var can't override the
//     keychain value.
//
// Multi-mode behavior:
//
//   - empty OrgID → error. Hard caller bug; refuse loudly rather
//     than silently leaking the parent process's env (which in a
//     hosted container would be the operator-supplied shared key,
//     which is the exact cross-tenant bleed the ticket exists to
//     prevent).
//   - orgID set, no credentials configured →
//     ErrNoCredentialsConfigured. Caller surfaces however its UX
//     dictates; we don't fall back to the parent env.
func resolveCredentials(ctx context.Context, secrets SecretsReader, orgID string) (map[string]string, error) {
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
		return map[string]string{}, nil
	}

	// Anthropic direct: API key alone is sufficient. Optional base
	// URL + auth-token-style bearer let advanced setups front the
	// SDK through a gateway, but neither is required.
	apiKey, err := secrets.Get(ctx, orgID, secretAnthropicAPIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve anthropic_api_key: %w", err)
	}
	if apiKey != "" {
		env := map[string]string{"ANTHROPIC_API_KEY": apiKey}
		// Optional fields: a Get error here is a real backend failure
		// (the primary key read above already succeeded, so the backend
		// is reachable). Propagate so a transient vault hiccup surfaces
		// at the call site instead of silently dropping the gateway URL
		// or proxy bearer and continuing with partial config — which
		// would manifest later as opaque SDK auth errors. Missing
		// secrets come back as ("", nil) and are handled by the
		// non-empty checks below.
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

	// Bedrock API key (Option E in the Claude Code docs): a single
	// bearer token that skips the AWS SigV4 chain. Strictly simpler
	// than the AWS triple and increasingly the recommended path for
	// orgs that don't need full IAM roles. Region is still required
	// for the SDK to construct the Bedrock endpoint URL.
	bedrockBearer, err := secrets.Get(ctx, orgID, secretAWSBearerTokenBedrock)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_bearer_token_bedrock: %w", err)
	}
	if bedrockBearer != "" {
		env := map[string]string{
			"AWS_BEARER_TOKEN_BEDROCK": bedrockBearer,
			"CLAUDE_CODE_USE_BEDROCK":  "1",
		}
		region, err := secrets.Get(ctx, orgID, secretAWSRegion)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_region: %w", err)
		}
		if region != "" {
			env["AWS_REGION"] = region
		}
		modelID, err := secrets.Get(ctx, orgID, secretBedrockModelID)
		if err != nil {
			return nil, fmt.Errorf("resolve bedrock_model_id: %w", err)
		}
		if modelID != "" {
			env["ANTHROPIC_MODEL"] = modelID
		}
		return env, nil
	}

	// Bedrock access-key triple: require both access key + secret.
	// Session token + region are optional (region defaults to
	// us-east-1 in some setups; the SDK has its own region resolution
	// chain). Partial creds (e.g. access
	// key set, secret missing) means a malformed admin config —
	// treat as not-configured rather than half-injecting, so the
	// caller sees ErrNoCredentialsConfigured and not an AWS-SDK
	// error from inside the Node subprocess.
	accessKey, err := secrets.Get(ctx, orgID, secretAWSAccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_access_key_id: %w", err)
	}
	secretKey, err := secrets.Get(ctx, orgID, secretAWSSecretKey)
	if err != nil {
		return nil, fmt.Errorf("resolve aws_secret_access_key: %w", err)
	}
	if accessKey != "" && secretKey != "" {
		env := map[string]string{
			"AWS_ACCESS_KEY_ID":       accessKey,
			"AWS_SECRET_ACCESS_KEY":   secretKey,
			"CLAUDE_CODE_USE_BEDROCK": "1",
		}
		sessionTok, err := secrets.Get(ctx, orgID, secretAWSSessionToken)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_session_token: %w", err)
		}
		if sessionTok != "" {
			env["AWS_SESSION_TOKEN"] = sessionTok
		}
		region, err := secrets.Get(ctx, orgID, secretAWSRegion)
		if err != nil {
			return nil, fmt.Errorf("resolve aws_region: %w", err)
		}
		if region != "" {
			env["AWS_REGION"] = region
		}
		modelID, err := secrets.Get(ctx, orgID, secretBedrockModelID)
		if err != nil {
			return nil, fmt.Errorf("resolve bedrock_model_id: %w", err)
		}
		if modelID != "" {
			env["ANTHROPIC_MODEL"] = modelID
		}
		return env, nil
	}

	// No Anthropic, no Bedrock. Local mode: subscription fallback.
	// Multi mode: hard error so the caller surfaces it.
	if multi {
		return nil, fmt.Errorf("%w: org %s has no anthropic_api_key or AWS credentials in vault", ErrNoCredentialsConfigured, orgID)
	}
	return map[string]string{}, nil
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
