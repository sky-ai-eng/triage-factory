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

// TestParseLine_ThinkingEmitsOwnMessage pins that a thinking block becomes
// its own subtype:"thinking" message, emitted immediately — ahead of the
// text sibling that accumulates on the shared assistant message and only
// flushes on stop_reason.
func TestParseLine_ThinkingEmitsOwnMessage(t *testing.T) {
	s := NewStreamState()
	msgs, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"thinking","thinking":"let me reason"}]}}`), "t")
	if len(msgs) != 1 {
		t.Fatalf("expected immediate thinking message; got %d msgs", len(msgs))
	}
	th := msgs[0]
	if th.Role != "assistant" || th.Subtype != "thinking" || th.Content != "let me reason" || th.Model != "sonnet" {
		t.Errorf("thinking message wrong shape: %+v", th)
	}

	s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"answer"}]}}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`), "t")
	if len(flushed) != 1 || flushed[0].Subtype != "text" || flushed[0].Content != "answer" {
		t.Fatalf("text sibling should flush separately after thinking; got %+v", flushed)
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
