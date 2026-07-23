package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestParseLine_CaptureSessionAndAccumulate exercises the typical
// stream-json sequence: system/init carrying session_id, then an
// assistant message with text content, then a result event. Pinned
// behavior: session_id is set before any messages are emitted so a
// caller can persist it eagerly; messages emit only after stop_reason
// or a new msg id (no premature flushes).
func TestParseLine_CaptureSessionAndAccumulate(t *testing.T) {
	s := NewStreamState()

	if msgs, res := s.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"sess-abc"}`), "trace-1"); msgs != nil || res != nil {
		t.Fatalf("system/init should not emit messages or result; got msgs=%v res=%v", msgs, res)
	}
	if got := s.SessionID(); got != "sess-abc" {
		t.Errorf("SessionID = %q, want sess-abc", got)
	}

	// Assistant turn with text but no stop_reason — accumulated, no flush.
	if msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"text","text":"hello"}]}}`), "trace-1"); len(msgs) != 0 {
		t.Errorf("expected no flush before stop_reason; got %d msgs", len(msgs))
	}

	// Same msg id, now with stop_reason — flushes one assistant msg.
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`), "trace-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 flushed msg on stop_reason; got %d", len(msgs))
	}
	if msgs[0].Role != "assistant" || msgs[0].Content != "hello" || msgs[0].RunID != "trace-1" {
		t.Errorf("flushed message wrong shape: %+v", msgs[0])
	}

	// Terminal result event.
	_, res := s.ParseLine([]byte(`{"type":"result","is_error":false,"duration_ms":120,"num_turns":2,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"done\"}"}`), "trace-1")
	if res == nil {
		t.Fatal("expected Result on result event")
	}
	if res.DurationMs != 120 || res.NumTurns != 2 || res.CostUSD != 0.01 || res.StopReason != "end_turn" {
		t.Errorf("result accounting mismatch: %+v", res)
	}
}

// TestParseLine_ThinkingRidesAssistantMessage pins the fidelity fix: a
// thinking block no longer flushes as its own subtype:"thinking" row.
// Instead it accumulates as a Reasoning entry (capturing the signature) on
// the same assistant message its text/tool_use siblings share, and the
// whole thing flushes together on stop_reason.
func TestParseLine_ThinkingRidesAssistantMessage(t *testing.T) {
	s := NewStreamState()

	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"thinking","thinking":"let me reason","signature":"sig-1"}]}}`), "t")
	if len(msgs) != 0 {
		t.Fatalf("thinking block must not emit its own row; got %d msgs: %+v", len(msgs), msgs)
	}

	msgs, _ = s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"answer"}]}}`), "t")
	if len(msgs) != 0 {
		t.Fatalf("text sibling should not flush before stop_reason; got %+v", msgs)
	}

	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`), "t")
	if len(flushed) != 1 {
		t.Fatalf("expected exactly one flushed message carrying both thinking and text; got %d: %+v", len(flushed), flushed)
	}
	msg := flushed[0]
	if msg.Role != "assistant" || msg.Content != "answer" {
		t.Errorf("flushed message wrong shape: %+v", msg)
	}
	if len(msg.Reasoning) != 1 {
		t.Fatalf("expected 1 reasoning entry, got %d: %+v", len(msg.Reasoning), msg.Reasoning)
	}
	r := msg.Reasoning[0]
	if r.Index != 0 || r.Type != "text" || r.Text != "let me reason" || r.Signature != "sig-1" {
		t.Errorf("reasoning entry wrong shape: %+v", r)
	}
	if msg.ContentBlocks != nil {
		t.Errorf("single text block should not promote ContentBlocks; got %+v", msg.ContentBlocks)
	}
}

// TestParseLine_ReasoningOrderingPreserved pins that multiple thinking
// blocks across separate stream lines accumulate onto Reasoning in the
// order they were produced, each carrying its own signature and a
// zero-based Index matching that order.
func TestParseLine_ReasoningOrderingPreserved(t *testing.T) {
	s := NewStreamState()
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"thinking","thinking":"step one","signature":"sig-a"}]}}`), "t")
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"thinking","thinking":"step two","signature":"sig-b"}]}}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`), "t")
	if len(flushed) != 1 {
		t.Fatalf("expected 1 flushed message, got %d", len(flushed))
	}
	got := flushed[0].Reasoning
	if len(got) != 2 {
		t.Fatalf("expected 2 reasoning entries, got %d: %+v", len(got), got)
	}
	if got[0].Index != 0 || got[0].Text != "step one" || got[0].Signature != "sig-a" {
		t.Errorf("reasoning[0] wrong: %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Text != "step two" || got[1].Signature != "sig-b" {
		t.Errorf("reasoning[1] wrong: %+v", got[1])
	}
}

// TestParseLine_MultiTextBlockConcatenation pins the fix for the
// last-block-wins bug: multiple text blocks on one assistant message
// concatenate into Content instead of the last one overwriting the rest,
// and the full block list is preserved on ContentBlocks so nothing is lost.
func TestParseLine_MultiTextBlockConcatenation(t *testing.T) {
	s := NewStreamState()
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"first"}]}}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[{"type":"text","text":"second"}]}}`), "t")
	if len(flushed) != 1 {
		t.Fatalf("expected 1 flushed message, got %d", len(flushed))
	}
	msg := flushed[0]
	if msg.Content != "first\nsecond" {
		t.Errorf("Content = %q, want concatenated text blocks", msg.Content)
	}
	if len(msg.ContentBlocks) != 2 || msg.ContentBlocks[0].Text != "first" || msg.ContentBlocks[1].Text != "second" {
		t.Errorf("ContentBlocks wrong: %+v", msg.ContentBlocks)
	}
}

// TestParseLine_NoThinkingRowEmitted is a broader regression guard: across a
// full turn mixing thinking, text, and tool_use, no emitted message ever
// carries subtype "thinking" — new writes stop producing that row shape
// entirely (historical rows are a frontend-only rendering concern).
func TestParseLine_NoThinkingRowEmitted(t *testing.T) {
	s := NewStreamState()
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"thinking","thinking":"reasoning...","signature":"sig-1"}]}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"c1","name":"Read","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"done"}]}}`,
	}
	for _, line := range lines {
		msgs, _ := s.ParseLine([]byte(line), "t")
		for _, m := range msgs {
			if m.Subtype == "thinking" {
				t.Fatalf("no message should carry subtype=thinking; got %+v", m)
			}
		}
	}
}

func TestParseLine_ToolUseAndToolResult(t *testing.T) {
	s := NewStreamState()
	// Tool use inside assistant turn.
	s.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s"}`), "t")
	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/x"}}]}}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[]}}`), "t")
	if len(flushed) != 1 || flushed[0].Subtype != "tool_use" || len(flushed[0].ToolCalls) != 1 {
		t.Fatalf("expected flushed assistant tool_use; got %+v", flushed)
	}
	if flushed[0].ToolCalls[0].Name != "Read" {
		t.Errorf("tool name = %q, want Read", flushed[0].ToolCalls[0].Name)
	}

	// Tool result emitted as a "user" line.
	out, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"contents"}]}}`), "t")
	var toolMsg *domain.AgentMessage
	for _, m := range out {
		if m.Role == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("expected tool message in %+v", out)
	}
	if toolMsg.ToolCallID != "call-1" || toolMsg.Content != "contents" {
		t.Errorf("tool message wrong: %+v", toolMsg)
	}
}

// TestParseLine_ResultSubtypePopulated pins that the result event's
// subtype flows into Result.Subtype — the interactive reader keys off
// "error_during_execution" to corroborate an interrupt.
func TestParseLine_ResultSubtypePopulated(t *testing.T) {
	s := NewStreamState()
	_, res := s.ParseLine([]byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"num_turns":1,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"ok"}`), "t")
	if res == nil {
		t.Fatal("expected Result on result event")
	}
	if res.Subtype != "success" {
		t.Errorf("Result.Subtype = %q, want success", res.Subtype)
	}
}

// TestParseLine_InterruptSubtype pins that an interrupted turn surfaces
// as a result with subtype error_during_execution — the on-the-wire
// signal the interactive reader treats as an interrupt.
func TestParseLine_InterruptSubtype(t *testing.T) {
	s := NewStreamState()
	_, res := s.ParseLine([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":8,"num_turns":1,"total_cost_usd":0.01}`), "t")
	if res == nil {
		t.Fatal("expected Result on result event")
	}
	if res.Subtype != "error_during_execution" {
		t.Errorf("Result.Subtype = %q, want error_during_execution", res.Subtype)
	}
	if !res.IsError {
		t.Errorf("interrupted result should be is_error=true")
	}
}

// TestParseLine_MultipleResultsEachSurface guards the multi-turn
// streaming case at the parser level: each result envelope yields its
// own Result (the interactive reader is what folds them — ParseLine must
// not swallow the second).
func TestParseLine_MultipleResultsEachSurface(t *testing.T) {
	s := NewStreamState()
	_, r1 := s.ParseLine([]byte(`{"type":"result","subtype":"success","duration_ms":10,"num_turns":1,"total_cost_usd":0.01,"result":"one"}`), "t")
	_, r2 := s.ParseLine([]byte(`{"type":"result","subtype":"success","duration_ms":20,"num_turns":1,"total_cost_usd":0.02,"result":"two"}`), "t")
	if r1 == nil || r2 == nil {
		t.Fatalf("both result envelopes must surface; got r1=%v r2=%v", r1, r2)
	}
	if r1.Result != "one" || r2.Result != "two" {
		t.Errorf("result texts wrong: r1=%q r2=%q", r1.Result, r2.Result)
	}
}

func TestParseLine_IgnoresMalformedJSON(t *testing.T) {
	s := NewStreamState()
	if msgs, res := s.ParseLine([]byte(`not json`), "t"); msgs != nil || res != nil {
		t.Errorf("malformed line should be silently dropped; got msgs=%v res=%v", msgs, res)
	}
}

// TestParseLine_ToolResultBlockArrayContent pins the block-array form of
// tool_result content ([{type:"text",...}]) observed from the SDK stream —
// it must flatten to text rather than store an empty result.
func TestParseLine_ToolResultBlockArrayContent(t *testing.T) {
	s := NewStreamState()
	out, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`), "t")
	if len(out) != 1 {
		t.Fatalf("expected 1 tool message, got %d", len(out))
	}
	if out[0].Content != "line one\nline two" {
		t.Errorf("content = %q, want flattened text blocks", out[0].Content)
	}
	if out[0].ToolCallID != "c1" {
		t.Errorf("tool_call_id = %q, want c1", out[0].ToolCallID)
	}
}

// TestParseLine_ToolResultImageBlockCapture pins the other half of the tool-
// result fidelity fix: an image content block (screenshot, Read on an
// image) must not vanish. Text still flattens onto Content; the image lands
// on ContentBlocks as a data: URI built from the base64 source.
func TestParseLine_ToolResultImageBlockCapture(t *testing.T) {
	s := NewStreamState()
	out, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}}`), "t")
	if len(out) != 1 {
		t.Fatalf("expected 1 tool message, got %d", len(out))
	}
	msg := out[0]
	if msg.Content != "screenshot" {
		t.Errorf("Content = %q, want the flattened text block", msg.Content)
	}
	if len(msg.ContentBlocks) != 1 {
		t.Fatalf("expected 1 content block, got %d: %+v", len(msg.ContentBlocks), msg.ContentBlocks)
	}
	img := msg.ContentBlocks[0]
	if img.Type != domain.ContentBlockImage || img.ImageURL == nil || img.ImageURL.URL != "data:image/png;base64,QUJD" {
		t.Errorf("image block wrong shape: %+v", img)
	}
}

// TestParseLine_ParallelToolResultsAllSurface pins that a single "user" line
// carrying multiple tool_result blocks — parallel tool calls from one
// assistant turn resolving together — yields one message per block instead
// of dropping every result but the first.
func TestParseLine_ParallelToolResultsAllSurface(t *testing.T) {
	s := NewStreamState()
	out, _ := s.ParseLine([]byte(`{"type":"user","message":{"content":[
		{"type":"tool_result","tool_use_id":"c1","content":"result one"},
		{"type":"tool_result","tool_use_id":"c2","content":"result two","is_error":true}
	]}}`), "t")
	if len(out) != 2 {
		t.Fatalf("expected 2 tool messages, got %d: %+v", len(out), out)
	}
	if out[0].ToolCallID != "c1" || out[0].Content != "result one" || out[0].IsError {
		t.Errorf("first tool result wrong: %+v", out[0])
	}
	if out[1].ToolCallID != "c2" || out[1].Content != "result two" || !out[1].IsError {
		t.Errorf("second tool result wrong: %+v", out[1])
	}
}

// TestParseLine_TerminalReasonMarksInterrupted pins the native interrupt
// marker: the SDK reports an interrupted turn as an is_error /
// error_during_execution result (deliberately shape-identical to a runtime
// error) carrying terminal_reason aborted_streaming / aborted_tools — that
// field, not the subtype, is what distinguishes a pause from a failure.
func TestParseLine_TerminalReasonMarksInterrupted(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"aborted_streaming", `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"terminal_reason":"aborted_streaming"}`, true},
		{"aborted_tools", `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"terminal_reason":"aborted_tools"}`, true},
		{"max_turns is not an interrupt", `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":1,"terminal_reason":"max_turns"}`, false},
		{"absent terminal_reason", `{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := NewStreamState().ParseLine([]byte(tc.line), "t")
			if res == nil {
				t.Fatal("expected Result")
			}
			if res.Interrupted != tc.want {
				t.Errorf("Interrupted = %v, want %v", res.Interrupted, tc.want)
			}
		})
	}
}
