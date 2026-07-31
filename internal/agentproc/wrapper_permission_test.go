package agentproc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// permissionStubSDK stands in for @anthropic-ai/claude-agent-sdk so the
// permission round-trip can be driven without the real runtime (or an API
// key): query() immediately gates one tool call with a known toolUseID and the
// prompt copy the SDK renders alongside it, then yields whatever decision came
// back so the test can read the answer off stdout.
const permissionStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      const decision = await options.canUseTool(
        "Write",
        { file_path: "/tmp/notes.txt", content: "hi" },
        {
          toolUseID: "toolu_01FIXTUREID",
          title: "Claude wants to write notes.txt",
          displayName: "Write file",
          description: "Claude will create a new file",
        },
      )
      yield { type: "stub_decision", decision }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperPermissionRequestUsesToolUseID pins the identifier the wrapper
// puts on a permission prompt: the SDK's own toolUseID for the gated call,
// emitted as tool_call_id, with the SDK's prompt copy alongside it. The reply
// is keyed by that same id, and an allow with no override echoes the original
// input back as updatedInput (the SDK's CLI rejects a bare allow).
func TestWrapperPermissionRequestUsesToolUseID(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, permissionStubSDK)
	req := w.next()
	if req["type"] != "control" || req["subtype"] != "permission_request" {
		t.Fatalf("first non-ready line = %v, want a permission_request", req)
	}
	if got := req["tool_call_id"]; got != "toolu_01FIXTUREID" {
		t.Errorf("tool_call_id = %v, want the SDK's toolUseID (toolu_01FIXTUREID)", got)
	}
	if _, ok := req["request_id"]; ok {
		t.Error("permission_request still carries a request_id; the tool_use id is the identifier now")
	}
	if got := req["tool_name"]; got != "Write" {
		t.Errorf("tool_name = %v, want Write", got)
	}
	if got := req["title"]; got != "Claude wants to write notes.txt" {
		t.Errorf("title = %v, want the SDK's rendered prompt sentence", got)
	}
	if got := req["display_name"]; got != "Write file" {
		t.Errorf("display_name = %v, want Write file", got)
	}
	if got := req["description"]; got != "Claude will create a new file" {
		t.Errorf("description = %v, want the SDK's subtitle", got)
	}

	// Answer the prompt by its tool_call_id. An allow with no override must come
	// back to the SDK carrying the original input.
	w.answer(t, "toolu_01FIXTUREID")

	answered := w.next()
	if answered["type"] != "stub_decision" {
		t.Fatalf("expected the stub's decision echo, got %v (stderr: %s)", answered, w.stderr.String())
	}
	decision, _ := answered["decision"].(map[string]any)
	if decision["behavior"] != "allow" {
		t.Fatalf("behavior = %v, want allow", decision["behavior"])
	}
	updated, _ := decision["updatedInput"].(map[string]any)
	if updated["file_path"] != "/tmp/notes.txt" || updated["content"] != "hi" {
		t.Errorf("updatedInput = %v, want the original tool input echoed back", updated)
	}
}

// TestWrapperHasNoPermissionCounter guards the identifier itself: a per-process
// counter is meaningless outside the run that minted it, so nothing may
// reintroduce one alongside the tool_use id.
func TestWrapperHasNoPermissionCounter(t *testing.T) {
	if bytes.Contains(wrapperJS, []byte("permCounter")) {
		t.Error("wrapper.mjs mints its own permission ids again; the SDK's toolUseID is the identifier")
	}
}

// TestParseControlLine_PermissionRequest pins the Go half of the same contract:
// the reader decodes the wrapper's permission_request into the fields the
// handler sees, tool_call_id included.
func TestParseControlLine_PermissionRequest(t *testing.T) {
	line := []byte(`{"type":"control","subtype":"permission_request","tool_call_id":"toolu_01ABC",` +
		`"tool_name":"Bash","input":{"command":"ls"},"title":"Claude wants to run ls",` +
		`"display_name":"Run command","description":"Lists the directory"}`)
	ctl, ok := parseControlLine(line)
	if !ok {
		t.Fatal("parseControlLine rejected a control envelope")
	}
	if ctl.ToolCallID != "toolu_01ABC" {
		t.Errorf("ToolCallID = %q, want toolu_01ABC", ctl.ToolCallID)
	}
	if ctl.ToolName != "Bash" || ctl.Input["command"] != "ls" {
		t.Errorf("tool = %q input = %v, want Bash / {command: ls}", ctl.ToolName, ctl.Input)
	}
	if ctl.Title != "Claude wants to run ls" || ctl.DisplayName != "Run command" || ctl.Description != "Lists the directory" {
		t.Errorf("prompt copy = (%q, %q, %q), want the SDK's title/display_name/description",
			ctl.Title, ctl.DisplayName, ctl.Description)
	}
}

// TestHandlePermission_RepliesKeyedByToolCallID pins the reply leg: the handler
// sees the tool_use id, and the response the wrapper matches against is keyed
// by that same value.
func TestHandlePermission_RepliesKeyedByToolCallID(t *testing.T) {
	stdin := newSyncBuffer()
	l := &LiveRun{stdin: captureWriteCloser{stdin}, done: make(chan struct{})}

	var seen PermissionRequest
	l.handlePermission(controlLine{
		Subtype:    "permission_request",
		ToolCallID: "toolu_01ABC",
		ToolName:   "Bash",
		Input:      map[string]any{"command": "ls"},
		Title:      "Claude wants to run ls",
	}, func(req PermissionRequest) PermissionDecision {
		seen = req
		return PermissionDecision{Behavior: "deny", Message: "nope"}
	})

	if seen.ToolCallID != "toolu_01ABC" {
		t.Errorf("handler saw ToolCallID %q, want toolu_01ABC", seen.ToolCallID)
	}
	if seen.Title != "Claude wants to run ls" {
		t.Errorf("handler saw Title %q, want the SDK's prompt sentence", seen.Title)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &resp); err != nil {
		t.Fatalf("decode permission_response %q: %v", stdin.String(), err)
	}
	if resp["kind"] != "permission_response" || resp["tool_call_id"] != "toolu_01ABC" || resp["behavior"] != "deny" {
		t.Errorf("permission_response = %v, want a deny keyed by toolu_01ABC", resp)
	}
	if _, ok := resp["request_id"]; ok {
		t.Error("permission_response still carries a request_id; the wrapper matches on tool_call_id")
	}
}

// missingToolUseIDStubSDK is the contract-violating case sdk.d.ts says cannot
// happen: canUseTool invoked with no toolUseID at all. It gates two calls
// concurrently, because collapsing both onto one absent key is how a missing id
// strands a turn rather than merely mislabeling it.
const missingToolUseIDStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      const decisions = await Promise.all([
        options.canUseTool("Read", { file_path: "/tmp/a.txt" }, {}),
        options.canUseTool("Read", { file_path: "/tmp/b.txt" }, {}),
      ])
      yield { type: "stub_decision", decisions }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperPermissionRequestSynthesizesMissingToolUseID pins the fallback: an
// absent toolUseID must still produce a prompt that can be answered. Without a
// synthesized key the promise parks under `undefined`, the reply arrives keyed
// by the empty string (JSON drops an undefined field), nothing matches, and the
// agent's turn hangs until the run ends — and two parallel calls collapse onto
// that one key and strand each other.
func TestWrapperPermissionRequestSynthesizesMissingToolUseID(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, missingToolUseIDStubSDK)

	ids := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		req := w.next()
		if req["subtype"] != "permission_request" {
			t.Fatalf("line %d = %v, want a permission_request", i, req)
		}
		id, _ := req["tool_call_id"].(string)
		if id == "" {
			t.Fatalf("tool_call_id = %v, want a synthesized non-empty id", req["tool_call_id"])
		}
		if !strings.HasPrefix(id, "synthetic-") {
			t.Errorf("tool_call_id = %q, want the synthetic- prefix so it is not mistaken for a tool_use id", id)
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both prompts keyed by %q; parallel calls would strand each other", ids[0])
	}

	// Each prompt is independently answerable by the id it was announced with —
	// the property the missing id would otherwise cost.
	for _, id := range ids {
		w.answer(t, id)
	}
	answered := w.next()
	if answered["type"] != "stub_decision" {
		t.Fatalf("expected the stub's decision echo, got %v (stderr: %s)", answered, w.stderr.String())
	}
	decisions, _ := answered["decisions"].([]any)
	if len(decisions) != 2 {
		t.Fatalf("decisions = %v, want both calls resolved", answered["decisions"])
	}
	for i, d := range decisions {
		decision, _ := d.(map[string]any)
		if decision["behavior"] != "allow" {
			t.Errorf("decision %d = %v, want allow", i, decision)
		}
	}
}

// wrapperProc is a running wrapper.mjs speaking the control protocol over
// stdio, with a stub SDK resolvable from its directory.
type wrapperProc struct {
	stdin  io.WriteCloser
	stderr *syncBuffer
	lines  *bufio.Scanner
	t      *testing.T
}

// startWrapper materializes the SHIPPED wrapper (not a copy of the source file,
// so the test pins what the binary installs) next to a stub
// @anthropic-ai/claude-agent-sdk, and runs it in streaming-input mode with
// permission prompts on. The process is killed and reaped at test end.
func startWrapper(t *testing.T, node, stubSDK string) *wrapperProc {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir stub sdk: %v", err)
	}
	writeFile(t, filepath.Join(pkgDir, "package.json"),
		`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.0.0-stub","type":"module","main":"index.mjs"}`)
	writeFile(t, filepath.Join(pkgDir, "index.mjs"), stubSDK)
	wrapper := filepath.Join(dir, "wrapper.mjs")
	writeFile(t, wrapper, string(wrapperJS))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, node, wrapper, "--input-format", "stream-json", "--permission-prompts")
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := newSyncBuffer()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &wrapperProc{stdin: stdin, stderr: stderr, lines: lines, t: t}
}

// next returns the wrapper's next stdout line as a decoded object, skipping the
// ready signal. A closed stream before one arrives fails the test rather than
// blocking (the process is under a ctx deadline).
func (w *wrapperProc) next() map[string]any {
	w.t.Helper()
	for w.lines.Scan() {
		var m map[string]any
		if err := json.Unmarshal(w.lines.Bytes(), &m); err != nil {
			w.t.Fatalf("non-JSON wrapper output %q (stderr: %s)", w.lines.Text(), w.stderr.String())
		}
		if m["subtype"] == "ready" {
			continue // the wrapper's start signal, not content
		}
		return m
	}
	w.t.Fatalf("wrapper output ended early (stderr: %s)", w.stderr.String())
	return nil
}

// answer allows the prompt keyed by toolCallID, with no input override — so the
// wrapper must echo the original input back as updatedInput.
func (w *wrapperProc) answer(t *testing.T, toolCallID string) {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"kind":         "permission_response",
		"tool_call_id": toolCallID,
		"behavior":     "allow",
	})
	if err != nil {
		t.Fatalf("marshal permission_response: %v", err)
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		t.Fatalf("write permission_response: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// captureWriteCloser is the stdin WriteCloser a LiveRun holds, kept readable so
// a test can assert on the control lines written to it.
type captureWriteCloser struct{ b *syncBuffer }

func (c captureWriteCloser) Write(p []byte) (int, error) { return c.b.Write(p) }
func (c captureWriteCloser) Close() error                { return nil }
