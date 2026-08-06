package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// scrubbedAttrs are the keys stripped from every span created through
// ScrubbedTracerProvider: the ones carrying a concrete URL.
//
// Both directions leak, under different keys. Outbound, otelhttp records
// url.full, and TF's outbound URLs put tenant data in the path —
// /repos/{owner}/{repo}/pulls/{n}, a Jira issue key, an artifact filename.
// Inbound, it records url.path, and TF's own API has routes keyed by
// repository rather than by id (PATCH /api/repos/{owner}/{repo} and its
// /branches sibling), so a server span carries the customer's repo name
// just as plainly. Either way the export hands a tenant's repository
// inventory to whatever backend the operator runs, and it is no less an
// egress for having been written by a library rather than by TF.
//
// What survives says nearly everything those did, minus the names: the
// span name is the matched route, uninterpolated (PATCH
// /api/repos/{owner}/{repo}) on the server side and the client's identity
// outbound; server.address is the host, and method and status describe the
// exchange. The one real loss is diagnosing an unrecognized path from a
// trace alone — accepted, because the alternative is exporting every path
// that ever reaches TF.
var scrubbedAttrs = map[attribute.Key]struct{}{
	"url.full":  {},
	"url.path":  {},
	"url.query": {},
}

// ScrubbedTracerProvider delegates to the process global but drops the
// keys above. It exists for instrumentation TF does not write and cannot
// configure — otelhttp offers no hook for filtering the attributes it sets,
// on either the server handler or the client transport, so both are built
// against this. TF's own spans don't need it: they go through attrs.go,
// which can only express keys that were argued for.
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
	// parent correctly, and the wrapper only intercepts this one span.
	ctx, span := t.Tracer.Start(ctx, name, scrubStartOptions(opts)...)
	return ctx, scrubbedSpan{Span: span}
}

// scrubStartOptions filters attributes handed to Start, which has to be
// separate from the SetAttributes path because the two otelhttp halves
// differ: the client transport calls SetAttributes after starting, but the
// SERVER handler passes its attributes — url.path among them — as a start
// option, before the span exists. A SetAttributes-only wrapper would let
// every server span's concrete path through.
//
// Rebuilt from the parsed config rather than appended to, since an
// attribute already in the option list can't be removed by adding another.
// The rebuild restates every field a SpanStartOption can carry;
// WithStackTrace is absent because it is end/event-only and unreachable
// here. Getting WithNewRoot or WithLinks wrong would be silent and serious:
// they are how otelhttp implements the public-endpoint posture, so dropping
// them would quietly start honoring inbound traceparent as a parent.
func scrubStartOptions(opts []trace.SpanStartOption) []trace.SpanStartOption {
	cfg := trace.NewSpanStartConfig(opts...)
	attrs := cfg.Attributes()
	kept := scrubAttrs(attrs)
	if len(kept) == len(attrs) {
		return opts
	}

	rebuilt := []trace.SpanStartOption{
		trace.WithAttributes(kept...),
		trace.WithSpanKind(cfg.SpanKind()),
	}
	if links := cfg.Links(); len(links) > 0 {
		rebuilt = append(rebuilt, trace.WithLinks(links...))
	}
	if cfg.NewRoot() {
		rebuilt = append(rebuilt, trace.WithNewRoot())
	}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		rebuilt = append(rebuilt, trace.WithTimestamp(ts))
	}
	return rebuilt
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
