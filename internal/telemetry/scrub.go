package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// scrubbedAttrs are the attribute keys stripped from every span created
// through ScrubbedTracerProvider.
//
// url.full is on this list because of what TF's outbound URLs contain.
// otelhttp's client transport records the complete request URL, and the
// APIs TF calls put tenant data in the path: a GitHub REST call is
// /repos/{owner}/{repo}/pulls/{n}, a Jira call carries the issue key, an
// artifact download carries the file name. Exporting those would hand the
// repository inventory of every tenant to whatever trace backend the
// operator runs — the exact egress the attribute-hygiene rule in this
// package's doc exists to prevent, and it is no less an egress for having
// been written by a library rather than by TF.
//
// What survives says everything the URL did except the tenant's names:
// server.address identifies which upstream was called, the span name
// identifies which client called it, http.request.method and
// http.response.status_code describe the exchange, and the parent span
// says what the call was for.
var scrubbedAttrs = map[attribute.Key]struct{}{
	"url.full":  {},
	"url.query": {},
}

// ScrubbedTracerProvider returns a TracerProvider that delegates to the
// process global but drops the attributes above from any span started
// through it. It exists for instrumentation TF does not write and cannot
// configure — otelhttp's transport in particular, which offers no hook for
// filtering the attributes it sets.
//
// Use it for outbound HTTP clients. TF's own spans do not need it: they set
// attributes through the helpers in attrs.go, which can only express keys
// that were argued for.
//
// The global is resolved per Tracer call rather than captured, so a
// provider constructed before Init still records once Init installs the
// real one.
func ScrubbedTracerProvider() trace.TracerProvider { return scrubbedProvider{} }

type scrubbedProvider struct{ embedded.TracerProvider }

func (scrubbedProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return scrubbedTracer{Tracer: otel.GetTracerProvider().Tracer(name, opts...)}
}

// scrubbedTracer and scrubbedSpan embed the interface they implement, so
// every method they do not override forwards untouched and a future
// addition to the OTel API cannot break the build here. The one override
// each is the whole implementation.
type scrubbedTracer struct{ trace.Tracer }

func (t scrubbedTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	// The returned ctx carries the inner span, not the wrapper: a child
	// started from it must parent correctly, and a wrapper in the context
	// would only add a layer for every descendant to unwrap. The wrapper's
	// job is narrow — intercept SetAttributes on this one span, which is
	// the only way otelhttp records anything.
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	return ctx, scrubbedSpan{Span: span}
}

type scrubbedSpan struct{ trace.Span }

func (s scrubbedSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.Span.SetAttributes(scrubAttrs(kv)...)
}

// scrubAttrs removes the scrubbed keys, allocating only when there is
// something to remove — which is most of the time nothing, since a span
// carries one url.full among several attributes and TF's own spans carry
// none at all.
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
