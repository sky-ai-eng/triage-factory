package agentloop

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/agentloop")

// How an agent.turn or agent.prepare span ended. The ones an iteration that
// goes on to another ends with are named here; an iteration that ends the
// engagement takes its outcome from the Result (resultOutcome), except a
// flow-control stop, which says so.
const (
	prepareOK          = "ok"
	turnToolCalls      = "tool_calls"
	turnCompacted      = "compacted"
	turnTruncatedBatch = "truncated_batch"
	turnRetryLargerCap = "retry_larger_cap"
	turnPendingInput   = "pending_input"
	turnNudged         = "nudged"
	turnStopped        = "stopped"
)

// Why a turn parked, beside outcome "parked". A guard's notice is prose for a
// person, so the span names only which kind of stop it was.
const (
	parkGuard          = "guard"
	parkDecline        = "decline"
	parkUnknownStop    = "unknown_stop"
	parkEmptyToolBatch = "empty_tool_batch"
)

// Why a compaction ran (the reason attribute of agent.compact), and which
// arm committed it (its outcome).
const (
	compactTrip       = "trip"
	compactResume     = "resume"
	compactOverflow   = "overflow"
	compactWindowWall = "window_wall"

	compactedWarm      = "warm"
	compactedCold      = "cold"
	compactedRecovered = "recovered"
)

// How an agent.tool span ended.
const (
	toolOK            = "ok"
	toolErrored       = "tool_error"
	toolDenied        = "denied"
	toolProtocolError = "protocol_error"
	toolHostLost      = "host_lost"
	toolCancelled     = "cancelled"
)

// How an agent.tool_host.restart span ended.
const (
	restartDone      = "restarted"
	restartExhausted = "exhausted"
	restartFailed    = "failed"
)

// The outcomes shared by every span here that a failure, a stop, or an outage
// can end.
const (
	outcomeFailed              = "failed"
	outcomeCancelled           = "cancelled"
	outcomeUpstreamUnavailable = "upstream_unavailable"
)

// unknownTool is the tool attribute of a call to a name the registry does not
// declare.
const unknownTool = "unknown"

// loopSpan is one of the engagement's root spans: its preparation (repair,
// the tool host's policy, a compaction on resume) or one iteration of the
// loop.
//
// A root per iteration, never one span over the engagement. An engagement has
// no bound on its length (the watchdog bounds each operation in it, and only
// the spend guard bounds how many there are), and a span exports only when it
// ends, so one held open across it would be lost to any restart of the
// executor, a rolling deploy included. An iteration is bounded by the
// deadlines of the operations in it and holds no human wait: a native run
// never prompts, and a park ends the engagement. Each root links to the
// engagement's own (Params.EngagementSpan) and carries conversation.id, the
// join that survives a link whose other side was not sampled.
type loopSpan struct {
	span    trace.Span
	outcome string
	done    bool
}

// startLoopSpan opens one of the engagement's root spans.
func (e *Engine) startLoopSpan(ctx context.Context, params Params, name string) (context.Context, *loopSpan) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(telemetry.ConversationID(params.ConversationID), telemetry.OrgID(params.OrgID)),
	}
	if params.EngagementSpan.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: params.EngagementSpan}))
	}
	ctx, span := tracer.Start(ctx, name, opts...)
	return ctx, &loopSpan{span: span}
}

// called records the provider call this iteration made: its ordinal in the
// engagement and the loop's reading of why the provider stopped.
func (l *loopSpan) called(turn int, class stopClass) {
	l.span.SetAttributes(telemetry.Turn(turn), telemetry.StopReason(class.String()))
}

// parked names why the iteration parked the engagement.
func (l *loopSpan) parked(reason string) {
	l.span.SetAttributes(telemetry.Reason(reason))
}

// next ends a span whose engagement goes on, with how it handed on.
func (l *loopSpan) next(outcome string) {
	if l == nil || l.done {
		return
	}
	l.done = true
	l.span.SetAttributes(telemetry.Outcome(outcome))
	l.span.End()
}

// finish ends the span the engagement ended in, unless it has already ended.
// Only a failure is an error status, with a fixed message: the error's text
// can carry a provider's response or a tool's output. A park, a stop, an
// outage, and a lost tool host each leave the conversation whole and are
// handed on, so each is an outcome and not an error.
func (l *loopSpan) finish(r Result) {
	if l == nil || l.done {
		return
	}
	l.done = true
	outcome := l.outcome
	if outcome == "" {
		outcome = resultOutcome(r.Kind)
	}
	l.span.SetAttributes(telemetry.Outcome(outcome))
	if r.Kind == ResultFailed {
		l.span.SetStatus(codes.Error, "engagement failed")
	}
	l.span.End()
}

// resultOutcome names how an engagement ended.
func resultOutcome(kind ResultKind) string {
	switch kind {
	case ResultConcluded:
		return "concluded"
	case ResultParked:
		return "parked"
	case ResultFailed:
		return outcomeFailed
	case ResultCancelled:
		return outcomeCancelled
	case ResultUpstreamUnavailable:
		return outcomeUpstreamUnavailable
	case ResultToolHostLost:
		return "tool_host_lost"
	}
	return "unknown"
}

// compaction runs one compaction under its own agent.compact span, a child of
// the iteration's (or the preparation's) span. Which arm committed it is the
// outcome the arm records with markCompacted; a failure is named the way an
// engagement's is.
func (e *Engine) compaction(ctx context.Context, reason string, run func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, "agent.compact", trace.WithAttributes(telemetry.Reason(reason)))
	defer span.End()
	err := run(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		span.SetAttributes(telemetry.Outcome(outcomeCancelled))
	case upstreamUnavailable(err):
		span.SetAttributes(telemetry.Outcome(outcomeUpstreamUnavailable))
	default:
		span.SetAttributes(telemetry.Outcome(outcomeFailed))
		span.SetStatus(codes.Error, "compaction failed")
	}
	return err
}

// markCompacted records which arm committed the compaction ctx's span covers.
func markCompacted(ctx context.Context, arm string) {
	trace.SpanFromContext(ctx).SetAttributes(telemetry.Outcome(arm))
}

// backoff waits out one retry of a provider call under an llm.retry.backoff
// span, the same shape as the GitHub and Jira clients' waits: which attempt
// failed, the class it failed with as the disposition, and an outcome only
// when the wait was cut short.
func (e *Engine) backoff(ctx context.Context, sleep func(context.Context, time.Duration) error, delay time.Duration, attempt int, provider string, class upstream.Class) error {
	ctx, span := tracer.Start(ctx, "llm.retry.backoff", trace.WithAttributes(
		telemetry.Provider(provider), telemetry.Attempt(attempt), telemetry.Disposition(string(class))))
	defer span.End()
	if err := sleep(ctx, delay); err != nil {
		span.SetAttributes(telemetry.Outcome(outcomeCancelled))
		return err
	}
	return nil
}

// startTool opens one tool call's agent.tool span, a child of the iteration's.
func (e *Engine) startTool(ctx context.Context, params Params, call domain.ToolCall) (context.Context, trace.Span) {
	return tracer.Start(ctx, "agent.tool", trace.WithAttributes(telemetry.Tool(toolLabel(call.Name, params))))
}

// toolLabel is a call's tool name as a span may carry it: the name, when the
// registry declares it for this engagement, and unknownTool otherwise. The
// model chooses the name it calls, so the raw string is model output.
func toolLabel(name string, params Params) string {
	for _, t := range tooldefs.Sandbox() {
		if t.Name == name {
			return name
		}
	}
	for _, t := range tooldefs.TFTools(tfToolFacts(params.HasBlueprint)) {
		if t.Name == name {
			return name
		}
	}
	return unknownTool
}

// toolTrace is what one tool call's dispatch learned for its span.
type toolTrace struct {
	outcome string
	reason  string
	kills   int
}

// end closes a tool call's span. A tool that ran and failed is the model's
// business and no error; a dead tool host is. err is the dispatch's own: a
// cancellation, or a result that could not be recorded.
func (t *toolTrace) end(ctx context.Context, span trace.Span, err error) {
	outcome := t.outcome
	switch {
	case err != nil && ctx.Err() != nil:
		outcome = toolCancelled
	case outcome == toolHostLost:
		span.SetStatus(codes.Error, "tool host lost")
	case err != nil:
		span.SetStatus(codes.Error, "recording the tool result failed")
	}
	if outcome != "" {
		span.SetAttributes(telemetry.Outcome(outcome))
	}
	if t.reason != "" {
		span.SetAttributes(telemetry.Reason(t.reason))
	}
	if t.kills > 0 {
		span.SetAttributes(telemetry.MemoryKills(t.kills))
	}
	span.End()
}
