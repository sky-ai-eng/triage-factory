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

// Provider families, as providerFamily classifies an org's resolved env map.
// The family decides which model the request carries (see directRequestModel)
// and is the coarse dimension the span reports.
const (
	providerFamilyAnthropic = "anthropic"
	providerFamilyBedrock   = "bedrock"
	providerFamilyNone      = "none"
)

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

	// Pre-flight circuit-breaker check (see breaker.go): if this same
	// upstream provider recently failed with a transient error and its
	// cooldown hasn't elapsed, fail fast without spending a request — the
	// caller's existing fallback (leave for next poll cycle) handles it
	// exactly like any other completeDirect error.
	provider := providerKey(creds)
	// providerKey embeds the configured base URL, which can name an
	// operator's private gateway, so the span gets the coarse family
	// instead — which vendor answered is the useful dimension.
	trace.SpanFromContext(ctx).SetAttributes(telemetry.Provider(providerFamily(creds)))
	if backoffErr := r.breaker.check(provider); backoffErr != nil {
		return nil, backoffErr
	}

	model := directRequestModel(creds, opts.DirectModel)
	client, requestProvider, closeClient, err := newDirectClient(creds, model)
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
		Provider:     requestProvider,
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
func resolveDirectCreds(ctx context.Context, opts CompleteOptions) (map[string]string, error) {
	if opts.LLMResolver != nil {
		return opts.LLMResolver(ctx, opts.OrgID)
	}
	return agentproc.ResolveCredentialsForBundle(ctx, opts.Secrets, opts.OrgID)
}

// newDirectClient builds the per-call inference client for the resolved env
// map, whitelisted to the one model this call will request. It also returns
// the provider the mapping picked — the request must name the same one the
// key was minted for, and that mapping is the authority on which it is. The
// release closure shuts the embedded bifrost workers down.
func newDirectClient(creds map[string]string, model string) (*inference.Client, schemas.ModelProvider, func(), error) {
	pc, err := inference.ProviderCredentialsFromEnv(creds, []string{model})
	if err != nil {
		// ResolveCredentialsForBundle already returns
		// ErrNoCredentialsConfigured for an org with nothing configured in
		// multi mode, so an unconfigured map rarely reaches here; a resolver
		// that returned an empty map still does.
		return nil, "", nil, fmt.Errorf("systemllm: %w", err)
	}
	account, err := inference.NewAccount(pc)
	if err != nil {
		return nil, "", nil, fmt.Errorf("systemllm: %w", err)
	}
	client, err := inference.New(account)
	if err != nil {
		return nil, "", nil, fmt.Errorf("systemllm: %w", err)
	}
	return client, pc.Provider, client.Close, nil
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
		usage = directUsageFrom(completion.Usage)
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

// directUsageFrom projects the neutral usage onto the row's four token
// columns, which have always been disjoint counts — input tokens next to,
// not inclusive of, the cache buckets, which is also what the local path's
// subprocess reports.
//
// The neutral layer counts prompt tokens the OpenAI way instead: inclusive of
// cache reads and writes. Both conventions are internally consistent, and
// pricing wants the inclusive one (CostForUsage subtracts the cache buckets
// back out itself), so the projection happens here rather than by rewriting
// what the ledger means: one column that means "tokens billed at the input
// rate" under local and "everything the prompt cost" under multi is a number
// nobody can sum.
func directUsageFrom(u inference.Usage) DirectUsage {
	nonCached := u.InputTokens - u.CacheReadTokens - u.CacheCreationTokens
	if nonCached < 0 {
		// A provider that already reports prompt tokens exclusively (or a
		// malformed usage payload) must not produce a negative count.
		nonCached = 0
	}
	return DirectUsage{
		InputTokens:         nonCached,
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

// directRequestModel picks the model the request carries: the Anthropic
// branch uses the caller's pinned model id verbatim, while a Bedrock org
// resolves its own (its bedrock_model_id override, else the pinned Haiku
// inference profile). The pinned id remains the cost-accounting key for both.
func directRequestModel(creds map[string]string, pinnedModel string) string {
	if providerFamily(creds) == providerFamilyBedrock {
		return bedrockModel(creds)
	}
	return pinnedModel
}

// providerKey derives the circuit-breaker registry key for creds — mirrors
// the provider-branch precedence (Anthropic direct wins over Bedrock bearer
// wins over Bedrock SigV4) so the breaker groups exactly the calls that share
// an upstream failure domain.
//
// A custom base-URL override (a customer gateway/proxy, or a Bedrock VPC
// endpoint) is treated as a distinct fleet from the vendor's default
// endpoint, so it gets its own key. A bare region with no override is not
// customer-specific: a 529 from Anthropic's default endpoint reflects
// overall vendor capacity, not a per-key quota, so every org on that
// default endpoint deliberately shares one breaker entry — that's the
// whole point of keying by provider instead of by org.
// providerFamily is providerKey reduced to the vendor, for the span
// attribute. providerKey must distinguish two gateways for the same vendor
// (it keys a breaker, and one being down says nothing about the other); a
// span wants the opposite — bounded, and carrying no topology off the host.
func providerFamily(creds map[string]string) string {
	switch {
	case creds["ANTHROPIC_API_KEY"] != "":
		return providerFamilyAnthropic
	case creds["AWS_BEARER_TOKEN_BEDROCK"] != "",
		creds["AWS_ACCESS_KEY_ID"] != "" && creds["AWS_SECRET_ACCESS_KEY"] != "":
		return providerFamilyBedrock
	}
	return providerFamilyNone
}

func providerKey(creds map[string]string) string {
	if creds["ANTHROPIC_API_KEY"] != "" {
		if base := normalizeProviderURL(creds["ANTHROPIC_BASE_URL"]); base != "" {
			return "anthropic-direct:" + base
		}
		return "anthropic-direct:default"
	}

	hasBedrock := creds["AWS_BEARER_TOKEN_BEDROCK"] != "" ||
		(creds["AWS_ACCESS_KEY_ID"] != "" && creds["AWS_SECRET_ACCESS_KEY"] != "")
	if hasBedrock {
		region := creds["AWS_REGION"]
		if base := normalizeProviderURL(creds["ANTHROPIC_BEDROCK_BASE_URL"]); base != "" {
			return "bedrock:" + region + "@" + base
		}
		return "bedrock:" + region
	}

	// Unreachable in practice — resolveDirectCreds already errors out for an
	// org with nothing configured before providerKey is ever called — kept
	// as a defensive fallback.
	return "unknown"
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
