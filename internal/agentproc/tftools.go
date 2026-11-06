package agentproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
)

// tfToolPrefix is the name an SDK run's model calls a TF tool by, ahead of
// the tool's own name: the wrapper serves TF tools from an MCP server named
// "tf", and Claude Code names an MCP tool mcp__<server>__<tool>.
const tfToolPrefix = "mcp__tf__"

// tfToolCallTimeout bounds one TF tool call on an SDK run. A TF tool is TF's
// own work, answered in this process, so a call still running at this point
// is stuck rather than slow; the model is told it timed out and the turn goes
// on.
const tfToolCallTimeout = 2 * time.Minute

// storedToolName is the name a tool_use block is stored under: a TF tool's own
// name, without the MCP prefix, so its rows read the same in both runtimes.
// Every other name, another MCP server's included, is stored as the SDK
// reported it.
func storedToolName(name string) string {
	if bare, ok := strings.CutPrefix(name, tfToolPrefix); ok && bare != "" {
		return bare
	}
	return name
}

// tfToolSpec is one entry of --tf-tools.
type tfToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// tfToolsFlag renders --tf-tools: each tool's name, description and argument
// schema as plain JSON Schema, which the wrapper converts to the zod shape the
// SDK builds an MCP tool from. It cannot fail, for the reason JSONSchema
// cannot: every field is a string or a document that already marshaled.
func tfToolsFlag(tools []tooldefs.Tool) string {
	specs := make([]tfToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, tfToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.Params.JSONSchema()})
	}
	b, err := json.Marshal(specs)
	if err != nil {
		panic(fmt.Sprintf("agentproc: render --tf-tools: %v", err))
	}
	return string(b)
}

// withTFToolGrants adds each TF tool to the --allowedTools value, under the
// MCP name the model calls it by. TF answers the call itself, so a call that
// prompted would wait on a person for nothing. Granting them here, beside
// --tf-tools, means a served tool is always a granted one.
func withTFToolGrants(allowed string, tools []tooldefs.Tool) string {
	grants := make([]string, 0, len(tools)+1)
	if allowed != "" {
		grants = append(grants, allowed)
	}
	for _, t := range tools {
		grants = append(grants, tfToolPrefix+t.Name)
	}
	return strings.Join(grants, ",")
}

// tfToolsByName indexes the run's TF tools for the reader. A tool with no
// handler is left out: nothing could answer it.
func tfToolsByName(tools []tooldefs.Tool) map[string]tooldefs.Tool {
	if len(tools) == 0 {
		return nil
	}
	byName := make(map[string]tooldefs.Tool, len(tools))
	for _, t := range tools {
		if t.Handler != nil {
			byName[t.Name] = t
		}
	}
	return byName
}

// handleToolCall answers one tool_call: it runs the named tool's handler on
// its own goroutine and replies through writeControl, keyed by the wrapper's
// id. The reader never waits for it. A permission prompt parks the reader on
// purpose, because the turn is waiting on a person; a TF tool is the agent's
// own work, and the stream it is part of keeps moving while it runs.
//
// The handler's context ends when the run does, when the wrapper cancels the
// call, or at tfToolCallTimeout. A call cancelled by the wrapper gets no
// reply: the wrapper has already settled it.
func (l *LiveRun) handleToolCall(ctl controlLine) {
	tool, ok := l.tfTools[ctl.ToolName]
	if !ok {
		l.replyToolCall(ctl.ID, fmt.Sprintf("%s is not a tool this run was offered.", ctl.ToolName), true)
		return
	}

	timeout := l.toolCallTimeout
	if timeout <= 0 {
		timeout = tfToolCallTimeout
	}
	ctx, cancel := context.WithTimeout(l.toolCallBase(), timeout)
	l.toolCallsMu.Lock()
	if l.toolCalls == nil {
		l.toolCalls = map[string]context.CancelFunc{}
	}
	l.toolCalls[ctl.ID] = cancel
	l.toolCallsMu.Unlock()

	call := tooldefs.Call{ID: ctl.ToolCallID, Args: ctl.Input, Run: l.tfRun}
	go func() {
		defer func() {
			l.toolCallsMu.Lock()
			delete(l.toolCalls, ctl.ID)
			l.toolCallsMu.Unlock()
			cancel()
		}()
		// The handler runs apart from the wait, so one that ignores its
		// context still gets its call answered at the deadline.
		answered := make(chan tooldefs.Outcome, 1)
		go func() { answered <- tool.Handler(ctx, call) }()
		select {
		case out := <-answered:
			if errors.Is(ctx.Err(), context.Canceled) {
				// Cancelled as it returned: the wrapper settled the call,
				// or the run ended, and nobody reads the answer.
				return
			}
			if out.Terminal != "" {
				l.recordTerminal(ctl.ToolCallID, Terminal{Outcome: out.Terminal, Reason: out.Reason, Summary: out.Summary})
			}
			l.replyToolCall(ctl.ID, out.Content, out.IsError)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				l.replyToolCall(ctl.ID, fmt.Sprintf("%s did not answer within %s, so its result was abandoned. The work it started may still complete.", ctl.ToolName, timeout), true)
			}
		}
	}()
}

// recordTerminal keeps the ending one answered call recorded, for the
// tool_batch that names it. A call with no tool_use id is never named by one,
// so its ending is not kept: the batch it belongs to then cannot stop, and the
// run goes on rather than ending on a call nothing can attribute. That only
// happens if the SDK stops sending the id, so it is logged.
func (l *LiveRun) recordTerminal(toolUseID string, t Terminal) {
	if toolUseID == "" {
		agentprocLog.Warn("tf tool ended the run but its call carried no tool_use id; the run goes on", "outcome", t.Outcome)
		return
	}
	l.toolCallsMu.Lock()
	defer l.toolCallsMu.Unlock()
	if l.terminals == nil {
		l.terminals = map[string]Terminal{}
	}
	l.terminals[toolUseID] = t
}

// batchTerminal applies the rule the native loop's dispatchBatch does: a batch
// ends the run only when it is a single call that recorded an ending. A model
// that pairs stop_blueprint with real work in one message gets the work done
// and keeps going. held reports that some call in the batch recorded an
// ending, whether or not the batch ends the run.
//
// Every id the batch names is consumed whatever the answer, because an ending
// in a batch that did not stop is discarded, as it is natively.
func (l *LiveRun) batchTerminal(toolUseIDs []string) (t Terminal, ends, held bool) {
	l.toolCallsMu.Lock()
	defer l.toolCallsMu.Unlock()
	for _, id := range toolUseIDs {
		if rec, ok := l.terminals[id]; ok {
			t, held = rec, true
		}
		delete(l.terminals, id)
	}
	return t, held && len(toolUseIDs) == 1, held
}

// cancelToolCall ends the handler of one call the wrapper gave up on: the
// turn it belonged to was interrupted.
func (l *LiveRun) cancelToolCall(id string) {
	l.toolCallsMu.Lock()
	cancel, ok := l.toolCalls[id]
	l.toolCallsMu.Unlock()
	if ok {
		cancel()
	}
}

// cancelToolCalls ends every handler still running, once the stream has
// ended and nothing is left to read a reply.
func (l *LiveRun) cancelToolCalls() {
	l.toolCallsMu.Lock()
	defer l.toolCallsMu.Unlock()
	for _, cancel := range l.toolCalls {
		cancel()
	}
}

// toolCallBase is the context every handler's derives from: the run's.
func (l *LiveRun) toolCallBase() context.Context {
	if l.runCtx != nil {
		return l.runCtx
	}
	return context.Background()
}

func (l *LiveRun) replyToolCall(id, content string, isError bool) {
	if err := l.writeControl(map[string]any{"kind": "tool_call_reply", "id": id, "content": content, "is_error": isError}); err != nil {
		agentprocLog.Warn("tf tool reply write failed", "error", err)
	}
}
