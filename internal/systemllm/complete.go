package systemllm

import (
	"context"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// runLocal is the local-mode execution seam — agentproc.Run in production,
// swapped out in tests so Complete's mode branch can be verified without
// spawning a subprocess.
var runLocal = agentproc.Run

// CompleteOptions carries everything a toolless, single-turn system-job
// completion needs, for either mode Complete might take.
type CompleteOptions struct {
	OrgID string
	Job   string // one of the Job* constants

	// Message is the full combined instructions+data prompt, used verbatim
	// as the local-mode agentproc.Run -p message — byte-identical to the
	// prior direct-agentproc.Run behavior. Required in local mode; ignored
	// in multi mode, which uses SystemPrompt/UserMessage instead.
	Message string

	// SystemPrompt + UserMessage are used only by the direct (multi-mode)
	// API path, in place of Message: SystemPrompt carries the instructions,
	// UserMessage carries just the data being triaged. Required whenever
	// Complete may take the direct path — i.e. always, in a caller that
	// might run in multi mode.
	SystemPrompt string
	UserMessage  string

	// Model is the CLI model alias (e.g. "haiku") passed verbatim to
	// agentproc.Run in local mode.
	Model string
	// DirectModel is the pinned model id used as the Anthropic-direct
	// request model and, for every provider, the cost-accounting key passed
	// to CostFn — required regardless of which auth branch resolves. The
	// Bedrock branches resolve their own request model independently (see
	// bedrockModel) and never use DirectModel for the request itself.
	DirectModel string

	MaxTokens   int64
	Temperature float64

	TraceID string
	Secrets agentproc.SecretsReader

	// LLMResolver, when non-nil, resolves the org's LLM env map through the
	// role-aware seam (internal/llmcred) instead of the raw secret store — so
	// a role-mode Bedrock org mints short-lived STS session credentials for
	// this call rather than reading a key that doesn't exist (TFAC-616). The
	// direct (multi) path signs the Bedrock request with the returned session
	// triple; the local path forwards it to agentproc.Run. nil keeps the
	// built-in raw-secret resolution (bearer/access_keys/Anthropic, local
	// ambient) — byte-for-byte unchanged.
	LLMResolver func(ctx context.Context, orgID string) (map[string]string, error)

	// Metadata is optional per-job context (e.g. {"batch_size": 10}),
	// threaded through to the system_llm_runs row.
	Metadata map[string]any

	// CostFn computes USD cost from token counts for the direct path.
	// systemllm can't import internal/ai (internal/ai already imports
	// systemllm for Recorder), so callers pass ai.CalculateCostUSD.
	CostFn func(model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) float64
}

// CompleteResult is the model's raw text output. Callers apply
// ai.StripCodeFences + json.Unmarshal themselves, exactly as they did on
// agentproc.Outcome.Result.Result before the direct-API path existed.
type CompleteResult struct {
	Text string
}

// Complete runs one toolless, single-turn system-job completion, recording
// its cost/tokens into system_llm_runs either way. In local mode it's
// agentproc.Run exactly as before — the OAuth subscription flow only works
// through the SDK subprocess, so local never takes the direct path even if
// an API key happens to be configured. In multi mode it calls the
// configured Anthropic/Bedrock provider directly from this process — no
// subprocess, no sandbox.
func (r *Recorder) Complete(ctx context.Context, opts CompleteOptions) (*CompleteResult, error) {
	// One logical span over both branches, so a scoring batch, a repo
	// profile, or a classification vote costs the same shape of span
	// whichever mode it ran in — and so the mode is a filter rather than
	// the reason two deployments' traces can't be compared. Both branches
	// are also where all three system jobs actually spend their time, so
	// this is the per-batch LLM span for every one of them.
	ctx, span := tracer.Start(ctx, "systemllm.complete",
		trace.WithAttributes(telemetry.OrgID(opts.OrgID), telemetry.Job(opts.Job)))
	defer span.End()

	var (
		res *CompleteResult
		err error
	)
	if runmode.Current() != runmode.ModeMulti {
		span.SetAttributes(telemetry.Transport("subprocess"))
		res, err = r.completeLocal(ctx, opts)
	} else {
		span.SetAttributes(telemetry.Transport("direct"))
		res, err = r.completeDirect(ctx, opts)
	}

	switch {
	case err == nil:
		span.SetAttributes(telemetry.Outcome("completed"))
	case IsProviderBackoff(err):
		// The circuit breaker declining to spend a request on an upstream
		// it just watched fail. Every caller already treats this as "leave
		// it for the next cycle", so it is an anticipated skip and must
		// not carry an error status — otherwise a provider outage paints
		// every trace red and buries the failures that are actually TF's.
		span.SetAttributes(telemetry.Outcome("provider_backoff"))
	default:
		// Fixed message, not err.Error(): a completion failure's text can
		// carry a provider response body.
		span.SetStatus(codes.Error, "completion failed")
		span.SetAttributes(telemetry.Outcome("error"))
	}
	return res, err
}

func (r *Recorder) completeLocal(ctx context.Context, opts CompleteOptions) (*CompleteResult, error) {
	startedAt := time.Now().UTC()
	usage := &agentproc.UsageSink{}
	outcome, err := runLocal(ctx, agentproc.RunOptions{
		Model:       opts.Model,
		Message:     opts.Message,
		TraceID:     opts.TraceID,
		OrgID:       opts.OrgID,
		Secrets:     opts.Secrets,
		LLMResolver: opts.LLMResolver,
	}, usage)
	r.Record(ctx, Call{
		OrgID:     opts.OrgID,
		Job:       opts.Job,
		Model:     opts.Model,
		StartedAt: startedAt,
		Metadata:  opts.Metadata,
	}, outcome, usage)
	if err != nil {
		stderr := ""
		if outcome != nil {
			stderr = outcome.Stderr
		}
		return nil, fmt.Errorf("%w (stderr: %s)", err, stderr)
	}
	if outcome == nil || outcome.Result == nil {
		return nil, fmt.Errorf("agent: no terminal result event")
	}
	return &CompleteResult{Text: outcome.Result.Result}, nil
}
