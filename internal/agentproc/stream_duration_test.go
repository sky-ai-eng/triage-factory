package agentproc

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// streamClock is a hand-driven clock so the duration assertions below are
// exact rather than "some positive number of milliseconds".
type streamClock struct{ t time.Time }

func newStreamClock() *streamClock             { return &streamClock{t: time.Unix(1700000000, 0)} }
func (c *streamClock) now() time.Time          { return c.t }
func (c *streamClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// state returns a StreamState reading this clock, with its first request
// mark at the clock's current instant.
func (c *streamClock) state() *StreamState {
	s := NewStreamState()
	s.now = c.now
	s.MarkRequest()
	return s
}

func durationOrFail(t *testing.T, label string, ms *int) int {
	t.Helper()
	if ms == nil {
		t.Fatalf("%s: DurationMs is nil, want a measured value", label)
	}
	return *ms
}

// TestParseLine_StampsAssistantDuration pins the assistant half of the
// per-row timing contract: the row carries the wall clock from the request
// going out to its own last line landing, reasoning included — which is what
// makes a "thought for Ns" readout derivable from the row that did the
// thinking, with no reference to any neighbour.
func TestParseLine_StampsAssistantDuration(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(6 * time.Second)
	if msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"thinking","thinking":"hmm","signature":"sig"}]}}`), "t"); len(msgs) != 0 {
		t.Fatalf("thinking must not flush its own row; got %d", len(msgs))
	}

	// A second line for the same message: the stamp settles on the last one,
	// not the first.
	c.advance(2 * time.Second)
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"answer"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected the flushed assistant row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "assistant", msgs[0].DurationMs); got != 8000 {
		t.Errorf("assistant DurationMs = %d, want 8000 (the whole produce-the-message window)", got)
	}
}

// TestParseLine_AssistantDurationExcludesPriorSteps pins that each row is
// measured over its own segment: a second assistant message must not
// re-count the tool time (or the first message's time) that preceded it.
func TestParseLine_AssistantDurationExcludesPriorSteps(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(3 * time.Second)
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`), "t")

	// The tool runs for a while, then its result goes back to the model.
	c.advance(43 * time.Second)
	s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`), "t")

	c.advance(2 * time.Second)
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected the second assistant row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "second assistant", msgs[0].DurationMs); got != 2000 {
		t.Errorf("second assistant DurationMs = %d, want 2000 (its own segment, not the 43s tool wait)", got)
	}
}

// TestParseLine_StampsToolDurationPerCall pins the tool half: each result is
// timed against the dispatch of its own tool_use id, so parallel calls
// resolving out of one assistant turn get the durations they individually
// earned rather than a shared one.
func TestParseLine_StampsToolDurationPerCall(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(time.Second)
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[
		{"type":"tool_use","id":"tu1","name":"Bash","input":{}},
		{"type":"tool_use","id":"tu2","name":"Read","input":{}}]}}`), "t")

	// tu2 comes back first, on its own line.
	c.advance(500 * time.Millisecond)
	fast, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","content":"file"}]}}`), "t")
	if len(fast) != 1 {
		t.Fatalf("expected one tool row for tu2; got %d", len(fast))
	}
	if got := durationOrFail(t, "tu2", fast[0].DurationMs); got != 500 {
		t.Errorf("tu2 DurationMs = %d, want 500", got)
	}

	c.advance(30 * time.Second)
	slow, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`), "t")
	if len(slow) != 1 {
		t.Fatalf("expected one tool row for tu1; got %d", len(slow))
	}
	if got := durationOrFail(t, "tu1", slow[0].DurationMs); got != 30500 {
		t.Errorf("tu1 DurationMs = %d, want 30500 (measured from its own dispatch, not tu2's result)", got)
	}
}

// TestParseLine_ToolDurationAbsentWithoutDispatch pins the honest gap: a
// result whose dispatch this process never observed (a resumed session's
// in-flight call) carries no duration at all rather than one invented from
// whatever mark happened to be lying around.
func TestParseLine_ToolDurationAbsentWithoutDispatch(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(9 * time.Second)
	msgs, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"orphan","content":"out"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected one tool row; got %d", len(msgs))
	}
	if msgs[0].DurationMs != nil {
		t.Errorf("orphan tool DurationMs = %d, want nil (nothing measured it)", *msgs[0].DurationMs)
	}
}

// TestParseLine_MeasuredZeroIsNotAbsent pins the distinction the pointer
// exists for: a step fast enough to round to nothing is a measurement of 0,
// not an unmeasured row.
func TestParseLine_MeasuredZeroIsNotAbsent(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"instant"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected the flushed assistant row; got %d", len(msgs))
	}
	if msgs[0].DurationMs == nil || *msgs[0].DurationMs != 0 {
		t.Errorf("DurationMs = %v, want a measured 0", msgs[0].DurationMs)
	}
}

// TestLiveRunSend_MarksRequest pins the live path's mark: a run parked
// between turns would otherwise bill the entire wait to the first assistant
// message of the next turn, which is the one a "thought for Ns" readout is
// most likely to be read off.
func TestLiveRunSend_MarksRequest(t *testing.T) {
	c := newStreamClock()
	ready := make(chan struct{})
	close(ready)
	l := &LiveRun{
		stdin:  nopWriteCloser{},
		done:   make(chan struct{}),
		ready:  ready,
		stream: c.state(),
	}

	// The run sits parked for an hour waiting on a human follow-up.
	c.advance(time.Hour)
	if err := l.Send(context.Background(), "keep going"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	c.advance(4 * time.Second)
	msgs, _ := l.stream.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected the flushed assistant row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "post-follow-up assistant", msgs[0].DurationMs); got != 4000 {
		t.Errorf("DurationMs = %d, want 4000 (the parked hour is not thinking time)", got)
	}
}

// gatedRun wires a live run whose permission handler takes `waited` on the
// test clock to answer — the human standing at the prompt. It returns the
// rows the reader produced from `lines`, which is the real wiring
// (consumeStreamInteractive → handlePermission → DiscountGate), not the
// marks poked directly.
func gatedRun(t *testing.T, c *streamClock, waited time.Duration, lines ...string) []*domain.Message {
	t.Helper()
	l := &LiveRun{stdin: nopWriteCloser{}, done: make(chan struct{}), ready: make(chan struct{}), stream: c.state()}
	sink := &captureSink{}
	perms := func(PermissionRequest) PermissionDecision {
		c.advance(waited)
		return PermissionDecision{Behavior: "allow"}
	}
	if _, err := l.consumeStreamInteractive(strings.NewReader(strings.Join(lines, "\n")+"\n"), sink, l.stream, perms, nil, "t"); err != nil {
		t.Fatalf("consumeStreamInteractive: %v", err)
	}
	return sink.messages
}

// TestPermissionGate_ExcludedFromToolDuration is the one that matters for a
// gated call: a Bash step a human took two minutes to approve ran for two
// seconds, and the row has to say two seconds. The approval wait is the
// permission record's fact, not this row's.
func TestPermissionGate_ExcludedFromToolDuration(t *testing.T) {
	c := newStreamClock()
	msgs := gatedRun(t, c, 2*time.Minute,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
		`{"type":"control","subtype":"permission_request","tool_call_id":"tu1","tool_name":"Bash","input":{}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`,
	)
	if len(msgs) != 2 {
		t.Fatalf("expected the assistant row and the tool row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "gated tool", msgs[1].DurationMs); got != 0 {
		t.Errorf("gated tool DurationMs = %d, want 0 (it ran instantly; the 2m was the human)", got)
	}
}

// TestPermissionGate_ExcludedWhenPromptArrivesFirst pins the same guarantee
// under the other arrival order. The SDK does not order the gated tool_use
// line against the prompt, and while the reader is parked the tool_use line
// sits unread in the pipe — so with the prompt first it is the ASSISTANT
// row's window the wait would land in instead.
func TestPermissionGate_ExcludedWhenPromptArrivesFirst(t *testing.T) {
	c := newStreamClock()
	msgs := gatedRun(t, c, 10*time.Minute,
		`{"type":"control","subtype":"permission_request","tool_call_id":"tu1","tool_name":"Bash","input":{}}`,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`,
	)
	if len(msgs) != 2 {
		t.Fatalf("expected the assistant row and the tool row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "assistant", msgs[0].DurationMs); got != 0 {
		t.Errorf("assistant DurationMs = %d, want 0 (the 10m stood at the prompt, the model did not think it)", got)
	}
	if got := durationOrFail(t, "gated tool", msgs[1].DurationMs); got != 0 {
		t.Errorf("gated tool DurationMs = %d, want 0", got)
	}
}

// TestPermissionGate_KeepsRealWorkOnEitherSide pins that the discount takes
// the gate window and nothing else: the model's thinking before the prompt
// and the tool's execution after it both survive it intact.
func TestPermissionGate_KeepsRealWorkOnEitherSide(t *testing.T) {
	c := newStreamClock()
	l := &LiveRun{stdin: nopWriteCloser{}, done: make(chan struct{}), ready: make(chan struct{}), stream: c.state()}
	sink := &captureSink{}

	lines := []string{
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
		`{"type":"control","subtype":"permission_request","tool_call_id":"tu1","tool_name":"Bash","input":{}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`,
	}
	// 3s of model time produces the gated call, a human deliberates for 5m,
	// and the approved tool then runs for 7s before its result lands.
	src := &lineReader{lines: lines, before: func(i int) {
		switch i {
		case 0:
			c.advance(3 * time.Second)
		case 2:
			c.advance(7 * time.Second)
		}
	}}
	perms := func(PermissionRequest) PermissionDecision {
		c.advance(5 * time.Minute)
		return PermissionDecision{Behavior: "allow"}
	}
	if _, err := l.consumeStreamInteractive(src, sink, l.stream, perms, nil, "t"); err != nil {
		t.Fatalf("consumeStreamInteractive: %v", err)
	}

	if len(sink.messages) != 2 {
		t.Fatalf("expected the assistant row and the tool row; got %d", len(sink.messages))
	}
	if got := durationOrFail(t, "assistant", sink.messages[0].DurationMs); got != 3000 {
		t.Errorf("assistant DurationMs = %d, want 3000 (its own thinking, kept)", got)
	}
	if got := durationOrFail(t, "gated tool", sink.messages[1].DurationMs); got != 7000 {
		t.Errorf("gated tool DurationMs = %d, want 7000 (its own execution, kept; the 5m gate, dropped)", got)
	}
}

// lineReader hands the reader loop one line per Read so a test can move the
// clock between them — a strings.Reader gives the whole stream up at once,
// which collapses every interval the marks are supposed to distinguish.
// before runs immediately ahead of the line at that index being served.
type lineReader struct {
	lines  []string
	before func(i int)
	i      int
	buf    []byte
}

func (r *lineReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.i >= len(r.lines) {
			return 0, io.EOF
		}
		if r.before != nil {
			r.before(r.i)
		}
		r.buf = []byte(r.lines[r.i] + "\n")
		r.i++
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// TestPermissionGate_SandboxedAutoApproveDiscountsNothing pins the multi-mode
// shape: a sandboxed run auto-approves without a human, so the window is nil
// and the timing is byte-identical to an ungated run.
func TestPermissionGate_SandboxedAutoApproveDiscountsNothing(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(2 * time.Second)
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`), "t")

	gateAt := s.Now() // an auto-approver answers on the spot
	s.DiscountGate(gateAt)

	c.advance(9 * time.Second)
	msgs, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"out"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected one tool row; got %d", len(msgs))
	}
	if got := durationOrFail(t, "tool", msgs[0].DurationMs); got != 9000 {
		t.Errorf("tool DurationMs = %d, want 9000 (nothing to discount)", got)
	}
}
