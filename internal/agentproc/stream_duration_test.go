package agentproc

import (
	"context"
	"testing"
	"time"
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
