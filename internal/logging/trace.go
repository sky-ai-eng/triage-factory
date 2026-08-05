package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler stamps trace_id and span_id onto every record emitted under
// an active span. This is the standard logs↔traces join, and the reason no
// domain table in TF carries a trace-ID column: a trace's retention is days
// and a row's is forever, so the durable side of that join is the span's
// domain-ID attributes, and the ephemeral side is this.
//
// It reads the record's context rather than any ambient state, so it is
// exactly as correct as the caller's ctx threading — and no more. slog only
// carries a context on the *Context call variants (InfoContext, ErrorContext,
// Log), so a bare log.Info under a span is stamped with nothing. That is a
// deliberate non-fix: inventing a goroutine-local current span to paper
// over it would make the attribute a guess, and a wrong trace_id is worse
// than an absent one.
//
// Like procHandler, injection happens at Handle time rather than at
// construction, which is what lets a package-level
// `var xLog = logging.Component("x")` — built during package init, long
// before any span exists — pick up per-record span context.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	// Valid, not recording/sampled: a span the SDK sampled out still has a
	// real trace ID, and grouping one request's log lines by it is useful
	// whether or not a trace was ultimately exported. With no provider
	// installed the span context is the zero value, so IsValid is false and
	// nothing is stamped — the disabled path adds two field reads per
	// record and no attributes.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
