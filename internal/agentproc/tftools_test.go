package agentproc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
)

// testTFTool is a TF tool whose handler is the test's.
func testTFTool(name string, handler tooldefs.Handler) tooldefs.Tool {
	return tooldefs.Tool{
		Name:        name,
		Description: "a test tool",
		Params: tooldefs.Schema{
			Type:     "object",
			Fields:   []tooldefs.Field{{Name: "note", Schema: tooldefs.Schema{Type: "string", Description: "anything"}}},
			Required: []string{"note"},
		},
		Handler: handler,
	}
}

// TestBuildArgs_TFToolsOnlyInteractiveWithATool pins when the wrapper is told
// to serve TF tools: in streaming-input mode, and only with at least one, so
// a run offered none builds no MCP server and imports no zod. The value names
// each tool with its description and a plain JSON Schema.
func TestBuildArgs_TFToolsOnlyInteractiveWithATool(t *testing.T) {
	tools := []tooldefs.Tool{testTFTool("record_call", nil)}

	if args := BuildArgs(RunOptions{Interactive: true}); slices.Contains(args, "--tf-tools") {
		t.Errorf("an interactive run offered no TF tool carries --tf-tools: %v", args)
	}
	if args := BuildArgs(RunOptions{Message: "hi", TFTools: tools}); slices.Contains(args, "--tf-tools") {
		t.Errorf("a one-shot run carries --tf-tools, with no channel to answer a call on: %v", args)
	}

	args := BuildArgs(RunOptions{Interactive: true, TFTools: tools})
	i := slices.Index(args, "--tf-tools")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("an interactive run with a TF tool has no --tf-tools value: %v", args)
	}
	var specs []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &specs); err != nil {
		t.Fatalf("--tf-tools is not the spec array: %v\n%s", err, args[i+1])
	}
	if len(specs) != 1 || specs[0].Name != "record_call" || specs[0].Description != "a test tool" {
		t.Fatalf("specs = %+v, want the one tool with its description", specs)
	}
	schema := specs[0].InputSchema
	if schema.Type != "object" || schema.Properties["note"] == nil || !slices.Equal(schema.Required, []string{"note"}) {
		t.Errorf("input_schema = %+v, want the tool's argument schema", schema)
	}
}

// TestBuildAllowedToolsFor_GrantsTFToolsByTheirMCPName pins that each TF tool
// is granted under the name the model calls it by, so a call never prompts.
func TestBuildAllowedToolsFor_GrantsTFToolsByTheirMCPName(t *testing.T) {
	got := strings.Split(BuildAllowedToolsFor(AllowedToolsOptions{SelfBin: "/bin/tf", TFTools: []string{"render_component", "record_call"}}), ",")
	for _, want := range []string{"mcp__tf__render_component", "mcp__tf__record_call"} {
		if !slices.Contains(got, want) {
			t.Errorf("allowlist is missing %s", want)
		}
	}
	if none := BuildAllowedToolsFor(AllowedToolsOptions{SelfBin: "/bin/tf"}); strings.Contains(none, tfToolPrefix) {
		t.Errorf("a run offered no TF tool grants one: %s", none)
	}
}

// TestParseLine_StoresTFToolsWithoutTheirMCPPrefix pins that a TF tool's row
// names it as the native loop's does, while any other MCP tool keeps the name
// the SDK reported.
func TestParseLine_StoresTFToolsWithoutTheirMCPPrefix(t *testing.T) {
	s := NewStreamState()
	s.ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s"}`), "t")
	flushed, _ := s.ParseLine([]byte(`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[`+
		`{"type":"tool_use","id":"c1","name":"mcp__tf__x","input":{}},`+
		`{"type":"tool_use","id":"c2","name":"mcp__other__y","input":{}}]}}`), "t")
	if len(flushed) != 1 || len(flushed[0].ToolCalls) != 2 {
		t.Fatalf("expected one assistant row with two calls, got %+v", flushed)
	}
	if got := flushed[0].ToolCalls[0].Name; got != "x" {
		t.Errorf("TF tool stored as %q, want x", got)
	}
	if got := flushed[0].ToolCalls[1].Name; got != "mcp__other__y" {
		t.Errorf("another server's tool stored as %q, want mcp__other__y unchanged", got)
	}
}

// readerHarness drives consumeStreamInteractive over a pipe, so a test can
// write wrapper lines one at a time and read what the reader wrote back.
type readerHarness struct {
	t       *testing.T
	l       *LiveRun
	stdout  *io.PipeWriter
	stdin   *syncBuffer
	results chan *Result
	ended   chan struct{}
}

func newReaderHarness(t *testing.T, tools ...tooldefs.Tool) *readerHarness {
	t.Helper()
	pr, pw := io.Pipe()
	stdin := newSyncBuffer()
	h := &readerHarness{
		t:      t,
		stdout: pw,
		stdin:  stdin,
		l: &LiveRun{
			stdin:   captureWriteCloser{stdin},
			ready:   make(chan struct{}),
			done:    make(chan struct{}),
			tfTools: tfToolsByName(tools),
			tfRun:   tooldefs.Run{OrgID: "org", ConversationID: "conv", ClaimID: "claim"},
		},
		results: make(chan *Result, 8),
		ended:   make(chan struct{}),
	}
	go func() {
		defer close(h.ended)
		_, _ = h.l.consumeStreamInteractive(pr, NoopSink{}, NewStreamState(), nil, func(r *Result) { h.results <- r }, "t")
	}()
	t.Cleanup(h.end)
	return h
}

// write hands the reader one line. The pipe has no buffer, so a write that
// does not land means the reader has stopped reading.
func (h *readerHarness) write(line string) {
	h.t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := h.stdout.Write([]byte(line + "\n"))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			h.t.Fatalf("write %s: %v", line, err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("the reader did not take %s: something it ran is holding it", line)
	}
}

// end closes the stream, as the wrapper's exit does, and waits for the
// reader to return.
func (h *readerHarness) end() {
	_ = h.stdout.Close()
	<-h.ended
}

// replies returns the tool_call_reply lines written so far.
func (h *readerHarness) replies() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.stdin.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["kind"] == "tool_call_reply" {
			out = append(out, m)
		}
	}
	return out
}

// waitReplies waits until n tool_call_reply lines have been written.
func (h *readerHarness) waitReplies(n int) []map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := h.replies(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %d tool_call_reply lines; stdin holds %q", n, h.stdin.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func toolCallLine(id, toolUseID, name string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "control", "subtype": "tool_call", "id": id,
		"tool_call_id": toolUseID, "tool_name": name, "input": map[string]any{"note": "hi"},
	})
	return string(b)
}

const resultLine = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok"}`

// TestConsumeStreamInteractive_ToolCallRunsTheHandler: a tool_call runs the
// named tool's handler with the call's tool_use id, its arguments and the
// run's identity, and its answer goes back as one tool_call_reply keyed by
// the wrapper's id.
func TestConsumeStreamInteractive_ToolCallRunsTheHandler(t *testing.T) {
	seen := make(chan tooldefs.Call, 1)
	h := newReaderHarness(t, testTFTool("record_call", func(_ context.Context, call tooldefs.Call) tooldefs.Outcome {
		seen <- call
		return tooldefs.Outcome{Content: "recorded", IsError: true}
	}))

	h.write(toolCallLine("w-1", "toolu_01", "record_call"))
	call := <-seen
	if call.ID != "toolu_01" || call.Args["note"] != "hi" {
		t.Errorf("handler saw id %q args %v, want toolu_01 and the call's input", call.ID, call.Args)
	}
	if want := (tooldefs.Run{OrgID: "org", ConversationID: "conv", ClaimID: "claim"}); call.Run != want {
		t.Errorf("handler saw run %+v, want %+v", call.Run, want)
	}

	h.waitReplies(1)
	h.end()
	// Read again once the stream has ended, so a second reply would show.
	replies := h.replies()
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want exactly one: %v", len(replies), replies)
	}
	if r := replies[0]; r["id"] != "w-1" || r["content"] != "recorded" || r["is_error"] != true {
		t.Errorf("reply = %v, want the handler's answer keyed by w-1", r)
	}
}

// TestConsumeStreamInteractive_ASlowToolCallDoesNotHoldTheStream: while a
// handler is still running, the lines after its call are read and acted on.
func TestConsumeStreamInteractive_ASlowToolCallDoesNotHoldTheStream(t *testing.T) {
	release := make(chan struct{})
	// Deferred too, so a failure below still frees a reader the handler is
	// holding, and the harness's cleanup can wait for it.
	releaseHandler := sync.OnceFunc(func() { close(release) })
	defer releaseHandler()
	h := newReaderHarness(t, testTFTool("slow", func(context.Context, tooldefs.Call) tooldefs.Outcome {
		<-release
		return tooldefs.Outcome{Content: "finally"}
	}))

	h.write(toolCallLine("w-1", "toolu_01", "slow"))
	h.write(resultLine)
	select {
	case <-h.results:
	case <-time.After(5 * time.Second):
		t.Fatal("the line after a running tool call was not read: the handler is holding the reader")
	}
	if got := h.replies(); len(got) != 0 {
		t.Fatalf("replied before the handler finished: %v", got)
	}

	releaseHandler()
	if r := h.waitReplies(1)[0]; r["id"] != "w-1" || r["content"] != "finally" {
		t.Errorf("reply = %v, want the slow handler's answer", r)
	}
}

// TestConsumeStreamInteractive_UnknownAndTimedOutToolCallsAreErrors: a name
// the run was not offered, and a handler still running at the deadline, are
// both answered with an error, the second saying it timed out.
func TestConsumeStreamInteractive_UnknownAndTimedOutToolCallsAreErrors(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	h := newReaderHarness(t, testTFTool("stuck", func(context.Context, tooldefs.Call) tooldefs.Outcome {
		// Ignores its context, so the deadline alone has to answer it.
		<-stuck
		return tooldefs.Outcome{Content: "too late"}
	}))
	h.l.toolCallTimeout = 20 * time.Millisecond

	h.write(toolCallLine("w-unknown", "toolu_01", "no_such_tool"))
	h.write(toolCallLine("w-stuck", "toolu_02", "stuck"))
	byID := map[string]map[string]any{}
	for _, r := range h.waitReplies(2) {
		byID[r["id"].(string)] = r
	}

	if r := byID["w-unknown"]; r["is_error"] != true || !strings.Contains(r["content"].(string), "no_such_tool") {
		t.Errorf("unknown tool reply = %v, want an error naming it", r)
	}
	if r := byID["w-stuck"]; r["is_error"] != true || !strings.Contains(r["content"].(string), "timed out") {
		t.Errorf("timed-out reply = %v, want an error saying it timed out", r)
	}
}

// TestConsumeStreamInteractive_ToolCallCancelEndsTheHandler: a
// tool_call_cancel ends that handler's context, and the end of the stream
// ends every handler still running.
func TestConsumeStreamInteractive_ToolCallCancelEndsTheHandler(t *testing.T) {
	errs := map[string]chan error{"toolu_cancelled": make(chan error, 1), "toolu_orphaned": make(chan error, 1)}
	h := newReaderHarness(t, testTFTool("wait", func(ctx context.Context, call tooldefs.Call) tooldefs.Outcome {
		<-ctx.Done()
		errs[call.ID] <- ctx.Err()
		return tooldefs.Outcome{}
	}))

	h.write(toolCallLine("w-1", "toolu_cancelled", "wait"))
	h.write(toolCallLine("w-2", "toolu_orphaned", "wait"))
	h.write(`{"type":"control","subtype":"tool_call_cancel","id":"w-1"}`)
	select {
	case err := <-errs["toolu_cancelled"]:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled handler's context ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool_call_cancel did not end the handler's context")
	}
	select {
	case err := <-errs["toolu_orphaned"]:
		t.Fatalf("cancelling one call ended another's context: %v", err)
	default:
	}

	h.end()
	select {
	case err := <-errs["toolu_orphaned"]:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("orphaned handler's context ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the stream did not end a running handler's context")
	}
	if got := h.replies(); len(got) != 0 {
		t.Errorf("cancelled calls were answered: %v", got)
	}
}
