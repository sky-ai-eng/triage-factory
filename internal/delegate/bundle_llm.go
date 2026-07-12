package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// errNoCurrentLLMBundle is the "no current sealed bundle for this run"
// condition the executor's live LLM source surfaces so the SigV4 proxy 502s
// with the credential-refresh-lagging hint rather than signing stale.
func errNoCurrentLLMBundle(orgID, runID string) error {
	return fmt.Errorf("no current credential bundle for run %s (org %s) — credential refresh lagging, check brain health", runID, orgID)
}

// bundleLLMResolver is the narrow slice of internal/llmcred the spawner uses
// to resolve a delegated run's LLM material off the executor bundle path
// (all/local). Declared here so tests can stub it; the production impl is
// *llmcred.Resolver.
type bundleLLMResolver interface {
	ResolveForBundle(ctx context.Context, orgID, runID string) (llmcred.Material, error)
}

// llmResolverForRun builds the RunOptions.LLMResolver closure for a run: on
// the all/local path it resolves the org's LLM env map through llmcred
// (minting for role-mode Bedrock orgs, passing through otherwise). Returns
// nil when no resolver is wired (local ambient) — Run then keeps its
// built-in raw-secret resolution. On the executor role the closure is
// harmless: resolveCredentials reads the sealed bundle first and never calls
// it. A mint failure surfaces here and fails the run (nothing to fall back
// to for a role org), per the same "failure = fail" rule as the bundle path.
func (s *Spawner) llmResolverForRun(orgID, runID string) func(ctx context.Context, orgID string) (map[string]string, error) {
	resolver := s.getLLMResolver()
	if resolver == nil {
		return nil
	}
	return func(ctx context.Context, _ string) (map[string]string, error) {
		mat, err := resolver.ResolveForBundle(ctx, orgID, runID)
		if err != nil {
			return nil, err
		}
		return mat.Env, nil
	}
}

// bundleLLMSourceFor builds the RunOptions.LLMCredentialSource for an
// executor-role run (TFAC-616): the SigV4 proxy re-reads the newest sealed
// bundle's LLM triple per request instead of freezing the run-start
// snapshot, so a role-mode run whose STS session creds the brain re-mints
// mid-run keeps signing. Mirrors bundleGitProxyConfigFor's live re-read:
// every call unseals fresh via tryUnsealBundle (the same helper the
// awaiting-credentials wait uses), never a claim-time snapshot.
//
// Returns nil when this run carries no sealed bundle on ctx (the all/local
// path, where resolution went through llmResolverForRun and the proxy uses
// the static snapshot) — so the proxy only goes live where a bundle actually
// refreshes.
func (s *Spawner) bundleLLMSourceFor(ctx context.Context, info agenthost.RunInfo) func(ctx context.Context) (map[string]string, time.Time, error) {
	if _, ok := credbundle.FromContext(ctx); !ok {
		return nil
	}
	orgID := info.OrgID
	_, myBootEpoch := s.executorIdentity()
	return func(ctx context.Context) (map[string]string, time.Time, error) {
		bundle, ok := s.tryUnsealBundle(ctx, orgID, info.RunID, myBootEpoch)
		if !ok {
			// No current bundle (refresh lagging / claim moved on) — surface
			// as an error so the proxy 502s with the "credential refresh
			// lagging — check brain health" degradation, exactly like an
			// expired git token, rather than signing with stale material.
			return nil, time.Time{}, errNoCurrentLLMBundle(orgID, info.RunID)
		}
		// The brain re-mints role material at half-life; if the newest sealed
		// bundle's credential has already expired, the brain never caught up
		// (down, or STS throttled past expiry). Surface the hinted error
		// rather than signing with a dead session token (which AWS would 403
		// with ExpiredToken anyway) so the run_messages line names the cause.
		if exp := bundle.LLMExpiry(); !exp.IsZero() && exp.Before(time.Now()) {
			return nil, time.Time{}, errNoCurrentLLMBundle(orgID, info.RunID)
		}
		return bundle.LLM, bundle.LLMExpiry(), nil
	}
}
