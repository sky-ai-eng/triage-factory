package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// scrubbedAttrs are the keys stripped from every span created through
// ScrubbedTracerProvider.
//
// otelhttp's client transport records the complete request URL, and TF's
// outbound URLs put tenant data in the path: /repos/{owner}/{repo}/pulls/{n},
// a Jira issue key, an artifact filename. Exporting those hands every
// tenant's repository inventory to whatever trace backend the operator
// runs — no less an egress for having been written by a library.
//
// What survives says everything the URL did minus the names: server.address
// is the upstream, the span name is the client, method and status describe
// the exchange, and the parent span says what the call was for.
var scrubbedAttrs = map[attribute.Key]struct{}{
	"url.full":  {},
	"url.query": {},
}

// ScrubbedTracerProvider delegates to the process global but drops the
// keys above. It exists for instrumentation TF does not write and cannot
// configure — otelhttp's transport offers no hook for filtering the
// attributes it sets. TF's own spans don't need it: they go through
// attrs.go, which can only express keys that were argued for.
//
// The global is resolved per Tracer call rather than captured, so a client
// constructed before Init still records once Init installs the real one.
func ScrubbedTracerProvider() trace.TracerProvider { return scrubbedProvider{} }

type scrubbedProvider struct{ embedded.TracerProvider }

func (scrubbedProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return scrubbedTracer{Tracer: otel.GetTracerProvider().Tracer(name, opts...)}
}

// scrubbedTracer and scrubbedSpan embed the interface they implement, so
// unoverridden methods forward untouched and an addition to the OTel API
// can't break the build here.
type scrubbedTracer struct{ trace.Tracer }

func (t scrubbedTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	// The ctx carries the inner span, not the wrapper — children must
	// parent correctly, and the wrapper's only job is intercepting
	// SetAttributes on this one span.
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	return ctx, scrubbedSpan{Span: span}
}

type scrubbedSpan struct{ trace.Span }

func (s scrubbedSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.Span.SetAttributes(scrubAttrs(kv)...)
}

// scrubAttrs removes the scrubbed keys, allocating only when there is
// something to remove.
func scrubAttrs(kv []attribute.KeyValue) []attribute.KeyValue {
	drop := false
	for _, a := range kv {
		if _, bad := scrubbedAttrs[a.Key]; bad {
			drop = true
			break
		}
	}
	if !drop {
		return kv
	}
	kept := make([]attribute.KeyValue, 0, len(kv))
	for _, a := range kv {
		if _, bad := scrubbedAttrs[a.Key]; !bad {
			kept = append(kept, a)
		}
	}
	return kept
}
