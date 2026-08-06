package systemllm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// defaultBedrockHaikuModel is used when a Bedrock-configured org has not set
// bedrock_model_id (the ANTHROPIC_MODEL override). Same inference-profile id
// shape the Claude Code Bedrock docs recommend.
const defaultBedrockHaikuModel = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// directHTTPClient is the transport every direct provider call shares, so
// the three provider branches below get one instrumented client between
// them instead of each needing its own.
//
// Hoisted to package level because buildDirectClient runs per call: an
// anthropic.Client is constructed for every scoring batch, every repo
// profile, every classification vote, and building a fresh *http.Client
// each time would throw away the connection pool along with it.
//
// Zero Timeout on purpose — it matches http.DefaultClient, which is what
// the SDK uses when no client is supplied, so installing this changes the
// transport and nothing else. The request deadline stays where it already
// was: on the caller's context and the SDK's own per-request budget.
//
// Bedrock's SigV4 signing is unaffected — it is registered as SDK
// middleware, which runs above the HTTP client rather than inside it.
var directHTTPClient = &http.Client{Transport: telemetry.TracedTransport(nil, "llm")}

// completeDirect calls the org's configured Anthropic/Bedrock provider
// directly from this process — no subprocess, no sandbox. Only reachable in
// multi mode (see Complete).
func (r *Recorder) completeDirect(ctx context.Context, opts CompleteOptions) (*CompleteResult, error) {
	// Fail fast on a caller bug (a job that never populated the multi-mode
	// prompt split) rather than letting the SDK reject an empty system/user
	// turn with a far less actionable error. DirectModel is required
	// regardless of provider: besides being the Anthropic-direct request
	// model, it doubles as the cost-accounting key below — a resolved
	// Bedrock model (an inference-profile id/ARN) has no pricing-table
	// entry, so cost math needs the pinned Anthropic model id, not
	// whatever concrete endpoint actually served the request.
	if opts.SystemPrompt == "" || opts.UserMessage == "" {
		return nil, errors.New("systemllm: SystemPrompt and UserMessage are both required in multi mode")
	}
	if opts.DirectModel == "" {
		return nil, errors.New("systemllm: DirectModel is required in multi mode")
	}

	startedAt := time.Now().UTC()

	// Role-aware resolution (TFAC-616): a role-mode Bedrock org has no stored
	// key — LLMResolver mints a short-lived STS session triple, which
	// buildDirectClient's SigV4 branch signs the request with. Every other
	// mode (and a nil resolver) falls back to the raw secret map, unchanged.
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
	// providerKey itself is the breaker's key and embeds the configured
	// base URL, which can name an operator's private gateway — so the span
	// gets the coarse family instead. Which vendor answered is the useful
	// dimension; where it lives is not the span's business.
	trace.SpanFromContext(ctx).SetAttributes(telemetry.Provider(providerFamily(creds)))
	if backoffErr := r.breaker.check(provider); backoffErr != nil {
		return nil, backoffErr
	}

	client, model, err := buildDirectClient(creds, opts.DirectModel)
	if err != nil {
		return nil, err
	}

	msg, callErr := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       model,
		MaxTokens:   opts.MaxTokens,
		Temperature: anthropic.Float(opts.Temperature),
		System:      []anthropic.TextBlockParam{{Text: opts.SystemPrompt}},
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(opts.UserMessage))},
	})
	r.breaker.recordResult(provider, isTransientFailure(ctx, callErr))

	durationMs := int(time.Since(startedAt).Milliseconds())
	r.recordDirectCall(ctx, opts, model, startedAt, durationMs, msg, callErr)

	if callErr != nil {
		// anthropic.Error.Error() only ever formats method/URL/status/request-id
		// + the response body — never request headers, so the API key/bearer
		// token can't leak through a logged error string.
		return nil, fmt.Errorf("direct llm call failed: %w", callErr)
	}

	return &CompleteResult{Text: extractText(msg)}, nil
}

// resolveDirectCreds resolves the org's LLM env map for the direct path:
// through the role-aware seam (internal/llmcred) when a resolver is wired —
// minting for role-mode Bedrock orgs, passing stored material through
// otherwise — else the raw secret store. Both return the same env-map shape
// buildDirectClient consumes, so the provider branch below is unchanged.
func resolveDirectCreds(ctx context.Context, opts CompleteOptions) (map[string]string, error) {
	if opts.LLMResolver != nil {
		return opts.LLMResolver(ctx, opts.OrgID)
	}
	return agentproc.ResolveCredentialsForBundle(ctx, opts.Secrets, opts.OrgID)
}

// recordDirectCall builds the DirectUsage + cost figures from whatever the
// call produced (a response, an error, or both zero) and hands them to
// RecordDirect. Isolated from completeDirect's control flow so a recording
// bug can't affect the returned result either way.
func (r *Recorder) recordDirectCall(ctx context.Context, opts CompleteOptions, model string, startedAt time.Time, durationMs int, msg *anthropic.Message, callErr error) {
	var usage DirectUsage
	var traceID string
	var costUSD float64
	if msg != nil {
		usage = DirectUsage{
			InputTokens:         int(msg.Usage.InputTokens),
			OutputTokens:        int(msg.Usage.OutputTokens),
			CacheReadTokens:     int(msg.Usage.CacheReadInputTokens),
			CacheCreationTokens: int(msg.Usage.CacheCreationInputTokens),
		}
		traceID = msg.ID
		if opts.CostFn != nil {
			// Priced on opts.DirectModel (the pinned Anthropic model id), NOT
			// the resolved request model: a Bedrock call's actual model is an
			// inference-profile id/ARN (the regional default or the org's
			// bedrock_model_id override) with no pricing-table entry, which
			// would silently cost $0. All three system jobs are pinned to one
			// tier (Haiku), so DirectModel is the correct pricing key no
			// matter which concrete Bedrock endpoint served the request.
			costUSD = opts.CostFn(opts.DirectModel, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens)
		}
	}
	r.RecordDirect(ctx, Call{
		OrgID:     opts.OrgID,
		Job:       opts.Job,
		Model:     model,
		StartedAt: startedAt,
		Metadata:  opts.Metadata,
	}, traceID, usage, costUSD, durationMs, callErr != nil)
}

// extractText concatenates every text content block in the response, in
// order. In practice these toolless, non-streaming, tool-free completions
// produce exactly one text block; concatenating defensively covers a model
// that ever splits its output across more than one.
func extractText(msg *anthropic.Message) string {
	if msg == nil {
		return ""
	}
	var out strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

// providerKey derives the circuit-breaker registry key for creds — mirrors
// buildDirectClient's provider-branch precedence (Anthropic direct wins
// over Bedrock bearer wins over Bedrock SigV4) so the breaker groups
// exactly the calls that share an upstream failure domain.
//
// A custom base-URL override (a customer gateway/proxy, or a Bedrock VPC
// endpoint) is treated as a distinct fleet from the vendor's default
// endpoint, so it gets its own key. A bare region with no override is not
// customer-specific: a 529 from Anthropic's default endpoint reflects
// overall vendor capacity, not a per-key quota, so every org on that
// default endpoint deliberately shares one breaker entry — that's the
// whole point of keying by provider instead of by org.
// providerFamily is providerKey reduced to the vendor alone, for the span
// attribute. providerKey has to distinguish two different gateways for the
// same vendor — it keys a circuit breaker, and one being down says nothing
// about the other — but a span wants the opposite: a stable, bounded value
// that carries no deployment topology off the host.
func providerFamily(creds map[string]string) string {
	switch {
	case creds["ANTHROPIC_API_KEY"] != "":
		return "anthropic"
	case creds["AWS_BEARER_TOKEN_BEDROCK"] != "",
		creds["AWS_ACCESS_KEY_ID"] != "" && creds["AWS_SECRET_ACCESS_KEY"] != "":
		return "bedrock"
	}
	return "none"
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
	// as a defensive fallback matching buildDirectClient's own.
	return "unknown"
}

// normalizeProviderURL lowercases and trims a base-URL override so two
// configs that differ only in case or a trailing slash share one breaker
// entry instead of silently splitting into two.
func normalizeProviderURL(raw string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
}

// buildDirectClient resolves the provider from the env-var map
// agentproc.ResolveCredentialsForBundle returns (the same map it would have
// injected into the Node subprocess) and constructs an anthropic-sdk-go
// client for it. Mirrors resolveCredentials' precedence: Anthropic direct
// wins over Bedrock when both are somehow configured.
//
// option.WithoutEnvironmentDefaults skips the SDK's own env-var autoload —
// this process's env is not a per-org credential source, only the resolved
// map is, so every auth option below is set explicitly.
//
// pinnedModel is used verbatim as the request model for the Anthropic
// direct-API branch; the Bedrock branches resolve their own model (see
// bedrockModel) and never touch it. completeDirect has already checked it's
// non-empty.
func buildDirectClient(creds map[string]string, pinnedModel string) (anthropic.Client, string, error) {
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithHTTPClient(directHTTPClient)}

	if apiKey := creds["ANTHROPIC_API_KEY"]; apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
		if baseURL := creds["ANTHROPIC_BASE_URL"]; baseURL != "" {
			opts = append(opts, option.WithBaseURL(baseURL))
		}
		if authToken := creds["ANTHROPIC_AUTH_TOKEN"]; authToken != "" {
			// Gateway/proxy bearer, layered alongside the api-key header
			// (not instead of it) — matches resolveCredentials, which sets
			// both env vars together rather than treating them as
			// alternatives.
			opts = append(opts, option.WithAuthToken(authToken))
		}
		return anthropic.NewClient(opts...), pinnedModel, nil
	}

	if bearer := creds["AWS_BEARER_TOKEN_BEDROCK"]; bearer != "" {
		region := creds["AWS_REGION"]
		if region == "" {
			return anthropic.Client{}, "", errors.New("systemllm: bedrock bearer auth requires aws_region")
		}
		cfg := aws.Config{
			Region:                  region,
			BearerAuthTokenProvider: bedrock.NewStaticBearerTokenProvider(bearer),
		}
		opts = append(opts, bedrock.WithConfig(cfg))
		if baseURL := creds["ANTHROPIC_BEDROCK_BASE_URL"]; baseURL != "" {
			opts = append(opts, option.WithBaseURL(baseURL))
		}
		return anthropic.NewClient(opts...), bedrockModel(creds), nil
	}

	if accessKey, secretKey := creds["AWS_ACCESS_KEY_ID"], creds["AWS_SECRET_ACCESS_KEY"]; accessKey != "" && secretKey != "" {
		region := creds["AWS_REGION"]
		if region == "" {
			return anthropic.Client{}, "", errors.New("systemllm: bedrock SigV4 auth requires aws_region")
		}
		cfg := aws.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, creds["AWS_SESSION_TOKEN"]),
		}
		opts = append(opts, bedrock.WithConfig(cfg))
		if baseURL := creds["ANTHROPIC_BEDROCK_BASE_URL"]; baseURL != "" {
			opts = append(opts, option.WithBaseURL(baseURL))
		}
		return anthropic.NewClient(opts...), bedrockModel(creds), nil
	}

	// ResolveCredentialsForBundle already returns ErrNoCredentialsConfigured
	// for an org with nothing configured in multi mode, so this is
	// unreached in practice; kept as a defensive fallback.
	return anthropic.Client{}, "", fmt.Errorf("systemllm: %w", agentproc.ErrNoCredentialsConfigured)
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
