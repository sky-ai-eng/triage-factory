package delegate

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer owns this package's spans. Conventions in internal/telemetry.
var tracer = otel.Tracer("internal/delegate")

// Engagement outcomes — the closed set the root span ends with. They are
// not statuses: an engagement that reached the agent and one that was
// handed back before it started are both ordinary, and only the last is a
// span error.
const (
	// engagementLive is the success terminal: the agent is up and speaking.
	engagementLive = "agent_live"
	// engagementNotStarted is the catch-all for a claim that returned before
	// the agent ran without one of the named dispositions below — in
	// practice a shutdown mid-setup, which leaves the run for the boot
	// reconcile.
	engagementNotStarted = "not_started"
	// engagementCancelled — a cancel raced the claim, so the step was parked
	// rather than run. Anticipated, never an error.
	engagementCancelled = "cancelled"
	// engagementNoMessage — a follow-up claim on a finished blueprint that
	// carried nothing to deliver, parked back.
	engagementNoMessage = "no_message"
	// engagementFenced — the claim was released mid-setup and a successor
	// owns the conversation.
	engagementFenced = "fenced"
	// engagementSetupFailed — setup genuinely failed. The only outcome that
	// also carries an error status.
	engagementSetupFailed = "setup_failed"
)

// engagement is one claim attempt's root span, plus the SpanContext that
// outlives it.
//
// The span ENDS at agent-live (standing decision 5): a span exports only
// when it ends, so holding one open across the streaming period — parks,
// hibernation, a permission prompt waiting on a human — would buffer it in
// memory and lose it entirely on a crash, which is precisely the run whose
// setup we most want to see. What follows agent-live is covered by punctual
// spans instead, and sc is what they link to. It is a value copy taken
// before the end, because a SpanContext is immutable identity (trace + span
// id + flags) rather than a handle into the ended span.
type engagement struct {
	span trace.Span
	sc   trace.SpanContext
	once sync.Once
}

// finish ends the root exactly once. Every caller races every other — the
// agent going live, a setup failure, the dispatcher's deferred cleanup —
// and first writer wins, so agent-live is never overwritten by the deferred
// not_started that follows it a few frames later.
func (e *engagement) finish(outcome string, err error) {
	if e == nil {
		return
	}
	e.once.Do(func() {
		e.span.SetAttributes(telemetry.Outcome(outcome))
		if err != nil {
			e.span.RecordError(err)
			e.span.SetStatus(codes.Error, err.Error())
		}
		e.span.End()
	})
}

// beginEngagement opens the root span for one claim attempt and registers it
// under the conversation id, returning the setup context and the cleanup the
// dispatcher defers.
//
// A ROOT (WithNewRoot), like the router's route.event and for the same
// reason: the caller's context is the dispatcher's drain loop, which claims
// many runs across many cycles, so descending from it would grow one
// unbounded trace per process lifetime. Every join to the work that caused
// this engagement is an attribute — conversation.id most of all, which is
// what makes a requeued run's up-to-five attempts findable together while
// claim.attempt tells them apart.
//
// runtime is on the root rather than only on the loop's own spans because
// everything this span covers is UPSTREAM of the runtime branch: a native
// run's setup is traced here in full even though the loop it hands off to
// is deliberately out of scope.
func (s *Spawner) beginEngagement(ctx context.Context, run *domain.Conversation) (context.Context, func()) {
	attrs := []attribute.KeyValue{
		telemetry.ConversationID(run.ID),
		telemetry.OrgID(run.OrgID),
		telemetry.ClaimAttempt(run.Attempts),
	}
	if run.TaskID != "" {
		attrs = append(attrs, telemetry.TaskID(run.TaskID))
	}
	if run.Runtime != "" {
		attrs = append(attrs, telemetry.Runtime(run.Runtime))
	}
	ctx, span := tracer.Start(ctx, "engagement.setup",
		trace.WithNewRoot(), trace.WithAttributes(attrs...))

	e := &engagement{span: span, sc: span.SpanContext()}
	s.mu.Lock()
	s.engagements[run.ID] = e
	s.mu.Unlock()

	return ctx, func() {
		// A no-op once the agent went live, which is the ordinary path: the
		// root ended back at the phase-clear and this only deregisters.
		e.finish(engagementNotStarted, nil)
		s.mu.Lock()
		// Identity-checked: a re-claim of the same conversation may already
		// have registered its own engagement by the time this teardown runs,
		// and deleting by key alone would take the successor's link target
		// with it.
		if s.engagements[run.ID] == e {
			delete(s.engagements, run.ID)
		}
		s.mu.Unlock()
	}
}

// endEngagement closes runID's root with an explicit outcome. Safe for a
// conversation with no engagement registered (every non-dispatch caller of
// updatePhase — a resume turn, an unwired fixture) and safe to call twice.
func (s *Spawner) endEngagement(runID, outcome string) {
	s.engagementFor(runID).finish(outcome, nil)
}

// failEngagement closes runID's root on a setup failure, with the cause on
// the span. The one exit that gets an error status — everything else that
// ends an engagement early is a disposition, and "traces with errors" stays
// a usable filter only if it means something went wrong.
func (s *Spawner) failEngagement(runID string, err error) {
	s.engagementFor(runID).finish(engagementSetupFailed, err)
}

// engagementFor resolves runID's live engagement, or nil. Nil-safe
// downstream, so callers never branch on it.
func (s *Spawner) engagementFor(runID string) *engagement {
	if s == nil || runID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engagements[runID]
}

// engagementSpanContext is runID's retained root SpanContext, for the
// out-of-package consumers that capture it once (the per-run RelayServer).
// The zero SpanContext when there is no engagement, which every link site
// treats as "no link".
func (s *Spawner) engagementSpanContext(runID string) trace.SpanContext {
	e := s.engagementFor(runID)
	if e == nil {
		return trace.SpanContext{}
	}
	return e.sc
}

// startPunctual opens one of the spans that cover the period AFTER agent-live
// — a permission round-trip, a relayed op, a workspace snapshot, the terminal
// — as a fresh root LINKED to the engagement it belongs to.
//
// Linked rather than parented, because the parent has already ended and the
// streaming period it covers is unbounded: a conversation parked overnight
// and resumed would otherwise accrete spans into one trace for as long as it
// lives. A link says "this came from that engagement" and leaves each side
// independently exportable, which is what makes a mid-stream crash cost the
// punctual span and nothing else.
//
// Note the sampling caveat from the epic: links do not share the parent's
// sampling decision, so nothing here may assume both sides survive. Every
// span this opens carries conversation.id itself for exactly that reason.
func (s *Spawner) startPunctual(ctx context.Context, runID, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return startPunctualLinked(ctx, s.engagementSpanContext(runID), runID, name, attrs...)
}

// startPunctualLinked is startPunctual for a caller that captured the
// engagement's SpanContext once rather than looking it up per call — the
// per-run RelayServer, whose whole point is that it already knows its run.
func startPunctualLinked(ctx context.Context, sc trace.SpanContext, runID, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{trace.WithNewRoot()}
	if runID != "" {
		attrs = append(attrs, telemetry.ConversationID(runID))
	}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	if sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	return tracer.Start(ctx, name, opts...)
}

// recordSpanError puts err on span with an error status, and does nothing
// for a nil error. The shape repeats at every instrumented exit in this
// package; a helper keeps "an error status means something is wrong" from
// depending on remembering both calls.
func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
