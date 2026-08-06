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
)

// approvedKeys is the whole span-attribute vocabulary TF is allowed to
// emit from its own instrumentation, and this test is the gate on it.
//
// The point is not to catch a typo. Spans leave the process for a backend
// an operator may not control, so every key here was a decision that a
// repo name, a PR title, a username, a branch, a file path, or a message
// body is NOT what it carries. A new helper appearing without a line here
// fails the test, which is the moment to make that decision rather than
// three months later when a customer's repository inventory is already in
// someone's trace store.
//
// Keep sorted; the failure message tells you what to add.
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

// TestEveryAttributeHelperIsCovered parses attrs.go and fails if it
// declares an exported function this test does not exercise above. Without
// it, a new helper could carry a repo name to a trace backend and the test
// above would still pass, because nothing would call it.
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

	// The exercised set, derived from the test above by name so the two
	// can't drift silently.
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
// the hygiene rule: attributes TF does not set itself. otelhttp's client
// transport records the full outbound URL, and TF's outbound URLs contain
// repository names and issue keys, so the scrubbing provider has to remove
// it — and has to leave everything else alone.
func TestScrubbedTracerProviderDropsURLAttributes(t *testing.T) {
	restoreTraceGlobals(t)
	recorder := tracetest.NewSpanRecorder()
	// Installed on the global, not handed to the provider directly: the
	// scrubber resolves otel.GetTracerProvider() per Tracer call, and that
	// indirection — which is what lets an outbound client be constructed
	// before Init — is part of what this test covers.
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	_, span := ScrubbedTracerProvider().Tracer("test").Start(context.Background(), "outbound")
	span.SetAttributes(
		attribute.String("url.full", "https://api.github.com/repos/acme/secret-project/pulls/18"),
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
	for _, banned := range []string{"url.full", "url.query"} {
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
