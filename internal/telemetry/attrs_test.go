package telemetry

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// approvedKeys is the vocabulary TF is allowed to emit from its own
// instrumentation. Not a typo check: every key here was a decision that a
// repo name, title, username, or file path is NOT what it carries, and a
// new helper appearing without a line here fails the test — which is the
// moment to make that decision, rather than once a customer's repository
// inventory is already in someone's trace store. Keep sorted.
var approvedKeys = []string{
	"attempt",
	"claim.attempt",
	"conversation.id",
	"count",
	"disposition",
	"entity.id",
	"event.id",
	"event.type",
	"job",
	"org.id",
	"outcome",
	"provider",
	"runtime",
	"source",
	"task.id",
	"team.id",
	"transport",
}

// TestAttributeHelpersEmitOnlyApprovedKeys calls every exported helper in
// attrs.go and checks the key it produces is on the approved list.
func TestAttributeHelpersEmitOnlyApprovedKeys(t *testing.T) {
	produced := []attribute.KeyValue{
		OrgID("o"), TeamID("t"), EventID("e"), EventType("github:pr:opened"),
		EntityID("en"), TaskID("ta"), ConversationID("c"), ClaimAttempt(1),
		Source("github"), Disposition("routed"), Outcome("ok"), Runtime("sdk"),
		Attempt(2), Count(3), Job("scorer"), Provider("anthropic"),
		Transport("direct"),
	}
	for _, kv := range produced {
		if !slices.Contains(approvedKeys, string(kv.Key)) {
			t.Errorf("attribute key %q is not in approvedKeys — if this key is genuinely free of tenant data, add it to the list; otherwise the helper should not exist", kv.Key)
		}
	}
}

// TestEveryAttributeHelperIsCovered parses attrs.go and fails on an
// exported function the test above doesn't call — without it a new helper
// could carry a repo name to a trace backend and the gate would still pass,
// because nothing would exercise it.
func TestEveryAttributeHelperIsCovered(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "attrs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse attrs.go: %v", err)
	}

	var declared []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() {
			continue
		}
		declared = append(declared, fn.Name.Name)
	}

	// Mirrors the calls above by name.
	exercised := map[string]bool{
		"OrgID": true, "TeamID": true, "EventID": true, "EventType": true,
		"EntityID": true, "TaskID": true, "ConversationID": true,
		"ClaimAttempt": true, "Source": true, "Disposition": true,
		"Outcome": true, "Runtime": true, "Attempt": true, "Count": true,
		"Job": true, "Provider": true, "Transport": true,
	}
	sort.Strings(declared)
	for _, name := range declared {
		if !exercised[name] {
			t.Errorf("attrs.go declares %s but TestAttributeHelpersEmitOnlyApprovedKeys does not call it — a new attribute helper needs a deliberate decision that its value carries no tenant data", name)
		}
	}
}

// TestScrubbedTracerProviderDropsURLAttributes covers the other half of
// the hygiene rule: attributes TF does not set itself. otelhttp records
// the full outbound URL, and TF's carry repo names and issue keys — so the
// scrubber must remove those keys and leave everything else alone.
func TestScrubbedTracerProviderDropsURLAttributes(t *testing.T) {
	restoreTraceGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	// Installed on the global rather than handed over directly, so the
	// per-Tracer-call resolution that lets a client be built before Init is
	// part of what this covers.
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	_, span := ScrubbedTracerProvider().Tracer("test").Start(context.Background(), "outbound")
	span.SetAttributes(
		attribute.String("url.full", "https://api.github.com/repos/acme/secret-project/pulls/18"),
		attribute.String("url.path", "/api/repos/acme/secret-project"),
		attribute.String("url.query", "state=open&head=acme:private-branch"),
		attribute.String("server.address", "api.github.com"),
		attribute.String("http.request.method", "GET"),
	)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	var got []string
	for _, kv := range ended[0].Attributes() {
		got = append(got, string(kv.Key))
	}
	for _, banned := range []string{"url.full", "url.path", "url.query"} {
		if slices.Contains(got, banned) {
			t.Errorf("%s survived scrubbing; outbound URLs carry repo names and issue keys (attributes: %s)", banned, strings.Join(got, ", "))
		}
	}
	for _, kept := range []string{"server.address", "http.request.method"} {
		if !slices.Contains(got, kept) {
			t.Errorf("%s was dropped; the scrubber must remove only the URL keys (attributes: %s)", kept, strings.Join(got, ", "))
		}
	}
}

// TestScrubbedTracerProviderDropsStartOptionAttributes covers the half the
// SetAttributes override cannot reach. otelhttp's SERVER handler passes its
// attributes — url.path among them — as a span-start option rather than
// setting them afterwards, so without start-option filtering every server
// span would carry the concrete request path.
func TestScrubbedTracerProviderDropsStartOptionAttributes(t *testing.T) {
	restoreTraceGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	_, span := ScrubbedTracerProvider().Tracer("test").Start(context.Background(), "inbound",
		trace.WithAttributes(
			attribute.String("url.path", "/api/repos/acme/secret-project"),
			attribute.String("http.route", "PATCH /api/repos/{owner}/{repo}"),
		))
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	var got []string
	for _, kv := range ended[0].Attributes() {
		got = append(got, string(kv.Key))
	}
	if slices.Contains(got, "url.path") {
		t.Errorf("url.path survived a start option; TF has repo-keyed routes, so a server span's concrete path is tenant data (attributes: %s)", strings.Join(got, ", "))
	}
	// A stand-in for "everything that isn't a concrete URL": the scrubber
	// must be a denylist, not a filter that keeps only what it recognizes.
	if !slices.Contains(got, "http.route") {
		t.Errorf("a non-URL attribute was dropped; the scrubber removes only the URL keys (attributes: %s)", strings.Join(got, ", "))
	}
}

// TestScrubbedTracerProviderPreservesNonAttributeStartOptions guards the
// rebuild in scrubStartOptions. Filtering attributes means reconstructing
// the option list, and a field missed there fails silently — dropping
// WithNewRoot in particular would quietly start honoring inbound
// traceparent as a parent, reversing the public-endpoint posture.
func TestScrubbedTracerProviderPreservesNonAttributeStartOptions(t *testing.T) {
	restoreTraceGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01},
		SpanID:     trace.SpanID{0x02},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	// A parent in the ctx AND WithNewRoot, which is exactly the shape
	// otelhttp builds for a public endpoint: ignore the inbound parent,
	// keep it as a link.
	parentCtx := trace.ContextWithSpanContext(context.Background(), remote)

	_, span := ScrubbedTracerProvider().Tracer("test").Start(parentCtx, "inbound",
		trace.WithAttributes(attribute.String("url.path", "/api/repos/acme/thing")),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithNewRoot(),
		trace.WithLinks(trace.Link{SpanContext: remote}),
	)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	got := ended[0]
	if got.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want Server", got.SpanKind())
	}
	if len(got.Links()) != 1 {
		t.Errorf("links = %d, want 1 — the inbound context must survive as a link", len(got.Links()))
	}
	if got.Parent().TraceID() == remote.TraceID() {
		t.Error("the remote parent was honored; WithNewRoot was lost in the rebuild, which silently reverses the public-endpoint posture")
	}
}
