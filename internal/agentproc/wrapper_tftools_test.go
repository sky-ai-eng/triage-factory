package agentproc

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// tfToolsStubZod stands in for zod: fromJSONSchema hands back the schema it
// was given as the shape, so a test can see what the wrapper converted, and
// importing the module at all leaves a mark the stub SDK reports.
const tfToolsStubZod = `
globalThis.__stubZodImported = true
export function fromJSONSchema(schema) {
  return { shape: { fromSchema: schema } }
}
`

// tfToolsStubSDKHelpers are the SDK's MCP helpers, reduced to recording what
// they were called with.
const tfToolsStubSDKHelpers = `
export function tool(name, description, inputSchema, handler, extras) {
  return { name, description, inputSchema, handler, extras }
}
export function createSdkMcpServer(opts) {
  return { type: "sdk", name: opts.name, alwaysLoad: opts.alwaysLoad, tools: opts.tools }
}
`

// tfToolsStubSDK is a stub SDK whose query runs body, an async generator body
// with options in scope; handler is the first TF tool's.
func tfToolsStubSDK(body string) string {
	return tfToolsStubSDKHelpers + `
export function query({ options }) {
  return {
    async *[Symbol.asyncIterator]() {
      const handler = options.mcpServers?.tf?.tools?.[0]?.handler
      ` + body + `
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`
}

const tfToolsFlagValue = `[` +
	`{"name":"record_call","description":"records a call","input_schema":{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}},` +
	`{"name":"second","description":"another","input_schema":{"type":"object"}}]`

func startTFToolsWrapper(t *testing.T, body string, args ...string) *wrapperProc {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	return startWrapperWith(t, node, map[string]string{
		"@anthropic-ai/claude-agent-sdk": tfToolsStubSDK(body),
		"zod":                            tfToolsStubZod,
	}, args...)
}

func (w *wrapperProc) send(t *testing.T, ctl map[string]any) {
	t.Helper()
	line, err := json.Marshal(ctl)
	if err != nil {
		t.Fatalf("marshal %v: %v", ctl, err)
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		t.Fatalf("write %s: %v", line, err)
	}
}

// TestWrapperTFToolsBecomeAnMCPServer: with --tf-tools the query is handed an
// MCP server named tf holding every tool, each built from its converted JSON
// Schema and loaded up front rather than deferred behind tool search. Without
// the flag there is no mcpServers option at all, and zod is never imported.
func TestWrapperTFToolsBecomeAnMCPServer(t *testing.T) {
	const report = `
      const servers = options.mcpServers
      yield {
        type: "stub_options",
        zodImported: Boolean(globalThis.__stubZodImported),
        mcpServers: servers === undefined ? null : Object.fromEntries(Object.entries(servers).map(([key, s]) => [key, {
          type: s.type,
          name: s.name,
          alwaysLoad: s.alwaysLoad,
          tools: s.tools.map((t) => ({
            name: t.name,
            description: t.description,
            inputSchema: t.inputSchema,
            alwaysLoad: t.extras?.alwaysLoad,
            hasHandler: typeof t.handler === "function",
          })),
        }])),
      }`

	without := startTFToolsWrapper(t, report).next()
	if without["mcpServers"] != nil || without["zodImported"] != false {
		t.Errorf("without --tf-tools the stub saw %v, want no mcpServers and no zod import", without)
	}

	msg := startTFToolsWrapper(t, report, "--tf-tools", tfToolsFlagValue).next()
	servers, _ := msg["mcpServers"].(map[string]any)
	tf, _ := servers["tf"].(map[string]any)
	if len(servers) != 1 || tf["type"] != "sdk" || tf["name"] != "tf" || tf["alwaysLoad"] != true {
		t.Fatalf("mcpServers = %v, want one always-loaded sdk server named tf", msg["mcpServers"])
	}
	tools, _ := tf["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tf tools = %v, want both tools", tf["tools"])
	}
	for i, want := range []string{"record_call", "second"} {
		tool, _ := tools[i].(map[string]any)
		if tool["name"] != want || tool["alwaysLoad"] != true || tool["hasHandler"] != true {
			t.Errorf("tool %d = %v, want %s, always loaded, with a handler", i, tool, want)
		}
	}
	first, _ := tools[0].(map[string]any)
	shape, _ := first["inputSchema"].(map[string]any)
	schema, _ := shape["fromSchema"].(map[string]any)
	if first["description"] != "records a call" || schema["type"] != "object" || schema["properties"] == nil {
		t.Errorf("record_call = %v, want its description and the shape zod made from its schema", first)
	}
}

// TestWrapperTFToolCallIsAnsweredByTheReply: a handler call emits a tool_call
// carrying the call's tool_use id from the SDK's _meta, and the reply with
// the same id settles it with that content and error flag.
func TestWrapperTFToolCallIsAnsweredByTheReply(t *testing.T) {
	w := startTFToolsWrapper(t, `
      const result = await handler({ note: "hi" }, {
        _meta: { "claudecode/toolUseId": "toolu_01FIXTURE" },
        signal: new AbortController().signal,
      })
      yield { type: "stub_result", result }`, "--tf-tools", tfToolsFlagValue)

	call := w.next()
	id, _ := call["id"].(string)
	if call["subtype"] != "tool_call" || id == "" {
		t.Fatalf("first line = %v, want a tool_call with an id", call)
	}
	input, _ := call["input"].(map[string]any)
	if call["tool_call_id"] != "toolu_01FIXTURE" || call["tool_name"] != "record_call" || input["note"] != "hi" {
		t.Errorf("tool_call = %v, want the SDK's tool_use id, the tool's own name and its input", call)
	}

	w.send(t, map[string]any{"kind": "tool_call_reply", "id": id, "content": "recorded", "is_error": true})
	got := w.next()
	result, _ := got["result"].(map[string]any)
	content, _ := result["content"].([]any)
	block, _ := firstOf(content).(map[string]any)
	if got["type"] != "stub_result" || result["isError"] != true || block["type"] != "text" || block["text"] != "recorded" {
		t.Errorf("handler settled with %v, want the reply's text as an error result (stderr: %s)", got, w.stderr.String())
	}
}

// TestWrapperTFToolCallAbortSettlesAsAnError: an aborted call is settled as an
// error at once and Go is told to stop its handler.
func TestWrapperTFToolCallAbortSettlesAsAnError(t *testing.T) {
	w := startTFToolsWrapper(t, `
      const abort = new AbortController()
      const pending = handler({ note: "hi" }, { _meta: { "claudecode/toolUseId": "toolu_01" }, signal: abort.signal })
      abort.abort()
      yield { type: "stub_result", result: await pending }`, "--tf-tools", tfToolsFlagValue)

	call := w.next()
	id, _ := call["id"].(string)
	if call["subtype"] != "tool_call" || id == "" {
		t.Fatalf("first line = %v, want a tool_call", call)
	}
	if cancel := w.next(); cancel["subtype"] != "tool_call_cancel" || cancel["id"] != id {
		t.Fatalf("after the abort the wrapper said %v, want a tool_call_cancel for %s", cancel, id)
	}
	got := w.next()
	result, _ := got["result"].(map[string]any)
	block, _ := firstOf(result["content"]).(map[string]any)
	if result["isError"] != true || block["text"] != "interrupted" {
		t.Errorf("aborted call settled with %v, want an interrupted error", got)
	}
}

// TestWrapperTFToolCallsAreSettledAtEnd: ending the input settles every call
// still waiting, so none strands the query's drain.
func TestWrapperTFToolCallsAreSettledAtEnd(t *testing.T) {
	w := startTFToolsWrapper(t, `
      const meta = { _meta: { "claudecode/toolUseId": "toolu_01" } }
      const results = await Promise.all([handler({ note: "a" }, meta), handler({ note: "b" }, meta)])
      yield { type: "stub_results", results }`, "--tf-tools", tfToolsFlagValue)

	for i := 0; i < 2; i++ {
		if call := w.next(); call["subtype"] != "tool_call" {
			t.Fatalf("line %d = %v, want a tool_call", i, call)
		}
	}
	w.send(t, map[string]any{"kind": "end"})
	got := w.next()
	results, _ := got["results"].([]any)
	if got["type"] != "stub_results" || len(results) != 2 {
		t.Fatalf("after end the stub saw %v, want both calls settled", got)
	}
	for i, r := range results {
		result, _ := r.(map[string]any)
		block, _ := firstOf(result["content"]).(map[string]any)
		if result["isError"] != true || block["text"] != "run ended" {
			t.Errorf("call %d settled with %v, want a run-ended error", i, result)
		}
	}
}

func firstOf(v any) any {
	if list, ok := v.([]any); ok && len(list) > 0 {
		return list[0]
	}
	return nil
}

// TestWrapperTFToolsBatchStopEndsTheTurn: --tf-tools alone wires the
// PostToolBatch hook, whose tool_batch names the batch's calls by tool_use id.
// A reply with stop returns continue: false, which ends the turn at the batch;
// one with context alone hands the context to the model. A call with no
// tool_use id is still named, as "", so Go counts it as a call that did not
// end the run.
func TestWrapperTFToolsBatchStopEndsTheTurn(t *testing.T) {
	w := startTFToolsWrapper(t, `
      const hook = options.hooks?.PostToolBatch?.[0]?.hooks?.[0]
      if (!hook) {
        yield { type: "stub_hook", output: null }
        return
      }
      const signal = new AbortController().signal
      const batch = (...ids) => ({ hook_event_name: "PostToolBatch", tool_calls: ids.map((id) => ({ tool_name: "mcp__tf__record_call", tool_input: {}, tool_use_id: id })) })
      yield { type: "stub_hook", output: await hook(batch("toolu_A", "toolu_B"), undefined, { signal }) }
      const idless = batch("toolu_C")
      idless.tool_calls.push({ tool_name: "Bash", tool_input: {} })
      yield { type: "stub_hook", output: await hook(idless, undefined, { signal }) }`, "--tf-tools", tfToolsFlagValue)

	req := w.next()
	id, _ := req["id"].(string)
	ids, _ := req["tool_call_ids"].([]any)
	if req["subtype"] != "tool_batch" || id == "" || len(ids) != 2 || ids[0] != "toolu_A" || ids[1] != "toolu_B" {
		t.Fatalf("first line = %v, want a tool_batch naming toolu_A and toolu_B", req)
	}
	w.send(t, map[string]any{"kind": "tool_batch_reply", "id": id, "stop": true})
	out, _ := w.next()["output"].(map[string]any)
	if out["continue"] != false || out["stopReason"] == nil || out["hookSpecificOutput"] != nil {
		t.Fatalf("hook returned %v on a stop, want continue: false with a stop reason (stderr: %s)", out, w.stderr.String())
	}

	req = w.next()
	id, _ = req["id"].(string)
	if ids, _ := req["tool_call_ids"].([]any); len(ids) != 2 || ids[0] != "toolu_C" || ids[1] != "" {
		t.Fatalf("second tool_batch = %v, want toolu_C and the id-less call as \"\"", req)
	}
	w.send(t, map[string]any{"kind": "tool_batch_reply", "id": id, "context": "a note"})
	if res := w.next(); res["subtype"] != "tool_batch_result" || res["delivered"] != true {
		t.Fatalf("after a reply with context the wrapper said %v, want it delivered", res)
	}
	out, _ = w.next()["output"].(map[string]any)
	specific, _ := out["hookSpecificOutput"].(map[string]any)
	if _, stopped := out["continue"]; stopped || specific["additionalContext"] != "a note" {
		t.Errorf("hook returned %v without a stop, want only the reply's context", out)
	}
}
