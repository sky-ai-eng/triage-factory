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

	// Model is the org's background-jobs model — a modelcatalog key, resolved
	// from the org settings row before the call (see ModelForSettings), never
	// defaulted here. One field for both paths: local passes it to
	// `claude -p --model`, which accepts concrete ids, and multi sends it as
	// the request model and prices the call on it. Required; a call with no
	// model is refused rather than run on one nobody chose.
	Model string

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
	//
	// The model argument selects the provider, so the org's background-jobs
	// model is what decides which credential a system job resolves.
	LLMResolver func(ctx context.Context, orgID, model string) (map[string]string, error)

	// Metadata is optional per-job context (e.g. {"batch_size": 10}),
	// threaded through to the system_llm_runs row.
	Metadata map[string]any
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
	// One logical span over both branches, so a scoring batch or repo
	// profile has the same shape whichever mode ran it and the mode is a
	// filter rather than the reason two deployments' traces can't be
	// compared. This is also where both system jobs spend their time, so
	// it doubles as their per-batch LLM span.
	ctx, span := tracer.Start(ctx, "systemllm.complete",
		trace.WithAttributes(telemetry.OrgID(opts.OrgID), telemetry.Job(opts.Job)))
	defer span.End()

	// Both paths, before the mode branch: an unresolved model is the one input
	// neither can improvise. Local would hand `claude -p` an empty --model and
	// get whatever that CLI's own default happens to be; multi would send an
	// empty request model. Both are a model nobody chose, which is the
	// substitution the no-fallback rule forbids.
	if opts.Model == "" {
		span.SetStatus(codes.Error, "completion failed")
		span.SetAttributes(telemetry.Outcome("error"))
		return nil, fmt.Errorf("%w: Complete was called for org %s with no model", ErrNoModel, opts.OrgID)
	}

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
		// The breaker declining to spend a request on an upstream it just
		// watched fail. Every caller treats this as "leave it for the next
		// cycle", so an error status would let a provider outage paint
		// every trace red and bury the failures that are actually TF's.
		span.SetAttributes(telemetry.Outcome("provider_backoff"))
	default:
		// Fixed message, not err.Error() — the text can carry a provider
		// response body.
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
