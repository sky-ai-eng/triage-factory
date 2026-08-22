package delegate

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// bundleLLMResolver is the narrow slice of internal/llmcred the spawner uses
// to resolve a delegated conversation's LLM material on the all/local path.
// Declared here so tests can stub it; the production impl is *llmcred.Resolver.
type bundleLLMResolver interface {
	ResolveForBundle(ctx context.Context, orgID, conversationID, model string) (llmcred.Material, error)
}

// llmResolverForConversation builds the RunOptions.LLMResolver closure for a
// conversation: on the all/local path it resolves the org's LLM env map through llmcred
// (minting for role-mode Bedrock orgs, passing through otherwise). Returns nil
// when no resolver is wired (local ambient) — Run then keeps its built-in
// raw-secret resolution. On the executor role the closure is never consulted:
// agentproc launches into the prebuilt network and never resolves credentials
// (the sidecar holds them). A mint failure surfaces here and fails the
// conversation (nothing to fall back to for a role org).
//
// The model is the run's own — agentproc hands it back through the hook — and
// it selects the provider: an org holding both an Anthropic key and Bedrock
// material resolves whichever serves this run.
func (s *Spawner) llmResolverForConversation(orgID, conversationID string) func(ctx context.Context, orgID, model string) (map[string]string, error) {
	resolver := s.getLLMResolver()
	if resolver == nil {
		return nil
	}
	return func(ctx context.Context, _, model string) (map[string]string, error) {
		mat, err := resolver.ResolveForBundle(ctx, orgID, conversationID, model)
		if err != nil {
			return nil, err
		}
		return mat.Env, nil
	}
}
