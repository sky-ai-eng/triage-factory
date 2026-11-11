package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// One provider for the whole test binary, and a cursor per test: a
// package-level tracer is created at init and the OTel global wires it to the
// FIRST provider installed in the process, never re-wiring, so per-test
// install and restore would send later tests' spans to the first recorder.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordStreamSpans arms tracing (once) and returns a reader for the spans
// that end after this call.
func recordStreamSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

// streamAgainst runs one Stream against a fake Anthropic endpoint, under a
// parent span, and returns the call's error.
func streamAgainst(t *testing.T, ctx context.Context, handler http.HandlerFunc) error {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   "placeholder",
		BaseURL:  srv.URL,
		Models:   []string{"claude-sonnet-4-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	_, err = client.Stream(ctx, Request{
		Provider:  ProviderAnthropic,
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Rows:      []domain.Message{{Role: "user", Content: "hi"}},
	})
	return err
}

func streamSpan(t *testing.T, spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "llm.stream" {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d llm.stream spans, want 1", len(found))
	}
	return found[0]
}

func attrOf(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// TestStreamSpanRecordsACompletedCall pins what one successful call puts on
// its span: which provider and model it asked for, the cap it was sent with,
// the ledger's four token buckets, the price, and how long the first chunk
// took. It is a client span under the caller's own, which is what nests a
// call inside a native turn or a system job's completion.
func TestStreamSpanRecordsACompletedCall(t *testing.T) {
	ended := recordStreamSpans(t)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "caller")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err := streamAgainst(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range anthropicStreamFixture("claude-sonnet-4-5") {
			fmt.Fprint(w, frame)
			w.(http.Flusher).Flush()
		}
	})
	parent.End()
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	span := streamSpan(t, ended())
	if span.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", span.SpanKind())
	}
	if span.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("llm.stream must be a child of the caller's span")
	}
	if span.Status().Code == codes.Error {
		t.Errorf("a completed call must not carry an error status: %+v", span.Status())
	}
	want := map[string]attribute.Value{
		"provider":          attribute.StringValue("anthropic"),
		"model":             attribute.StringValue("claude-sonnet-4-5"),
		"outcome":           attribute.StringValue("ok"),
		"max_tokens":        attribute.IntValue(4096),
		"tokens.input":      attribute.IntValue(10),
		"tokens.output":     attribute.IntValue(5),
		"tokens.cache_read": attribute.IntValue(0),
	}
	for key, w := range want {
		got, ok := attrOf(span, key)
		if !ok || got != w {
			t.Errorf("%s = %v (present %v), want %v", key, got.String(), ok, w.String())
		}
	}
	for _, key := range []string{"llm.first_chunk_ms", "llm.cost_usd", "tokens.cache_write"} {
		if _, ok := attrOf(span, key); !ok {
			t.Errorf("%s is missing", key)
		}
	}
}

// TestStreamSpanNamesAFailureByItsClassOnly pins the hygiene rule for a
// failed call: the outcome and the status say which class of failure it was,
// and nothing on the span carries the rendered error, which names the
// endpoint and can carry the provider's response body.
func TestStreamSpanNamesAFailureByItsClassOnly(t *testing.T) {
	ended := recordStreamSpans(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var endpoint string
	err := streamAgainst(t, ctx, func(w http.ResponseWriter, r *http.Request) {
		endpoint = r.Host
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down, secret-project"}}`)
	})
	if err == nil {
		t.Fatal("a 429 must fail the call")
	}

	span := streamSpan(t, ended())
	if span.Status().Code != codes.Error || span.Status().Description != "rate_limited" {
		t.Errorf("status = %+v, want an error described as rate_limited", span.Status())
	}
	if got, _ := attrOf(span, "outcome"); got.AsString() != "rate_limited" {
		t.Errorf("outcome = %q, want rate_limited", got.AsString())
	}
	if len(span.Events()) != 0 {
		t.Errorf("a failed call must record no error event (its message is the rendered error): %v", span.Events())
	}
	for _, kv := range span.Attributes() {
		v := kv.Value.String()
		if strings.Contains(v, endpoint) || strings.Contains(v, "secret-project") {
			t.Errorf("attribute %s carries the endpoint or the provider's message: %q", kv.Key, v)
		}
	}
}

// TestStreamSpanCancelledIsNotAnError pins that a call ended by its caller is
// no provider outcome: it says so, with no error status.
func TestStreamSpanCancelledIsNotAnError(t *testing.T) {
	ended := recordStreamSpans(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := streamAgainst(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}); err == nil {
		t.Fatal("a cancelled call must fail")
	}

	span := streamSpan(t, ended())
	if span.Status().Code == codes.Error {
		t.Errorf("a cancelled call must not carry an error status: %+v", span.Status())
	}
	if got, _ := attrOf(span, "outcome"); got.AsString() != "cancelled" {
		t.Errorf("outcome = %q, want cancelled", got.AsString())
	}
}
