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

// optionsEchoStubSDK stands in for the SDK to read back the Options the
// wrapper assembled from argv: query() yields them once and ends, so a test
// can assert on what the real SDK would have been handed.
const optionsEchoStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      yield {
        type: "stub_options",
        disallowedTools: options.disallowedTools ?? null,
        settingSources: options.settingSources ?? null,
        settings: options.settings ?? null,
      }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperDisallowedToolsReachSDKOptions pins that --disallowedTools is
// parsed into options.disallowedTools as a list, the same split the
// allowlist gets. A flag the wrapper ignored would log and drop it, and the
// tool it names would silently stay in the model's context.
func TestWrapperDisallowedToolsReachSDKOptions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, optionsEchoStubSDK, "--disallowedTools", "AskUserQuestion,WebSearch")
	msg := w.next()
	if msg["type"] != "stub_options" {
		t.Fatalf("first non-ready line = %v, want stub_options", msg)
	}
	got, _ := msg["disallowedTools"].([]any)
	want := []any{"AskUserQuestion", "WebSearch"}
	if len(got) != len(want) {
		t.Fatalf("disallowedTools = %v, want %v", msg["disallowedTools"], want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("disallowedTools[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if strings.Contains(w.stderr.String(), "ignoring unknown arg --disallowedTools") {
		t.Errorf("wrapper did not recognize --disallowedTools: %s", w.stderr.String())
	}
}

// TestWrapperSettingsFlagsReachSDKOptions pins the two settings flags: the
// source list becomes options.settingSources, which the SDK passes on only when
// it is set (unset loads every source, the operator's included), and --settings
// becomes an object, which the SDK serializes for Claude Code rather than
// reading the string as a file path.
func TestWrapperSettingsFlagsReachSDKOptions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, optionsEchoStubSDK,
		"--setting-sources", "project",
		"--settings", `{"apiKeyHelper":"/usr/local/bin/key-helper"}`)
	msg := w.next()
	if msg["type"] != "stub_options" {
		t.Fatalf("first non-ready line = %v, want stub_options", msg)
	}
	sources, _ := msg["settingSources"].([]any)
	if len(sources) != 1 || sources[0] != "project" {
		t.Errorf("settingSources = %v, want [project]", msg["settingSources"])
	}
	settings, ok := msg["settings"].(map[string]any)
	if !ok || settings["apiKeyHelper"] != "/usr/local/bin/key-helper" {
		t.Errorf("settings = %#v, want an object carrying apiKeyHelper", msg["settings"])
	}
	for _, flag := range []string{"--setting-sources", "--settings"} {
		if strings.Contains(w.stderr.String(), "ignoring unknown arg "+flag+"\n") {
			t.Errorf("wrapper did not recognize %s: %s", flag, w.stderr.String())
		}
	}

	// An empty list is a setting, not an absent one: it loads no settings file.
	none := startWrapper(t, node, optionsEchoStubSDK, "--setting-sources", "").next()
	if sources, ok := none["settingSources"].([]any); !ok || len(sources) != 0 {
		t.Errorf("settingSources for an empty flag = %#v, want []", none["settingSources"])
	}
}

// TestWrapperPermissionRequestUsesToolUseID pins the identifier the wrapper
// puts on a permission prompt: the SDK's own toolUseID for the gated call,
// emitted as tool_call_id, with the SDK's prompt copy alongside it. The reply
// is keyed by that same id, and an allow with no override echoes the original
// input back as updatedInput.
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

const permissionModeStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      yield { type: "stub_options", permission_mode: options.permissionMode }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

func TestWrapperPassesInitialPermissionMode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, permissionModeStubSDK, "--permission-mode", "auto")
	got := w.next()
	if got["permission_mode"] != "auto" {
		t.Errorf("SDK option permissionMode = %v, want auto", got["permission_mode"])
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

// toolBatchStubSDK fires the wrapper's PostToolBatch hook as the SDK does
// after a tool batch resolves, first for a subagent's batch and then for the
// agent's, and yields what each returned; null when the wrapper registered
// no hook.
const toolBatchStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      const hook = options.hooks?.PostToolBatch?.[0]?.hooks?.[0]
      if (!hook) {
        yield { type: "stub_hook", output: null }
        return
      }
      const signal = new AbortController().signal
      const sub = await hook({ hook_event_name: "PostToolBatch", tool_calls: [], agent_id: "sub-1" }, undefined, { signal })
      yield { type: "stub_subagent_hook", output: sub }
      const output = await hook({ hook_event_name: "PostToolBatch", tool_calls: [] }, undefined, { signal })
      yield { type: "stub_hook", output }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperToolBatchHookCarriesTheReply: with --tool-batch-notes the
// wrapper asks over a tool_batch for what the model reads after the agent's
// batch, and hands the reply's context back as the hook's additionalContext.
// A subagent's batch asks nothing. Without the flag no hook is registered.
func TestWrapperToolBatchHookCarriesTheReply(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	if out := startWrapper(t, node, toolBatchStubSDK).next(); out["type"] != "stub_hook" || out["output"] != nil {
		t.Errorf("without the flag the stub saw %v, want no hook", out)
	}

	w := startWrapper(t, node, toolBatchStubSDK, "--tool-batch-notes")
	sub := w.next()
	if subOut, ok := sub["output"].(map[string]any); sub["type"] != "stub_subagent_hook" || !ok || len(subOut) != 0 {
		t.Fatalf("first non-ready line = %v, want the subagent's batch answered with nothing and no tool_batch", sub)
	}
	req := w.next()
	id, _ := req["id"].(string)
	if req["type"] != "control" || req["subtype"] != "tool_batch" || id == "" {
		t.Fatalf("first non-ready line = %v, want a tool_batch with an id", req)
	}
	reply, err := json.Marshal(map[string]any{"kind": "tool_batch_reply", "id": id, "context": "the task is resolved"})
	if err != nil {
		t.Fatalf("marshal tool_batch_reply: %v", err)
	}
	if _, err := w.stdin.Write(append(reply, '\n')); err != nil {
		t.Fatalf("write tool_batch_reply: %v", err)
	}
	if res := w.next(); res["subtype"] != "tool_batch_result" || res["id"] != id || res["delivered"] != true {
		t.Errorf("after the reply the wrapper said %v, want a tool_batch_result delivered to %s", res, id)
	}
	out := w.next()
	output, _ := out["output"].(map[string]any)
	specific, _ := output["hookSpecificOutput"].(map[string]any)
	if specific["hookEventName"] != "PostToolBatch" || specific["additionalContext"] != "the task is resolved" {
		t.Errorf("hook returned %v, want the reply's context as PostToolBatch additionalContext (stderr: %s)", out, w.stderr.String())
	}
}

// toolBatchAbortStubSDK gives up its tool batch's hook, as an interrupt does,
// before any reply can arrive, then stays up so a late reply can be read.
const toolBatchAbortStubSDK = `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      const hook = options.hooks.PostToolBatch[0].hooks[0]
      const abort = new AbortController()
      const pending = hook({ hook_event_name: "PostToolBatch", tool_calls: [] }, undefined, { signal: abort.signal })
      abort.abort()
      yield { type: "stub_hook", output: await pending }
      await new Promise((resolve) => setTimeout(resolve, 20000))
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperToolBatchReplyAfterAnAbortIsReportedUndelivered: a reply that
// arrives after the hook was given up reaches nobody, and the wrapper says
// so, so its text can be offered again.
func TestWrapperToolBatchReplyAfterAnAbortIsReportedUndelivered(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, toolBatchAbortStubSDK, "--tool-batch-notes")
	req := w.next()
	id, _ := req["id"].(string)
	if req["subtype"] != "tool_batch" || id == "" {
		t.Fatalf("first non-ready line = %v, want a tool_batch with an id", req)
	}
	if out := w.next(); out["type"] != "stub_hook" {
		t.Fatalf("second line = %v, want the given-up hook's output", out)
	}
	reply, err := json.Marshal(map[string]any{"kind": "tool_batch_reply", "id": id, "context": "the task is resolved"})
	if err != nil {
		t.Fatalf("marshal tool_batch_reply: %v", err)
	}
	if _, err := w.stdin.Write(append(reply, '\n')); err != nil {
		t.Fatalf("write tool_batch_reply: %v", err)
	}
	if res := w.next(); res["subtype"] != "tool_batch_result" || res["id"] != id || res["delivered"] != false {
		t.Errorf("after the late reply the wrapper said %v, want a tool_batch_result undelivered to %s", res, id)
	}
}

// TestToolBatchUndo_RunsForTextTheModelNeverRead: a reply with text the
// wrapper reports undelivered is undone, one it reports delivered is not, and
// one it never reports on is undone when the stream ends.
func TestToolBatchUndo_RunsForTextTheModelNeverRead(t *testing.T) {
	undone := map[string]bool{}
	var next string
	l := &LiveRun{stdin: captureWriteCloser{newSyncBuffer()}, done: make(chan struct{}), toolBatch: func() ToolBatchNotes {
		id := next
		return ToolBatchNotes{Text: "note for " + id, Undo: func() { undone[id] = true }}
	}}
	for _, id := range []string{"abandoned", "delivered", "unsettled"} {
		next = id
		l.handleToolBatch(controlLine{Subtype: "tool_batch", ID: id})
	}
	for _, line := range []string{
		`{"type":"control","subtype":"tool_batch_result","id":"abandoned","delivered":false}`,
		`{"type":"control","subtype":"tool_batch_result","id":"delivered","delivered":true}`,
	} {
		ctl, ok := parseControlLine([]byte(line))
		if !ok {
			t.Fatalf("parseControlLine(%s) rejected it", line)
		}
		l.handleToolBatchResult(ctl)
	}
	l.abandonToolBatches()
	if !undone["abandoned"] || undone["delivered"] || !undone["unsettled"] {
		t.Errorf("undone = %v, want the abandoned and the unsettled batches only", undone)
	}
}

// TestHandleToolBatch_RepliesByID: the reader answers a tool_batch with the
// caller's text, keyed by the hook's id.
func TestHandleToolBatch_RepliesByID(t *testing.T) {
	stdin := newSyncBuffer()
	l := &LiveRun{stdin: captureWriteCloser{stdin}, done: make(chan struct{}), toolBatch: func() ToolBatchNotes { return ToolBatchNotes{Text: "the task is resolved"} }}

	l.handleToolBatch(controlLine{Subtype: "tool_batch", ID: "batch-1"})

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdin.String())), &resp); err != nil {
		t.Fatalf("decode tool_batch_reply %q: %v", stdin.String(), err)
	}
	if resp["kind"] != "tool_batch_reply" || resp["id"] != "batch-1" || resp["context"] != "the task is resolved" {
		t.Errorf("tool_batch_reply = %v, want the caller's text keyed by batch-1", resp)
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
func startWrapper(t *testing.T, node, stubSDK string, extraArgs ...string) *wrapperProc {
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
	args := []string{wrapper, "--input-format", "stream-json", "--permission-prompts"}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, node, args...)
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
