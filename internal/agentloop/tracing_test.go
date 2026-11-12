package agentloop

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

// One provider for the whole test binary, and a cursor per test: a
// package-level tracer is created at init and the OTel global wires it to the
// FIRST provider installed in the process, never re-wiring.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func oneSpanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	found := spansNamed(spans, name)
	if len(found) != 1 {
		t.Fatalf("recorded %d %s spans, want 1", len(found), name)
	}
	return found[0]
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func wantAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string, want attribute.Value) {
	t.Helper()
	got, ok := spanAttr(s, key)
	if !ok || got != want {
		t.Errorf("%s: %s = %v (present %v), want %v", s.Name(), key, got.String(), ok, want.String())
	}
}

func childrenOf(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name && s.Parent().SpanID() == parent.SpanContext().SpanID() {
			out = append(out, s)
		}
	}
	return out
}

// engagementSpan stands in for the engagement's root, which the delegate
// layer ends at agent-live before the loop starts.
func engagementSpan() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0e, 0x0e},
		SpanID:     trace.SpanID{0x0e},
		TraceFlags: trace.FlagsSampled,
	})
}

func tracedParams() Params {
	p := testParams()
	p.EngagementSpan = engagementSpan()
	return p
}

// TestEngagementIsOneRootPerIteration pins the trace shape: the preparation
// and every iteration are roots of their own, each in its own trace, linked
// to the engagement and carrying conversation.id, with the provider call's
// ordinal and the loop's reading of its stop reason on the iteration that
// made it, and each tool call a child of the iteration that dispatched it.
func TestEngagementIsOneRootPerIteration(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"command": "ls /secret"}}}},
		{text: "done"},
	}}
	got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), tracedParams())
	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	spans := ended()
	prepare := oneSpanNamed(t, spans, "agent.prepare")
	wantAttr(t, prepare, "outcome", attribute.StringValue(prepareOK))
	turns := spansNamed(spans, "agent.turn")
	if len(turns) != 2 {
		t.Fatalf("recorded %d agent.turn spans, want 2", len(turns))
	}

	traces := map[trace.TraceID]bool{}
	for _, s := range append([]sdktrace.ReadOnlySpan{prepare}, turns...) {
		if s.Parent().IsValid() {
			t.Errorf("%s has a parent; every loop span is a root", s.Name())
		}
		if len(s.Links()) != 1 || s.Links()[0].SpanContext.SpanID() != engagementSpan().SpanID() {
			t.Errorf("%s links %v, want the engagement's root", s.Name(), s.Links())
		}
		wantAttr(t, s, "conversation.id", attribute.StringValue("conv"))
		traces[s.SpanContext().TraceID()] = true
	}
	if len(traces) != 3 {
		t.Errorf("the loop's roots share traces (%d distinct of 3)", len(traces))
	}

	wantAttr(t, turns[0], "turn", attribute.IntValue(1))
	wantAttr(t, turns[0], "stop_reason", attribute.StringValue("tool_calls"))
	wantAttr(t, turns[0], "outcome", attribute.StringValue(turnToolCalls))
	wantAttr(t, turns[1], "turn", attribute.IntValue(2))
	wantAttr(t, turns[1], "stop_reason", attribute.StringValue("end_turn"))
	wantAttr(t, turns[1], "outcome", attribute.StringValue("concluded"))

	tools := childrenOf(spans, turns[0], "agent.tool")
	if len(tools) != 1 {
		t.Fatalf("first turn has %d agent.tool children, want 1", len(tools))
	}
	wantAttr(t, tools[0], "tool", attribute.StringValue("bash"))
	wantAttr(t, tools[0], "outcome", attribute.StringValue(toolOK))
	for _, kv := range tools[0].Attributes() {
		if strings.Contains(kv.Value.String(), "secret") {
			t.Errorf("agent.tool carries the call's arguments in %s", kv.Key)
		}
	}
}

// TestFailedTurnCarriesNoErrorText pins that the one error status the loop
// sets says only that the engagement failed: a provider's rendered error can
// quote the request.
func TestFailedTurnCarriesNoErrorText(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: errors.New("inference: provider error: messages.0: the secret prompt (HTTP 400)")},
	}}
	got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), tracedParams())
	if got.Kind != ResultFailed {
		t.Fatalf("disposition = %v, want failed", got.Kind)
	}

	turn := oneSpanNamed(t, ended(), "agent.turn")
	wantAttr(t, turn, "outcome", attribute.StringValue(outcomeFailed))
	if turn.Status().Code != codes.Error || strings.Contains(turn.Status().Description, "secret") {
		t.Errorf("status = %+v, want an error with a fixed description", turn.Status())
	}
	if len(turn.Events()) != 0 {
		t.Errorf("a failed turn records no error event: %v", turn.Events())
	}
}

// TestParkedTurnNamesWhichStop pins that a park is an outcome with a closed
// reason, never an error: a guard's notice is prose for a person.
func TestParkedTurnNamesWhichStop(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	e := newTestEngine(tr, &scriptedProvider{}, newScriptedToolHost())
	e.Guards = []Guard{guardFunc(func(context.Context, int) (string, error) {
		return "This run has spent its budget for the acme-secret team.", nil
	})}
	if got := e.Run(context.Background(), tracedParams()); got.Kind != ResultParked {
		t.Fatalf("disposition = %v, want parked", got.Kind)
	}

	turn := oneSpanNamed(t, ended(), "agent.turn")
	wantAttr(t, turn, "outcome", attribute.StringValue("parked"))
	wantAttr(t, turn, "reason", attribute.StringValue(parkGuard))
	if turn.Status().Code == codes.Error {
		t.Errorf("a park must not carry an error status: %+v", turn.Status())
	}
}

// TestToolSpanNamesOnlyRegisteredTools pins that the tool attribute is a
// closed set: the model chooses the name it calls, so a name the registry does
// not declare is recorded as unknown, and a tool that ran and failed is the
// model's business rather than an error.
func TestToolSpanNamesOnlyRegisteredTools(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	host.answers["grep"] = ToolOutcome{ToolError: "grep: /work/secret: no such file"}
	host.answers["/home/acme/run.sh"] = ToolOutcome{Protocol: &ProtocolError{Kind: protoUnknownTool}}
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "/home/acme/run.sh"}, {ID: "c2", Name: "grep"}}},
		{text: "done"},
	}}
	if got := newTestEngine(tr, p, host).Run(context.Background(), tracedParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	tools := spansNamed(ended(), "agent.tool")
	if len(tools) != 2 {
		t.Fatalf("recorded %d agent.tool spans, want 2", len(tools))
	}
	wantAttr(t, tools[0], "tool", attribute.StringValue(unknownTool))
	wantAttr(t, tools[0], "outcome", attribute.StringValue(toolProtocolError))
	wantAttr(t, tools[1], "tool", attribute.StringValue("grep"))
	wantAttr(t, tools[1], "outcome", attribute.StringValue(toolErrored))
	for _, s := range tools {
		if s.Status().Code == codes.Error {
			t.Errorf("%s: a call the tool host answered must not carry an error status", s.Name())
		}
	}
}

// TestToolSpanCountsMemoryKills pins that a ceiling kill the memory watch
// attributed to a call is counted on its span.
func TestToolSpanCountsMemoryKills(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	host := newScriptedToolHost()
	host.answers["bash"] = ToolOutcome{Content: "Killed", CeilingKills: 2}
	if got := newTestEngine(tr, oneToolCallThenDone(), host).Run(context.Background(), tracedParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	tool := oneSpanNamed(t, ended(), "agent.tool")
	wantAttr(t, tool, "memory.kills", attribute.IntValue(2))
}

// TestLostToolHostShowsOnItsCall pins how a dead tool host reads: the call it
// died in is an error naming the cause, and the restart is its child.
func TestLostToolHostShowsOnItsCall(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	host := &restartableHost{dies: map[string]bool{"bash": true}}
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{text: "done"},
	}}
	if got := newTestEngine(tr, p, host).Run(context.Background(), tracedParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	spans := ended()
	tool := oneSpanNamed(t, spans, "agent.tool")
	wantAttr(t, tool, "outcome", attribute.StringValue(toolHostLost))
	wantAttr(t, tool, "reason", attribute.StringValue(lostStopped))
	if tool.Status().Code != codes.Error {
		t.Errorf("a lost tool host is an error: %+v", tool.Status())
	}
	restarts := childrenOf(spans, tool, "agent.tool_host.restart")
	if len(restarts) != 1 {
		t.Fatalf("lost call has %d restart children, want 1", len(restarts))
	}
	wantAttr(t, restarts[0], "outcome", attribute.StringValue(restartDone))
	wantAttr(t, restarts[0], "count", attribute.IntValue(1))
}

// TestToolHostLostCauses pins the closed cause for each way a tool host is
// lost.
func TestToolHostLostCauses(t *testing.T) {
	cases := []struct {
		name string
		err  *ToolHostLostError
		want string
	}{
		{"memory ceiling", &ToolHostLostError{MemoryLimit: true}, lostMemoryCeiling},
		{"overran", ToolCallOverranError(30*time.Minute, errors.New("eof")), lostOverran},
		{"refused", toolHostLostFor(nil, &ProtocolError{Kind: protoRequestTooLarge}), lostRefused},
		{"unexplained", toolHostLostFor(errors.New("eof"), nil), lostStopped},
	}
	for _, c := range cases {
		if got := c.err.cause(); got != c.want {
			t.Errorf("%s: cause = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRetryBackoffIsSpanned pins that a provider retry's wait is a span under
// the iteration, the same shape as the other upstream clients' waits.
func TestRetryBackoffIsSpanned(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: errors.New("inference: provider error: slow down (HTTP 429)")},
		{text: "recovered"},
	}}
	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), tracedParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	spans := ended()
	turn := oneSpanNamed(t, spans, "agent.turn")
	backoffs := childrenOf(spans, turn, "llm.retry.backoff")
	if len(backoffs) != 1 {
		t.Fatalf("turn has %d backoff children, want 1", len(backoffs))
	}
	wantAttr(t, backoffs[0], "attempt", attribute.IntValue(1))
	wantAttr(t, backoffs[0], "disposition", attribute.StringValue("rate_limited"))
	wantAttr(t, backoffs[0], "provider", attribute.StringValue(string(inference.ProviderAnthropic)))
}

// TestCompactionSpanNamesItsReasonAndArm pins the reactive arm: the iteration
// whose call overflowed compacts under an agent.compact child and hands on.
func TestCompactionSpanNamesItsReasonAndArm(t *testing.T) {
	ended := recordSpans(t)
	old := time.Now().UTC().Add(-time.Hour)
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
		usedAssistant("prior work", 1000, old),
	)
	overflow := fmt.Errorf("%w: prompt is too long: 201000 tokens > 200000 maximum", inference.ErrContextOverflow)
	p := &scriptedProvider{turns: []scriptedTurn{
		{err: overflow},
		forcedReply("a", "s"),
		{text: "done"},
	}}
	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), tracedParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	spans := ended()
	turns := spansNamed(spans, "agent.turn")
	if len(turns) != 2 {
		t.Fatalf("recorded %d agent.turn spans, want 2", len(turns))
	}
	wantAttr(t, turns[0], "outcome", attribute.StringValue(turnCompacted))
	compactions := childrenOf(spans, turns[0], "agent.compact")
	if len(compactions) != 1 {
		t.Fatalf("overflowed turn has %d agent.compact children, want 1", len(compactions))
	}
	wantAttr(t, compactions[0], "reason", attribute.StringValue(compactOverflow))
	wantAttr(t, compactions[0], "outcome", attribute.StringValue(compactedCold))
}

// TestStopBlueprintNamesTheTurn pins that a flow-control stop says so on the
// iteration it ended.
func TestStopBlueprintNamesTheTurn(t *testing.T) {
	ended := recordSpans(t)
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: ToolStopBlueprint, Input: map[string]any{
			"type": "abort", "reason": "nothing to do here", "summary": "looked and found nothing to change",
		}}}},
	}}
	got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), tracedParams())
	if got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	spans := ended()
	turn := oneSpanNamed(t, spans, "agent.turn")
	wantAttr(t, turn, "outcome", attribute.StringValue(turnStopped))
	tool := oneSpanNamed(t, spans, "agent.tool")
	wantAttr(t, tool, "tool", attribute.StringValue(ToolStopBlueprint))
}
