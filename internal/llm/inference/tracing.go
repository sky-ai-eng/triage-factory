package inference

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/llm/inference")

// Outcomes of an llm.stream span that are not an upstream class. A failed
// call's outcome is its upstream.Class, the one classification of a provider
// failure (Classify), so the span and the upstream counters agree on what
// went wrong.
const (
	streamOK        = "ok"
	streamCancelled = "cancelled"
)

// startStream opens the span for one provider call: one HTTP request, since
// the client leaves bifrost's own retries at zero and configures no
// fallbacks. A caller that retries, as the agent loop does, therefore shows
// each attempt as its own span under whatever span it holds, with its backoff
// between them.
//
// A child of ctx's span rather than a root: a call is always part of
// something that already has its own root (a native turn, a compaction, a
// system job's completion), and is meaningless outside it.
//
// Client kind, because it is the outbound edge: the HTTP client underneath is
// fasthttp, which no transport wrapper sees, so this span is the only record
// of the request.
func startStream(ctx context.Context, req Request) (context.Context, trace.Span) {
	return tracer.Start(ctx, "llm.stream",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			telemetry.Provider(string(req.Provider)),
			telemetry.Model(req.Model),
		))
}

// firstChunkTimer wraps a caller's OnChunk so the span learns when the
// stream's first chunk arrived. started is when the request was handed to
// bifrost, which is before the connection is made: the wait it measures is
// everything a caller waits through before the provider says anything.
func firstChunkTimer(span trace.Span, started time.Time, next func()) func() {
	seen := false
	return func() {
		if !seen {
			seen = true
			span.SetAttributes(telemetry.FirstChunk(time.Since(started)))
		}
		if next != nil {
			next()
		}
	}
}

// endStream records how a call ended and ends its span.
//
// A failure's status message is its class, never the error's text: a
// rendered provider error carries the provider's own message, which can quote
// the request, and the base URL of the endpoint, which can name an operator's
// private gateway. A cancellation is no provider outcome at all, so it ends
// without an error status.
func endStream(ctx context.Context, span trace.Span, req Request, completion *Completion, err error) {
	defer span.End()
	if err == nil {
		u := completion.Usage
		span.SetAttributes(
			telemetry.Outcome(streamOK),
			telemetry.TokensInput(u.NonCachedInputTokens()),
			telemetry.TokensOutput(u.OutputTokens),
			telemetry.TokensCacheRead(u.CacheReadTokens),
			telemetry.TokensCacheWrite(u.CacheCreationTokens),
		)
		if cost, ok := CostForUsage(BilledModel(completion, req.Model), u); ok {
			span.SetAttributes(telemetry.LLMCostUSD(cost))
		}
		return
	}
	class, counted := Classify(ctx, err)
	if !counted {
		span.SetAttributes(telemetry.Outcome(streamCancelled))
		return
	}
	span.SetAttributes(telemetry.Outcome(string(class)))
	span.SetStatus(codes.Error, string(class))
}

// BilledModel is the model a completion is priced and recorded under: the one
// the provider reported serving, else the one requested. An inference profile
// or an alias can resolve to a concrete id, and the ledger records what was
// billed.
func BilledModel(completion *Completion, requested string) string {
	if completion != nil && completion.Model != "" {
		return completion.Model
	}
	return requested
}
