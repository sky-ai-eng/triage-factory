package agentproc

import (
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The lines below are the shape Claude Code writes for one model message with
// two calls when the first call's result comes back before the second call's
// block: one assistant line per block, each with stop_reason null and the
// message's usage, and the first result between them.
const (
	interleaveUsage    = `"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`
	interleaveCallA    = `{"type":"assistant","message":{"id":"m1","model":"sonnet","stop_reason":null,"content":[{"type":"tool_use","id":"A","name":"mcp__tf__stop_blueprint","input":{}}],` + interleaveUsage + `}}`
	interleaveResultA  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"A","content":"stopping"}]}}`
	interleaveCallB    = `{"type":"assistant","message":{"id":"m1","model":"sonnet","stop_reason":null,"content":[{"type":"tool_use","id":"B","name":"Bash","input":{}}],` + interleaveUsage + `}}`
	interleaveResultB  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"B","content":"ok"}]}}`
	interleaveNextText = `{"type":"assistant","message":{"id":"m2","model":"sonnet","stop_reason":null,"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}}`
	interleaveResult   = `{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"num_turns":2,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"done"}`
)

// rowShape renders the stored order compactly: an assistant row as its
// message's calls, a tool row as the call it answers.
func rowShape(msgs []*domain.Message) []string {
	var out []string
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			s := "assistant"
			for _, tc := range m.ToolCalls {
				s += ":" + tc.ID
			}
			if m.Content != "" {
				s += ":" + m.Content
			}
			out = append(out, s)
		default:
			out = append(out, m.Role+":"+m.ToolCallID)
		}
	}
	return out
}

func assertRows(t *testing.T, label string, got []*domain.Message, want ...string) {
	t.Helper()
	shape := rowShape(got)
	if strings.Join(shape, " ") != strings.Join(want, " ") {
		t.Fatalf("%s = %v, want %v", label, shape, want)
	}
}

// TestParseLine_ResultMidMessageKeepsOneRow pins that a tool result arriving
// between two blocks of one model message neither ends the message's row nor
// is stored ahead of it: the message is one row holding both calls, and both
// results follow it, as the native loop stores them.
func TestParseLine_ResultMidMessageKeepsOneRow(t *testing.T) {
	s := NewStreamState()
	for _, line := range []string{interleaveCallA, interleaveResultA, interleaveCallB, interleaveResultB} {
		if msgs, _ := s.ParseLine([]byte(line), "t"); len(msgs) != 0 {
			t.Fatalf("line %s emitted %v while its message was still open, want nothing", line, rowShape(msgs))
		}
	}
	msgs, _ := s.ParseLine([]byte(interleaveNextText), "t")
	assertRows(t, "rows emitted when the next message began", msgs, "assistant:A:B", "tool:A", "tool:B")
	if got := msgs[0].ToolCalls[0].Name; got != "stop_blueprint" {
		t.Errorf("first call stored as %q, want stop_blueprint", got)
	}
	msgs, _ = s.ParseLine([]byte(interleaveResult), "t")
	assertRows(t, "rows emitted at the result", msgs, "assistant:done")
}

// TestUsageSink_CountsAnInterleavedMessageOnce pins the token half: every
// block line of a message carries the message's usage, so everything that
// sums the rows counts the message once only while it is one row.
func TestUsageSink_CountsAnInterleavedMessageOnce(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		interleaveCallA, interleaveResultA, interleaveCallB, interleaveResultB, interleaveNextText, interleaveResult,
	}, "\n") + "\n"
	sink := &UsageSink{}
	if _, err := consumeStream(strings.NewReader(stream), sink, NewStreamState(), "t"); err != nil {
		t.Fatal(err)
	}
	if sink.InputTokens != 11 || sink.OutputTokens != 7 || sink.CacheReadTokens != 103 || sink.CacheCreationTokens != 24 {
		t.Errorf("usage = in %d, out %d, cache read %d, cache write %d; want 11, 7, 103, 24 (each message once)",
			sink.InputTokens, sink.OutputTokens, sink.CacheReadTokens, sink.CacheCreationTokens)
	}
}

// TestParseLine_ResultMidMessageLeavesItsTimingAlone pins the timing half: a
// result going back while its message still streams marks the next request,
// not this one, so the message is measured from its own request to its last
// line, and the next message from the last result.
func TestParseLine_ResultMidMessageLeavesItsTimingAlone(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(time.Second)
	s.ParseLine([]byte(interleaveCallA), "t")
	c.advance(100 * time.Millisecond)
	s.ParseLine([]byte(interleaveResultA), "t")
	c.advance(200 * time.Millisecond)
	s.ParseLine([]byte(interleaveCallB), "t")
	c.advance(2 * time.Second)
	s.ParseLine([]byte(interleaveResultB), "t")
	c.advance(3 * time.Second)
	msgs, _ := s.ParseLine([]byte(interleaveNextText), "t")
	assertRows(t, "rows", msgs, "assistant:A:B", "tool:A", "tool:B")

	if got := durationOrFail(t, "first message", msgs[0].DurationMs); got != 1300 {
		t.Errorf("first message DurationMs = %d, want 1300 (its request to its last block, not from A's result)", got)
	}
	if got := durationOrFail(t, "A", msgs[1].DurationMs); got != 100 {
		t.Errorf("A DurationMs = %d, want 100", got)
	}
	if got := durationOrFail(t, "B", msgs[2].DurationMs); got != 2000 {
		t.Errorf("B DurationMs = %d, want 2000", got)
	}

	msgs, _ = s.ParseLine([]byte(interleaveResult), "t")
	if got := durationOrFail(t, "second message", msgs[0].DurationMs); got != 3000 {
		t.Errorf("second message DurationMs = %d, want 3000 (from B's result going back)", got)
	}
}

// TestParseLine_NextMessageWithoutResultsMeasuresFromTheLastLine pins the
// mark a new message is measured from when nothing went back in between: the
// previous message's last line, not the moment the new one's first line
// closed it.
func TestParseLine_NextMessageWithoutResultsMeasuresFromTheLastLine(t *testing.T) {
	c := newStreamClock()
	s := c.state()

	c.advance(time.Second)
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"one"}]}}`), "t")
	c.advance(4 * time.Second)
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"two"}]}}`), "t")
	assertRows(t, "rows", msgs, "assistant:one")
	msgs, _ = s.ParseLine([]byte(interleaveResult), "t")
	if got := durationOrFail(t, "second message", msgs[0].DurationMs); got != 4000 {
		t.Errorf("second message DurationMs = %d, want 4000 (from the first message's last line)", got)
	}
}

// batchReader runs consumeStreamInteractive over lines with a tool-batch
// answer that records what the sink held when the batch was answered.
func batchReader(t *testing.T, lines ...string) (atBatch []string, all []*domain.Message) {
	t.Helper()
	sink := &captureSink{}
	l := &LiveRun{
		ready: make(chan struct{}),
		stdin: captureWriteCloser{newSyncBuffer()},
		toolBatch: func() ToolBatchNotes {
			atBatch = rowShape(sink.messages)
			return ToolBatchNotes{}
		},
	}
	stream := strings.Join(append([]string{`{"type":"control","subtype":"ready"}`, `{"type":"system","subtype":"init","session_id":"s"}`}, lines...), "\n") + "\n"
	if _, err := l.consumeStreamInteractive(strings.NewReader(stream), sink, NewStreamState(), nil, nil, "t"); err != nil {
		t.Fatal(err)
	}
	return atBatch, sink.messages
}

// TestConsumeStreamInteractive_ToolBatchClosesItsMessage pins the earliest
// close: a finished tool batch means its message has ended, so the message's
// row and its results are stored when the batch is answered rather than when
// the model's next message begins.
func TestConsumeStreamInteractive_ToolBatchClosesItsMessage(t *testing.T) {
	atBatch, all := batchReader(t,
		interleaveCallA, interleaveResultA, interleaveCallB, interleaveResultB,
		toolBatchLine("b-1", "A", "B"),
		interleaveNextText, interleaveResult)
	if strings.Join(atBatch, " ") != "assistant:A:B tool:A tool:B" {
		t.Errorf("sink held %v when the batch was answered, want the message and both results", atBatch)
	}
	assertRows(t, "stored rows", all, "assistant:A:B", "tool:A", "tool:B", "assistant:done")
}

// TestConsumeStreamInteractive_EarlyToolBatchLeavesItsMessageOpen pins the
// guard on that close: the batch is reported apart from the stream's lines
// and can be read before the line carrying one of its calls, and closing the
// row then would split the message in two.
func TestConsumeStreamInteractive_EarlyToolBatchLeavesItsMessageOpen(t *testing.T) {
	atBatch, all := batchReader(t,
		interleaveCallA, interleaveResultA,
		toolBatchLine("b-1", "A", "B"),
		interleaveCallB, interleaveResultB, interleaveNextText, interleaveResult)
	if len(atBatch) != 0 {
		t.Errorf("sink held %v when the batch was answered, want nothing: the message had not ended in the stream", atBatch)
	}
	assertRows(t, "stored rows", all, "assistant:A:B", "tool:A", "tool:B", "assistant:done")
}

// TestStreamReaders_EndOfStreamKeepsTheOpenMessage pins that a stream ending
// mid-message, a process that died, still stores the message's row and the
// results held behind it: those calls ran.
func TestStreamReaders_EndOfStreamKeepsTheOpenMessage(t *testing.T) {
	body := []string{`{"type":"system","subtype":"init","session_id":"s"}`, interleaveCallA, interleaveResultA}

	t.Run("one-shot", func(t *testing.T) {
		sink := &captureSink{}
		if _, err := consumeStream(strings.NewReader(strings.Join(body, "\n")+"\n"), sink, NewStreamState(), "t"); err != nil {
			t.Fatal(err)
		}
		assertRows(t, "stored rows", sink.messages, "assistant:A", "tool:A")
	})
	t.Run("interactive", func(t *testing.T) {
		sink := &captureSink{}
		lr := &LiveRun{ready: make(chan struct{})}
		if _, err := lr.consumeStreamInteractive(strings.NewReader(strings.Join(body, "\n")+"\n"), sink, NewStreamState(), nil, nil, "t"); err != nil {
			t.Fatal(err)
		}
		assertRows(t, "stored rows", sink.messages, "assistant:A", "tool:A")
	})
}
