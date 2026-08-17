// Package llmcred is the one brain-side seam every Bedrock (and Anthropic /
// bearer) LLM-credential resolution flows through. Given an org it returns
// neutral credential Material — an env-var map plus an optional expiry —
// and callers never branch on the org's auth method:
//
//   - bearer / access_keys / Anthropic orgs: the stored material passes
//     through unchanged (no expiry), byte-for-byte identical to what
//     agentproc.resolveCredentials produced before this package existed.
//   - role orgs: the resolver mints a short-lived STS session credential by
//     assuming the customer's IAM role as the deployment's OWN ambient AWS
//     identity (never a customer static key), scoped by an inline session
//     policy to bedrock:InvokeModel on the org's model and — for
//     executor-bound mints — bound to the executor's egress. The org stores
//     no long-lived Bedrock secret at all in this mode.
//
// Two mint flavors, deliberately not merged: executor-bound mints (the
// per-run bundle) carry the network condition and a per-run RoleSessionName;
// brain-bound mints (scorer / profiler / connect probe) carry NO network
// condition (they run from control-process egress) and a stable session
// name. A merged implementation would ship a scorer that 403s only in fleet
// deployments with the egress knob set.
//
// Callers: the bundle provisioner (internal/credprovision), the
// scorer/profiler/classifier via agentproc.Run's LLMResolver hook, the
// connect probe, and the role-setup endpoint. Return shape is neutral so a
// future scorer that makes live API calls instead of spawning the SDK
// consumes the same seam without re-plumbing.
package llmcred

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// Material is the neutral resolver result: the env-var map a consumer
// renders (into a proxy config or a subprocess env) plus an optional
// expiry. Expiry is zero for non-expiring passthrough material (Anthropic
// key, Bedrock bearer / access-keys, or local ambient) and the STS
// credential expiration for role-mode mints.
type Material struct {
	Env    map[string]string
	Expiry time.Time
}

// Resolver resolves LLM credential Material for an org. It holds the
// brain-side secret reader (the real, key-bearing store — never an
// executor's disabled one), the STS minter, the requested mint TTL, and the
// executor egress binding. Safe for concurrent use.
type Resolver struct {
	secrets agentproc.SecretsReader
	minter  STSMinter
	ttl     time.Duration
	egress  NetworkBinding
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	mat      Material
	halfLife time.Time
}

// NewResolver builds a Resolver. secrets is the brain-side system-door
// reader (agentproc.NewSystemSecretsReader over the real store). minter is
// the STS minter (NewAWSMinter in production; a fake in tests) — may be nil
// when no AWS SDK is wired, in which case a role-mode org surfaces
// ErrNoAmbientIdentity rather than panicking. ttl / egress come from the
// TF_LLM_CRED_TTL_SEC / TF_EXECUTOR_* env knobs.
func NewResolver(secrets agentproc.SecretsReader, minter STSMinter, ttl time.Duration, egress NetworkBinding) *Resolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	// Enforce the ≤1h ceiling at the mint boundary too (not just TTLFromEnv),
	// so a caller that constructs a resolver with a longer duration can't widen
	// the blast radius past what the feature guarantees.
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	return &Resolver{
		secrets: secrets,
		minter:  minter,
		ttl:     ttl,
		egress:  egress,
		now:     time.Now,
		cache:   map[string]cacheEntry{},
	}
}

// mintOptions distinguishes the two flavors within resolve.
type mintOptions struct {
	sessionName  string
	networkBound bool
}

// ResolveForBundle resolves executor-bound Material for run conversationID. Role
// mode carries the network condition (TF_EXECUTOR_EGRESS_CIDRS /
// TF_EXECUTOR_VPCE_IDS) and RoleSessionName = the run id (per-run CloudTrail
// attribution on the customer's side). Passthrough modes ignore conversationID and
// return the stored material unchanged.
func (r *Resolver) ResolveForBundle(ctx context.Context, orgID, conversationID string) (Material, error) {
	return r.resolve(ctx, orgID, mintOptions{sessionName: sessionNameForRun(conversationID), networkBound: true})
}

// ResolveForSystem resolves brain-bound Material for a system consumer
// (scorer / profiler / classifier / connect probe). Role mode carries NO
// network condition — these execute from control-process egress — and the
// stable sessionName the caller passes (e.g. "tf-scorer") for CloudTrail
// attribution. Passthrough modes return the stored material unchanged.
func (r *Resolver) ResolveForSystem(ctx context.Context, orgID, sessionName string) (Material, error) {
	return r.resolve(ctx, orgID, mintOptions{sessionName: clampSessionName(sanitizeSessionName(sessionName)), networkBound: false})
}

// IsRoleMode reports whether orgID is configured for Bedrock role mode
// (an aws_role_arn is stored). Used by the refresh sweep to key role
// material's re-mint cadence on expiry without reading the sealed bundle. A
// read error is treated as "not role mode" (best-effort) — the caller's
// primary refresh path (git-token age) still fires.
func (r *Resolver) IsRoleMode(ctx context.Context, orgID string) bool {
	arn, err := r.secrets.Get(ctx, orgID, integrations.KeyAWSRoleARN)
	return err == nil && strings.TrimSpace(arn) != ""
}

// TTL returns the configured mint TTL — the refresh sweep uses it to derive
// the role-material refresh threshold (half-TTL).
func (r *Resolver) TTL() time.Duration { return r.ttl }

// CallerIdentityARN returns the deployment's ambient AWS identity ARN
// (sts:GetCallerIdentity) for the role-setup trust-policy snippet.
func (r *Resolver) CallerIdentityARN(ctx context.Context) (string, error) {
	if r.minter == nil {
		return "", ErrNoAmbientIdentity
	}
	return r.minter.CallerIdentityARN(ctx)
}

// ProbeRole performs a live AssumeRole round-trip for the connect probe —
// the first Bedrock method with connect-time validation. It mints under a
// throwaway probe session name and discards the result; a nil return means
// the trust relationship works end-to-end (ambient identity present, trust
// policy allows this caller, External ID matches, duration fits). The two
// failure classes (ErrNoAmbientIdentity vs ErrAssumeRoleDenied) reach the
// handler unchanged so it can address the right audience.
func (r *Resolver) ProbeRole(ctx context.Context, roleARN, externalID string) error {
	if r.minter == nil {
		return ErrNoAmbientIdentity
	}
	policy := buildSessionPolicy("", nil)
	_, err := r.minter.AssumeRole(ctx, AssumeRoleInput{
		RoleARN:         strings.TrimSpace(roleARN),
		ExternalID:      strings.TrimSpace(externalID),
		RoleSessionName: "tf-connect-probe",
		Policy:          policy,
		Duration:        r.ttl,
	})
	return err
}

func (r *Resolver) resolve(ctx context.Context, orgID string, opts mintOptions) (Material, error) {
	roleARN, err := r.secrets.Get(ctx, orgID, integrations.KeyAWSRoleARN)
	if err != nil {
		return Material{}, fmt.Errorf("llmcred: read role arn for org %s: %w", orgID, err)
	}
	if strings.TrimSpace(roleARN) != "" {
		return r.mint(ctx, orgID, strings.TrimSpace(roleARN), opts)
	}

	// Passthrough: delegate to the exact env-map logic agentproc injects, so
	// bearer / access_keys / Anthropic / local-ambient orgs resolve
	// byte-for-byte as before this package existed (pinned by test).
	env, err := agentproc.ResolveCredentialsForBundle(ctx, r.secrets, orgID)
	if err != nil {
		return Material{}, err
	}
	return Material{Env: env}, nil
}

func (r *Resolver) mint(ctx context.Context, orgID, roleARN string, opts mintOptions) (Material, error) {
	key := cacheKey(orgID, opts)
	if m, ok := r.cachedFresh(key); ok {
		return m, nil
	}

	if r.minter == nil {
		return Material{}, fmt.Errorf("%w: no AWS SDK wired on this process", ErrNoAmbientIdentity)
	}

	externalID, err := r.secrets.Get(ctx, orgID, integrations.KeyAWSExternalID)
	if err != nil {
		return Material{}, fmt.Errorf("llmcred: read external id for org %s: %w", orgID, err)
	}
	if strings.TrimSpace(externalID) == "" {
		return Material{}, fmt.Errorf("%w: org %s is in role mode but has no External ID — re-run the Bedrock role setup", ErrRoleMisconfigured, orgID)
	}

	region, err := r.secrets.Get(ctx, orgID, integrations.KeyAWSRegion)
	if err != nil {
		return Material{}, fmt.Errorf("llmcred: read region for org %s: %w", orgID, err)
	}
	modelID, err := r.secrets.Get(ctx, orgID, integrations.KeyBedrockModelID)
	if err != nil {
		return Material{}, fmt.Errorf("llmcred: read model id for org %s: %w", orgID, err)
	}
	baseURL, err := r.secrets.Get(ctx, orgID, integrations.KeyBedrockBaseURL)
	if err != nil {
		return Material{}, fmt.Errorf("llmcred: read base url for org %s: %w", orgID, err)
	}

	var cond map[string]any
	if opts.networkBound {
		cond = networkCondition(r.egress)
	}
	policy := buildSessionPolicy(modelID, cond)

	minted, err := r.minter.AssumeRole(ctx, AssumeRoleInput{
		RoleARN:         roleARN,
		ExternalID:      strings.TrimSpace(externalID),
		RoleSessionName: opts.sessionName,
		Policy:          policy,
		Duration:        r.ttl,
	})
	if err != nil {
		return Material{}, err
	}

	env := map[string]string{
		"AWS_ACCESS_KEY_ID":       minted.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY":   minted.SecretAccessKey,
		"AWS_SESSION_TOKEN":       minted.SessionToken,
		"CLAUDE_CODE_USE_BEDROCK": "1",
	}
	if strings.TrimSpace(region) != "" {
		env["AWS_REGION"] = strings.TrimSpace(region)
	}
	if strings.TrimSpace(modelID) != "" {
		env["ANTHROPIC_MODEL"] = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(baseURL) != "" {
		env["ANTHROPIC_BEDROCK_BASE_URL"] = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}

	mat := Material{Env: env, Expiry: minted.Expiry}
	r.store(key, mat)
	return mat, nil
}

// cacheKey scopes a mint cache entry. Executor-bound mints key on the
// per-run session name (one live credential per run → O(runs)); brain-bound
// mints key on the stable session name (one per org → O(orgs)). The
// networkBound flag is folded in so the two flavors never share a cached
// credential (they carry different session policies).
func cacheKey(orgID string, opts mintOptions) string {
	bound := "sys"
	if opts.networkBound {
		bound = "exec"
	}
	return orgID + "|" + bound + "|" + opts.sessionName
}

func (r *Resolver) cachedFresh(key string) (Material, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[key]
	if !ok {
		return Material{}, false
	}
	if r.now().Before(e.halfLife) {
		return e.mat, true
	}
	return Material{}, false
}

// store caches a minted credential until its half-life — half of its ACTUAL
// lifetime (mint→expiry), not half the configured TTL, so a role whose
// max-session duration caps the credential below the TTL still caches
// sensibly instead of thrashing. A zero expiry (should not happen for a
// mint) is not cached.
func (r *Resolver) store(key string, mat Material) {
	if mat.Expiry.IsZero() {
		return
	}
	now := r.now()
	life := mat.Expiry.Sub(now)
	if life <= 0 {
		return
	}
	r.mu.Lock()
	r.cache[key] = cacheEntry{mat: mat, halfLife: now.Add(life / 2)}
	r.mu.Unlock()
}

// IsNoCredentials reports whether err is agentproc's no-credentials
// sentinel — surfaced by passthrough resolution in multi mode for an org
// with nothing configured. Exposed so callers (the brain-side consumers)
// can treat "org not configured" distinctly from a mint failure.
func IsNoCredentials(err error) bool {
	return errors.Is(err, agentproc.ErrNoCredentialsConfigured)
}

// SystemEnvResolver adapts a Resolver to the agentproc.RunOptions.LLMResolver
// shape for a brain-side system consumer (scorer / profiler / classifier /
// curator) with a stable RoleSessionName — a brain-bound mint carrying NO
// network condition. Returns nil when r is nil (local mode / ambient) so the
// consumer keeps Run's built-in resolution, unchanged. A mint failure
// surfaces from the returned func so the consumer skips just that org's cycle
// (it never crashes the per-org loop).
func SystemEnvResolver(r *Resolver, sessionName string) func(ctx context.Context, orgID string) (map[string]string, error) {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, orgID string) (map[string]string, error) {
		mat, err := r.ResolveForSystem(ctx, orgID, sessionName)
		if err != nil {
			return nil, err
		}
		return mat.Env, nil
	}
}
