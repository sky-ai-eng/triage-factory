package agentloop

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A tool row's duration_ms is how long its call ran, dispatch to result, and
// nil for a call that never ran. These tests drive the engine on a clock the
// test moves by hand, so every measured value is asserted exactly: a tool
// moves the clock by the time it "runs", and anything else that moves it (a
// hook, a restart) must not show up in the row.

// loopClock is a hand-driven clock for Engine.Now.
type loopClock struct {
	mu sync.Mutex
	t  time.Time
}

// newLoopClock starts at the real time, so a fixture whose cache warmth is
// read against the wall clock reads the same as it would without one.
func newLoopClock() *loopClock { return &loopClock{t: time.Now()} }

func (c *loopClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *loopClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// timedToolHost is a scriptedToolHost whose calls take time on the test
// clock: runs[name] passes before the call answers.
type timedToolHost struct {
	*scriptedToolHost
	clock *loopClock
	runs  map[string]time.Duration
}

func (h *timedToolHost) Call(name string, args map[string]any) (ToolOutcome, error) {
	h.clock.advance(h.runs[name])
	return h.scriptedToolHost.Call(name, args)
}

// clockedEngine is newTestEngine reading clock.
func clockedEngine(tr *memTranscript, p Provider, tools ToolHost, clock *loopClock) *Engine {
	e := newTestEngine(tr, p, tools)
	e.Now = clock.now
	return e
}

// toolRow returns the result row answering call id.
func toolRow(t *testing.T, tr *memTranscript, id string) domain.Message {
	t.Helper()
	row := tr.find(func(m domain.Message) bool { return m.Role == "tool" && m.ToolCallID == id })
	if row == nil {
		t.Fatalf("no result row for %s", id)
	}
	return *row
}

func assertMeasured(t *testing.T, row domain.Message, want time.Duration) {
	t.Helper()
	if row.DurationMs == nil {
		t.Fatalf("%s: DurationMs is nil, want %d", row.ToolCallID, want.Milliseconds())
	}
	if got := *row.DurationMs; got != int(want.Milliseconds()) {
		t.Errorf("%s: DurationMs = %d, want %d", row.ToolCallID, got, want.Milliseconds())
	}
}

func assertUnmeasured(t *testing.T, row domain.Message) {
	t.Helper()
	if row.DurationMs != nil {
		t.Errorf("%s: DurationMs = %d, want nil (the call never ran)", row.ToolCallID, *row.DurationMs)
	}
}

// Every outcome of a call the tool host ran and answered is measured: a
// success, a tool error, a survivable protocol error, and an image result.
func TestToolDuration_DispatchedCallsAreMeasured(t *testing.T) {
	clock := newLoopClock()
	scripted := newScriptedToolHost()
	scripted.answers["grep"] = ToolOutcome{ToolError: "no match"}
	scripted.answers["ls"] = ToolOutcome{Protocol: &ProtocolError{Kind: protoUnknownTool, Message: "ls"}}
	scripted.answers["read"] = ToolOutcome{Content: "an image", Images: []ToolImage{{Data: "AAAA", MimeType: "image/png"}}}
	host := &timedToolHost{scriptedToolHost: scripted, clock: clock, runs: map[string]time.Duration{
		"bash": 2 * time.Second,
		"grep": time.Second,
		"ls":   500 * time.Millisecond,
		"read": 250 * time.Millisecond,
	}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{
			{ID: "ok", Name: "bash"},
			{ID: "tool-error", Name: "grep"},
			{ID: "protocol-error", Name: "ls"},
			{ID: "image", Name: "read"},
		}},
		{text: "done"},
	}}

	if got := clockedEngine(tr, p, host, clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	assertMeasured(t, toolRow(t, tr, "ok"), 2*time.Second)
	assertMeasured(t, toolRow(t, tr, "tool-error"), time.Second)
	assertMeasured(t, toolRow(t, tr, "protocol-error"), 500*time.Millisecond)
	image := toolRow(t, tr, "image")
	if len(image.ContentBlocks) == 0 {
		t.Fatalf("the image result lost its blocks: %+v", image)
	}
	assertMeasured(t, image, 250*time.Millisecond)
}

// A loop-side tool is measured around its own handler. Paired with a call
// that ran for two seconds, it carries none of that call's time: an instant
// handler is a measured 0, not a missing value.
func TestToolDuration_LoopSideToolIsMeasuredAroundItsHandler(t *testing.T) {
	clock := newLoopClock()
	host := &timedToolHost{scriptedToolHost: newScriptedToolHost(), clock: clock, runs: map[string]time.Duration{
		"bash": 2 * time.Second,
	}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{
			{ID: "work", Name: "bash"},
			{ID: "stop", Name: ToolStopBlueprint, Input: map[string]any{"type": "finish", "reason": "r", "summary": "s"}},
		}},
		{text: "done"},
	}}

	if got := clockedEngine(tr, p, host, clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	assertMeasured(t, toolRow(t, tr, "work"), 2*time.Second)
	assertMeasured(t, toolRow(t, tr, "stop"), 0)
}

// The call the tool host died in is measured from its dispatch to the
// failure; the restart that follows is not in it. The calls after it never
// ran.
func TestToolDuration_TheLostCallIsMeasuredToItsFailure(t *testing.T) {
	clock := newLoopClock()
	host := &restartableHost{
		dies:      map[string]bool{"bash": true},
		onCall:    func() { clock.advance(3 * time.Second) },
		onRestart: func() { clock.advance(10 * time.Second) },
	}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{
			{ID: "lost", Name: "bash"},
			{ID: "after-1", Name: "read"},
			{ID: "after-2", Name: "grep"},
		}},
		{text: "done"},
	}}

	if got := clockedEngine(tr, p, host, clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	if host.restarts != 1 {
		t.Fatalf("restarts = %d, want the one the lost call caused", host.restarts)
	}
	assertMeasured(t, toolRow(t, tr, "lost"), 3*time.Second)
	assertUnmeasured(t, toolRow(t, tr, "after-1"))
	assertUnmeasured(t, toolRow(t, tr, "after-2"))
}

// The tool host dying with no restart to follow answers the batch the same
// way: the lost call measured, the rest not.
func TestToolDuration_TheLostCallIsMeasuredWhenTheEngagementEnds(t *testing.T) {
	clock := newLoopClock()
	scripted := newScriptedToolHost()
	scripted.errs["bash"] = io.EOF
	host := &timedToolHost{scriptedToolHost: scripted, clock: clock, runs: map[string]time.Duration{
		"bash": 4 * time.Second,
	}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "lost", Name: "bash"}, {ID: "after", Name: "read"}}},
	}}

	if got := clockedEngine(tr, p, host, clock).Run(context.Background(), testParams()); got.Kind != ResultToolHostLost {
		t.Fatalf("disposition = %v (err: %v), want the tool host lost", got.Kind, got.Err)
	}
	assertMeasured(t, toolRow(t, tr, "lost"), 4*time.Second)
	assertUnmeasured(t, toolRow(t, tr, "after"))
}

// A call the gate refused was never dispatched.
func TestToolDuration_AGateDenialIsUnmeasured(t *testing.T) {
	clock := newLoopClock()
	host := &timedToolHost{scriptedToolHost: newScriptedToolHost(), clock: clock, runs: map[string]time.Duration{
		"read": time.Second,
	}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "denied", Name: "bash"}, {ID: "allowed", Name: "read"}}},
		{text: "done"},
	}}
	e := clockedEngine(tr, p, host, clock)
	e.Hooks.BeforeToolCall = func(_ context.Context, call domain.ToolCall) string {
		clock.advance(5 * time.Second)
		if call.Name == "bash" {
			return "not allowed"
		}
		return ""
	}

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	assertUnmeasured(t, toolRow(t, tr, "denied"))
	assertMeasured(t, toolRow(t, tr, "allowed"), time.Second)
}

// The gate's checkpoint barrier and the after-call bookkeeping are the
// loop's work, not the tool's: a call held at the barrier and rewritten
// slowly afterwards reads as the time it ran.
func TestToolDuration_HooksAddNothing(t *testing.T) {
	clock := newLoopClock()
	host := &timedToolHost{scriptedToolHost: newScriptedToolHost(), clock: clock, runs: map[string]time.Duration{
		"bash": 2 * time.Second,
	}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		{text: "done"},
	}}
	e := clockedEngine(tr, p, host, clock)
	e.Hooks.BeforeToolCall = func(context.Context, domain.ToolCall) string {
		clock.advance(30 * time.Second) // a snapshot reading the tree
		return ""
	}
	e.Hooks.AfterToolCall = func(_ context.Context, _ domain.ToolCall, out ToolOutcome) ToolOutcome {
		clock.advance(20 * time.Second) // the memory mirror, checkpoint bookkeeping
		return out
	}

	if got := e.Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	assertMeasured(t, toolRow(t, tr, "c1"), 2*time.Second)
}

// The calls of a truncated message are answered without being run.
func TestToolDuration_UndispatchedCallsAreUnmeasured(t *testing.T) {
	clock := newLoopClock()
	host := &timedToolHost{scriptedToolHost: newScriptedToolHost(), clock: clock, runs: map[string]time.Duration{}}
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "write"}, {ID: "c2", Name: "edit"}}, finish: "length"},
		{text: "recovered"},
	}}

	if got := clockedEngine(tr, p, host, clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	if calls := host.calls(); len(calls) != 0 {
		t.Fatalf("a truncated batch must not run; the host saw %v", calls)
	}
	assertUnmeasured(t, toolRow(t, tr, "c1"))
	assertUnmeasured(t, toolRow(t, tr, "c2"))
}

// The repair pass answers a call an earlier engagement left unanswered. This
// engagement never ran it.
func TestToolDuration_RepairAnswersAreUnmeasured(t *testing.T) {
	clock := newLoopClock()
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}

	if got := clockedEngine(tr, p, newScriptedToolHost(), clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	assertUnmeasured(t, toolRow(t, tr, "c1"))
}

// An assistant row reads the engine's clock, and a call that took no time on
// it stores 0 rather than leaving the column empty.
func TestAssistantDuration_StoresAMeasuredZero(t *testing.T) {
	clock := newLoopClock()
	tr := newMemTranscript(pendingUser("go"))
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}}, onCall: func() { clock.advance(4 * time.Second) }},
		{text: "done"},
	}}

	if got := clockedEngine(tr, p, newScriptedToolHost(), clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	var assistants []domain.Message
	for _, m := range tr.snapshot() {
		if m.Role == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 2 {
		t.Fatalf("assistant rows = %d, want 2", len(assistants))
	}
	if d := assistants[0].DurationMs; d == nil || *d != 4000 {
		t.Errorf("first assistant DurationMs = %v, want 4000 on the engine's clock", d)
	}
	if d := assistants[1].DurationMs; d == nil || *d != 0 {
		t.Errorf("second assistant DurationMs = %v, want a measured 0", d)
	}
}

// The forced-shape compaction's reply row is measured the same way: a call
// that took no time on the clock stores 0.
func TestCompactionDuration_StoresAMeasuredZero(t *testing.T) {
	clock := newLoopClock()
	old := time.Now().UTC().Add(-time.Hour)
	tr := newMemTranscript(
		domain.Message{Role: "user", Content: "the mission", CreatedAt: old},
		usedAssistant("prior work", overThreshold, old),
	)
	p := &scriptedProvider{turns: []scriptedTurn{
		forcedReply("analysis", "summary"),
		{text: "done"},
	}}

	if got := clockedEngine(tr, p, newScriptedToolHost(), clock).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v (err: %v)", got.Kind, got.Err)
	}
	reply := tr.find(func(m domain.Message) bool { return m.Metadata["compaction_path"] == "forced" })
	if reply == nil {
		t.Fatal("the forced-shape reply row is missing")
	}
	if reply.DurationMs == nil || *reply.DurationMs != 0 {
		t.Errorf("forced reply DurationMs = %v, want a measured 0", reply.DurationMs)
	}
}
