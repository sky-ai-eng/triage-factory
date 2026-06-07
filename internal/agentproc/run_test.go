package agentproc

import (
	"bufio"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// captureSink records what consumeStream delivered, so a test can
// assert the regression-case message survived the stream reader.
type captureSink struct {
	sessionID string
	messages  []*domain.AgentMessage
}

func (c *captureSink) OnSession(sid string) error {
	c.sessionID = sid
	return nil
}

func (c *captureSink) OnMessage(m *domain.AgentMessage) error {
	c.messages = append(c.messages, m)
	return nil
}

// TestConsumeStream_HandlesOversizedToolResult is the SKY-* regression
// for "Run X failed: stream: bufio.Scanner: token too long". A real
// tool_result line carrying a multi-megabyte file read used to exceed
// our 1 MB scanner ceiling; the run aborted with no terminal Result
// captured even though the subprocess kept emitting valid JSON we
// just couldn't read. Asserts the bigger line flows through and the
// terminal `result` event is still observed.
func TestConsumeStream_HandlesOversizedToolResult(t *testing.T) {
	const oldScannerCap = 1 * 1024 * 1024
	huge := strings.Repeat("x", oldScannerCap+1) // Just over the old 1 MB cap.

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-big"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/big"}}]}}`,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"` + huge + `"}]}}`,
		`{"type":"result","is_error":false,"duration_ms":50,"num_turns":1,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"ok\"}"}`,
		"",
	}, "\n")

	sink := &captureSink{}
	result, err := consumeStream(strings.NewReader(stream), sink, NewStreamState(), "trace-big")
	if err != nil {
		t.Fatalf("consumeStream returned error on oversized line: %v", err)
	}
	if result == nil {
		t.Fatal("expected terminal Result, got nil — stream reader bailed before the result event")
	}
	if sink.sessionID != "sess-big" {
		t.Errorf("session id = %q, want sess-big", sink.sessionID)
	}

	var toolMsg *domain.AgentMessage
	for _, m := range sink.messages {
		if m.Role == "tool" {
			toolMsg = m
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("expected a tool message in the sink — the oversized line was dropped")
	}
	if len(toolMsg.Content) != len(huge) {
		t.Errorf("tool message content length = %d, want %d", len(toolMsg.Content), len(huge))
	}
}

// TestConsumeStreamInteractive_MultiTurn is the core multi-envelope
// regression: the interactive reader must NOT stop at the first result
// the way one-shot consumeStream does. A canned two-turn stream
// (ready + init + assistant + result, twice) must deliver both assistant
// messages and fold both results into one (cost/turns summed), and the
// ready signal must fire.
func TestConsumeStreamInteractive_MultiTurn(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"control","subtype":"ready"}`,
		`{"type":"system","subtype":"init","session_id":"sess-i"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"assistant","message":{"id":"m1","stop_reason":"end_turn","content":[]}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":10,"num_turns":1,"total_cost_usd":0.01,"stop_reason":"end_turn","result":"one"}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"second"}]}}`,
		`{"type":"assistant","message":{"id":"m2","stop_reason":"end_turn","content":[]}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":20,"num_turns":1,"total_cost_usd":0.02,"stop_reason":"end_turn","result":"two"}`,
		"",
	}, "\n")

	lr := &LiveRun{ready: make(chan struct{})}
	sink := &captureSink{}
	result, err := lr.consumeStreamInteractive(strings.NewReader(stream), sink, NewStreamState(), nil, "trace-i")
	if err != nil {
		t.Fatalf("consumeStreamInteractive returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected a folded terminal Result, got nil")
	}

	// Both turns folded.
	if result.NumTurns != 2 {
		t.Errorf("NumTurns = %d, want 2 (results not folded)", result.NumTurns)
	}
	if result.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want 0.03 (results not folded)", result.CostUSD)
	}
	if result.Result != "two" {
		t.Errorf("Result = %q, want two (resume text should win)", result.Result)
	}

	// Both assistant messages delivered — the reader did not stop at the
	// first result.
	var texts []string
	for _, m := range sink.messages {
		if m.Role == "assistant" {
			texts = append(texts, m.Content)
		}
	}
	if len(texts) != 2 || texts[0] != "first" || texts[1] != "second" {
		t.Errorf("assistant messages = %v, want [first second]", texts)
	}
	if sink.sessionID != "sess-i" {
		t.Errorf("session id = %q, want sess-i", sink.sessionID)
	}

	// ready signal fired.
	select {
	case <-lr.ready:
	default:
		t.Error("expected ready channel closed after control/ready line")
	}
}

// TestConsumeStreamInteractive_InterruptLabelsResult pins that a
// control/interrupted line tags the following result as an interrupt
// even when the SDK omits the error_during_execution subtype.
func TestConsumeStreamInteractive_InterruptLabelsResult(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"control","subtype":"ready"}`,
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"control","subtype":"interrupted"}`,
		`{"type":"result","is_error":true,"duration_ms":3,"num_turns":1,"total_cost_usd":0,"stop_reason":""}`,
		"",
	}, "\n")

	lr := &LiveRun{ready: make(chan struct{})}
	sink := &captureSink{}
	result, err := lr.consumeStreamInteractive(strings.NewReader(stream), sink, NewStreamState(), nil, "trace-int")
	if err != nil {
		t.Fatalf("consumeStreamInteractive returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected terminal Result, got nil")
	}
	if result.Subtype != "error_during_execution" {
		t.Errorf("Subtype = %q, want error_during_execution (interrupt label)", result.Subtype)
	}
}

// TestReadLine_RejectsRunawayLine guards the upper bound: if the
// subprocess wedges and streams without ever emitting a newline (or a
// single legitimate line somehow exceeds maxStreamLineBytes), we want
// a clear stream error and a failed run, not an OOM. Exercises the
// helper directly with a tight cap so the test stays cheap; the
// production cap is 64 MB.
func TestReadLine_RejectsRunawayLine(t *testing.T) {
	payload := strings.Repeat("y", 2*1024*1024) // No terminating newline.

	r := bufio.NewReader(strings.NewReader(payload))
	_, err := readLine(r, 1*1024*1024)
	if err == nil {
		t.Fatal("expected error when line exceeds cap; got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error message %q should mention the cap was exceeded", err.Error())
	}
}

// TestConsumeStream_TrailingLineWithoutNewline guards the EOF path:
// if the subprocess exits after writing a final event without a
// trailing newline, that event must still be parsed rather than
// silently swallowed by the EOF return.
func TestConsumeStream_TrailingLineWithoutNewline(t *testing.T) {
	// Final result event has no trailing \n.
	stream := `{"type":"system","subtype":"init","session_id":"sess-eof"}` + "\n" +
		`{"type":"result","is_error":false,"duration_ms":1,"num_turns":0,"total_cost_usd":0,"stop_reason":"end_turn","result":"{\"status\":\"completed\",\"summary\":\"\"}"}`

	sink := &captureSink{}
	result, err := consumeStream(strings.NewReader(stream), sink, NewStreamState(), "trace-eof")
	if err != nil {
		t.Fatalf("consumeStream returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected terminal Result on EOF-terminated final line")
	}
}
