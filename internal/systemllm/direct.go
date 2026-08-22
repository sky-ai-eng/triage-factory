package systemllm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// defaultBedrockHaikuModel is used when a Bedrock-configured org has not set
// bedrock_model_id (the ANTHROPIC_MODEL override). Same inference-profile id
// shape the Claude Code Bedrock docs recommend.
const defaultBedrockHaikuModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// completeDirect calls the org's configured Anthropic/Bedrock provider
// directly from this process — no subprocess, no sandbox — through
// internal/inference, the same embedded-bifrost client every native-loop call
// goes through. Only reachable in multi mode (see Complete).
func (r *Recorder) completeDirect(ctx context.Context, opts CompleteOptions) (*CompleteResult, error) {
	// Fail fast on a caller bug (a job that never populated the multi-mode
	// prompt split) rather than letting the provider reject an empty
	// system/user turn with a far less actionable error. DirectModel is
	// required regardless of provider: besides being the Anthropic-direct
	// request model, it doubles as the cost-accounting key below, and a
	// Bedrock org's request model is whatever inference-profile id or ARN it
	// configured — a string no pricing snapshot can be expected to carry.
	if opts.SystemPrompt == "" || opts.UserMessage == "" {
		return nil, errors.New("systemllm: SystemPrompt and UserMessage are both required in multi mode")
	}
	if opts.DirectModel == "" {
		return nil, errors.New("systemllm: DirectModel is required in multi mode")
	}

	startedAt := time.Now().UTC()

	// Role-aware resolution: a role-mode Bedrock org has no stored key —
	// LLMResolver mints a short-lived STS session triple, which the Bedrock
	// SigV4 branch signs the request with. Every other mode (and a nil
	// resolver) falls back to the raw secret map, unchanged.
	creds, err := resolveDirectCreds(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Map the env map onto provider credentials before anything else reads it,
	// so the request model, the breaker key, and the span attribute are all
	// derived from one decision about which provider this org is on rather
	// than three that have to agree.
	pc, err := mapDirectCreds(creds, opts.DirectModel)
	if err != nil {
		return nil, err
	}
	model := requestModel(pc, creds, opts.DirectModel)

	// Pre-flight circuit-breaker check (see breaker.go): if this same
	// upstream provider recently failed with a transient error and its
	// cooldown hasn't elapsed, fail fast without spending a request — the
	// caller's existing fallback (leave for next poll cycle) handles it
	// exactly like any other completeDirect error.
	provider := providerKey(pc)
	// providerKey embeds the configured base URL, which can name an
	// operator's private gateway, so the span gets the coarse family
	// instead — which vendor answered is the useful dimension.
	trace.SpanFromContext(ctx).SetAttributes(telemetry.Provider(string(pc.Provider)))
	if backoffErr := r.breaker.check(provider); backoffErr != nil {
		return nil, backoffErr
	}

	client, closeClient, err := newDirectClient(pc)
	if err != nil {
		return nil, err
	}
	// Per-call client, released after: an org's credentials can be a
	// short-lived STS triple, so nothing that outlives the call may be keyed
	// on them. One bifrost Init per call is real but small next to the call,
	// and system-job volume is a handful of calls per org per poll cycle.
	//
	// The release doesn't block the caller, because shutting bifrost down
	// waits for its in-flight worker — and on a cancelled call that worker is
	// still parked in a read only the provider, or the transport's own
	// multi-minute timeout, can end. Waiting there would trade prompt
	// cancellation for nothing: the caller's ctx is already dead.
	defer func() { go closeClient() }()

	temperature := opts.Temperature
	completion, callErr := client.Stream(ctx, inference.Request{
		Provider:     pc.Provider,
		Model:        model,
		SystemPrompt: opts.SystemPrompt,
		// One synthetic user row carrying the data being triaged. These jobs
		// have no conversation and no transcript; the row exists because
		// assembly is the only way into a request.
		Rows:        []domain.Message{{Role: "user", Content: opts.UserMessage}},
		MaxTokens:   int(opts.MaxTokens),
		Temperature: &temperature,
		// The tail here is this call's own data and is never sent again, so a
		// breakpoint on it would buy a cache write nothing can read back. The
		// system prefix still gets one, and that one repeats across every call
		// of the same job.
		NoConversationCacheBreakpoint: true,
	})
	r.breaker.recordResult(provider, isTransientFailure(ctx, callErr))

	durationMs := int(time.Since(startedAt).Milliseconds())
	r.recordDirectCall(ctx, opts, model, startedAt, durationMs, completion, callErr)

	if callErr != nil {
		// inference renders the provider's own message, the wrapped cause and
		// the status code — never request headers, so the API key/bearer
		// token can't leak through a logged error string.
		return nil, fmt.Errorf("direct llm call failed: %w", callErr)
	}

	return &CompleteResult{Text: completionText(completion)}, nil
}

// resolveDirectCreds resolves the org's LLM env map for the direct path:
// through the role-aware seam (internal/llmcred) when a resolver is wired —
// minting for role-mode Bedrock orgs, passing stored material through
// otherwise — else the raw secret store. Both return the same env-map shape
// inference.ProviderCredentialsFromEnv consumes, so the provider branch is
// unchanged either way.
//
// The model passed to either is empty on purpose. A model selects the provider
// for a run the user configured, but these jobs are pinned by TF to one small
// model that exists in every provider's vocabulary under a different spelling,
// so their provider is whatever the org configured — and requestModel below
// re-derives the right spelling from the credentials that resolution returned.
// Naming the Anthropic spelling here would refuse to run the background jobs on
// a Bedrock-only org.
func resolveDirectCreds(ctx context.Context, opts CompleteOptions) (map[string]string, error) {
	if opts.LLMResolver != nil {
		return opts.LLMResolver(ctx, opts.OrgID, "")
	}
	return agentproc.ResolveCredentialsForBundle(ctx, opts.Secrets, opts.OrgID, "")
}

// mapDirectCreds resolves the env map onto provider credentials. The key's
// whitelist names BOTH models this call could request, because which one it
// will is a consequence of the provider the mapping picks — so the mapping
// decides that once and requestModel reads its answer, rather than a second
// copy of the precedence rules deciding it in parallel. A key allowed to serve
// a model nobody asks for costs nothing; one missing the model actually
// requested fails the call.
func mapDirectCreds(creds map[string]string, pinnedModel string) (inference.ProviderCredentials, error) {
	models := []string{pinnedModel}
	if bedrock := bedrockModel(creds); bedrock != pinnedModel {
		models = append(models, bedrock)
	}
	pc, err := inference.ProviderCredentialsFromEnv(creds, models)
	if err != nil {
		// ResolveCredentialsForBundle already returns
		// ErrNoCredentialsConfigured for an org with nothing configured in
		// multi mode, so an unconfigured map rarely reaches here; a resolver
		// that returned an empty map still does.
		return inference.ProviderCredentials{}, fmt.Errorf("systemllm: %w", err)
	}
	return pc, nil
}

// requestModel picks the model the request carries: an Anthropic org sends
// the caller's pinned model id verbatim, a Bedrock org sends its own
// (bedrock_model_id override, else the pinned Haiku inference profile). The
// pinned id remains the cost-accounting key either way.
func requestModel(pc inference.ProviderCredentials, creds map[string]string, pinnedModel string) string {
	if pc.Provider == inference.ProviderBedrock {
		return bedrockModel(creds)
	}
	return pinnedModel
}

// newDirectClient builds the per-call inference client. The release closure
// shuts the embedded bifrost workers down.
func newDirectClient(pc inference.ProviderCredentials) (*inference.Client, func(), error) {
	account, err := inference.NewAccount(pc)
	if err != nil {
		return nil, nil, fmt.Errorf("systemllm: %w", err)
	}
	client, err := inference.New(account)
	if err != nil {
		return nil, nil, fmt.Errorf("systemllm: %w", err)
	}
	return client, client.Close, nil
}

// recordDirectCall builds the DirectUsage + cost figures from whatever the
// call produced (a completion, an error, or both zero) and hands them to
// RecordDirect. Isolated from completeDirect's control flow so a recording
// bug can't affect the returned result either way.
func (r *Recorder) recordDirectCall(ctx context.Context, opts CompleteOptions, model string, startedAt time.Time, durationMs int, completion *inference.Completion, callErr error) {
	var usage DirectUsage
	var traceID string
	var costUSD float64
	if completion != nil {
		usage = DirectUsageFrom(completion.Usage)
		traceID = completion.ID
		// Priced on opts.DirectModel (the pinned Anthropic model id), NOT the
		// resolved request model: a Bedrock org's model is an
		// inference-profile id or ARN it configured, and an org-specific one
		// is a string no snapshot carries — which would record $0 for every
		// call it ever makes. All three system jobs are pinned to one tier
		// (Haiku), so DirectModel prices them all no matter which concrete
		// endpoint served the request.
		//
		// A model the vendored datasheet doesn't carry still records $0, but
		// says so: a silent zero in the accounting table is indistinguishable
		// from a genuinely free call.
		cost, ok := inference.CostForUsage(opts.DirectModel, completion.Usage)
		if !ok {
			log.Warn("no pricing entry for system job model; recording zero cost",
				"job", opts.Job, "org", opts.OrgID, "model", opts.DirectModel)
		}
		costUSD = cost
	}
	r.RecordDirect(ctx, Call{
		OrgID:     opts.OrgID,
		Job:       opts.Job,
		Model:     model,
		StartedAt: startedAt,
		Metadata:  opts.Metadata,
	}, traceID, usage, costUSD, durationMs, callErr != nil)
}

// DirectUsageFrom projects the neutral usage onto the row's token columns,
// which are disjoint counts — the same projection the native loop stamps its
// message rows through, and the same shape the local path's subprocess
// reports. Cost is priced from the full usage, not from this.
//
// Exported for the availability prober, which makes its own inference call and
// hands the outcome to RecordDirect: it needs the identical projection, and a
// second copy of "which four counts, and which of them are disjoint" is how
// two rows in one ledger come to mean different things.
func DirectUsageFrom(u inference.Usage) DirectUsage {
	return DirectUsage{
		InputTokens:         u.NonCachedInputTokens(),
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
	}
}

// completionText concatenates the assistant message's text, in order. In
// practice these toolless single-turn completions produce one text block;
// concatenating defensively covers a model that splits its output across more
// than one.
func completionText(completion *inference.Completion) string {
	if completion == nil || completion.Message.Content == nil {
		return ""
	}
	content := completion.Message.Content
	if content.ContentStr != nil {
		return *content.ContentStr
	}
	var out strings.Builder
	for _, block := range content.ContentBlocks {
		if block.Type == schemas.ChatContentBlockTypeText && block.Text != nil {
			out.WriteString(*block.Text)
		}
	}
	return out.String()
}

// providerKey derives the circuit-breaker registry key from the credentials
// the call will actually use, so the key can never describe a configuration
// the request doesn't have — a region defaulted during mapping, a base URL
// trimmed there, are already applied here.
//
// A custom base-URL override (a customer gateway/proxy, or a Bedrock VPC
// endpoint) is treated as a distinct fleet from the vendor's default
// endpoint, so it gets its own key. A bare region with no override is not
// customer-specific: a 529 from Anthropic's default endpoint reflects
// overall vendor capacity, not a per-key quota, so every org on that
// default endpoint deliberately shares one breaker entry — that's the
// whole point of keying by provider instead of by org.
//
// The span attribute is deliberately NOT this: providerKey must distinguish
// two gateways for the same vendor (it keys a breaker, and one being down
// says nothing about the other), while a span wants the opposite — bounded,
// and carrying no operator topology off the host. It gets the bare provider.
func providerKey(pc inference.ProviderCredentials) string {
	base := normalizeProviderURL(pc.BaseURL)
	if pc.Provider == inference.ProviderBedrock {
		region := ""
		if pc.Bedrock != nil {
			region = pc.Bedrock.Region
		}
		if base != "" {
			return "bedrock:" + region + "@" + base
		}
		return "bedrock:" + region
	}
	if base != "" {
		return "anthropic-direct:" + base
	}
	return "anthropic-direct:default"
}

// normalizeProviderURL lowercases and trims a base-URL override so two
// configs that differ only in case or a trailing slash share one breaker
// entry instead of silently splitting into two.
func normalizeProviderURL(raw string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
}

// bedrockModel returns the org's configured bedrock_model_id override
// (surfaced as ANTHROPIC_MODEL by resolveCredentials) or the pinned Haiku
// inference-profile default.
func bedrockModel(creds map[string]string) string {
	if model := creds["ANTHROPIC_MODEL"]; model != "" {
		return model
	}
	return defaultBedrockHaikuModel
}
