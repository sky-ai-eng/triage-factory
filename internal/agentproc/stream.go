// Package agentproc invokes a headless `claude -p` subprocess and turns
// its stream-json output into structured messages + a terminal Result.
// It is the storage-neutral half of the runtime: it knows how to talk
// to Claude Code and parse what comes back, but not where to put the
// results. Callers wire it up with a Sink that decides persistence
// (delegate writes to runs / run_messages; the curator runtime in
// SKY-216 writes to its own tables).
package agentproc

import (
	"encoding/json"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StreamState tracks the current assistant message being accumulated
// across multiple NDJSON lines (thinking → text → tool_use all share
// one msg ID).
type StreamState struct {
	currentMsgID string
	current      *domain.AgentMessage
	sessionID    string // captured from the system/init event at stream start
}

// NewStreamState returns a fresh state ready for ParseLine.
func NewStreamState() *StreamState {
	return &StreamState{}
}

// SessionID returns the Claude Code session_id captured from the stream's
// `system/init` event, or empty if that event hasn't been seen yet.
// Callers persist this once it surfaces so later `--resume` invocations
// can attach to the session.
func (s *StreamState) SessionID() string { return s.sessionID }

// flush returns the accumulated assistant message (if any) and resets state.
func (s *StreamState) flush() *domain.AgentMessage {
	msg := s.current
	s.current = nil
	s.currentMsgID = ""
	return msg
}

// ParseLine processes one NDJSON line from claude's stream-json output.
// Returns messages ready to store and an optional terminal Result.
//
// traceID is stamped onto every emitted message's RunID field — the
// caller's choice of identifier (delegate runs use the agent run ID;
// the curator wires its own message-group ID through). Storage
// decisions live in the Sink, not here.
func (s *StreamState) ParseLine(line []byte, traceID string) ([]*domain.AgentMessage, *Result) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, nil
	}

	lineType, _ := raw["type"].(string)

	switch lineType {
	case "system":
		// system/init carries session_id we need for --resume. Other
		// system subtypes are ignored — they're metadata for the
		// harness, not content the consumer needs to persist.
		if subtype, _ := raw["subtype"].(string); subtype == "init" {
			if sid, ok := raw["session_id"].(string); ok {
				s.sessionID = sid
			}
		}
		return nil, nil

	case "assistant":
		return s.handleAssistant(raw, traceID), nil

	case "user":
		// Tool result — flush any pending assistant message first.
		var out []*domain.AgentMessage
		if flushed := s.flush(); flushed != nil {
			out = append(out, flushed)
		}
		if msg := parseToolResult(raw, traceID); msg != nil {
			out = append(out, msg)
		}
		return out, nil

	case "result":
		var out []*domain.AgentMessage
		if flushed := s.flush(); flushed != nil {
			out = append(out, flushed)
		}
		return out, parseResult(raw)
	}

	return nil, nil
}

func (s *StreamState) handleAssistant(raw map[string]any, traceID string) []*domain.AgentMessage {
	msgObj, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}

	msgID, _ := msgObj["id"].(string)
	if msgID == "" {
		return nil
	}

	var flushed []*domain.AgentMessage
	if msgID != s.currentMsgID && s.current != nil {
		flushed = append(flushed, s.flush())
	}

	if s.current == nil {
		model, _ := msgObj["model"].(string)
		s.currentMsgID = msgID
		s.current = &domain.AgentMessage{
			RunID:   traceID,
			Role:    "assistant",
			Subtype: "text",
			Model:   model,
		}
	}

	if usage, ok := msgObj["usage"].(map[string]any); ok {
		s.current.InputTokens = intPtr(usage, "input_tokens")
		s.current.OutputTokens = intPtr(usage, "output_tokens")
		s.current.CacheReadTokens = intPtr(usage, "cache_read_input_tokens")
		s.current.CacheCreationTokens = intPtr(usage, "cache_creation_input_tokens")
	}

	contentBlocks, _ := msgObj["content"].([]any)
	for _, block := range contentBlocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}

		switch b["type"] {
		case "thinking":
			// Reasoning surfaces as its own message row, emitted immediately
			// so it lands ahead of the text/tool_use siblings that accumulate
			// on s.current (same assistant message, flushed later) — matching
			// the order the blocks were produced in.
			if t, _ := b["thinking"].(string); t != "" {
				flushed = append(flushed, &domain.AgentMessage{
					RunID:   traceID,
					Role:    "assistant",
					Subtype: "thinking",
					Content: t,
					Model:   s.current.Model,
				})
			}

		case "text":
			text, _ := b["text"].(string)
			s.current.Content = text

		case "tool_use":
			toolName, _ := b["name"].(string)
			toolID, _ := b["id"].(string)
			toolInput, _ := b["input"].(map[string]any)
			s.current.Subtype = "tool_use"
			s.current.ToolCalls = append(s.current.ToolCalls, domain.ToolCall{
				ID:    toolID,
				Name:  toolName,
				Input: toolInput,
			})
		}
	}

	if stopReason, _ := msgObj["stop_reason"].(string); stopReason != "" {
		if msg := s.flush(); msg != nil {
			flushed = append(flushed, msg)
		}
	}

	return flushed
}

func parseToolResult(raw map[string]any, traceID string) *domain.AgentMessage {
	msgObj, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}

	contentBlocks, _ := msgObj["content"].([]any)
	if len(contentBlocks) == 0 {
		return nil
	}

	b, ok := contentBlocks[0].(map[string]any)
	if !ok {
		return nil
	}

	if b["type"] != "tool_result" {
		return nil
	}

	content, _ := b["content"].(string)
	toolUseID, _ := b["tool_use_id"].(string)
	isError, _ := b["is_error"].(bool)

	// tool_result content arrives either as a plain string or as a block
	// array ([{type:"text",text:...}, ...]); flatten the text blocks so the
	// array form doesn't store an empty result.
	if content == "" {
		if blocks, ok := b["content"].([]any); ok {
			var sb strings.Builder
			for _, bl := range blocks {
				m, ok := bl.(map[string]any)
				if !ok || m["type"] != "text" {
					continue
				}
				if t, _ := m["text"].(string); t != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			}
			content = sb.String()
		}
	}

	if content == "" {
		if r, ok := raw["tool_use_result"].(string); ok {
			content = r
		}
	}

	return &domain.AgentMessage{
		RunID:      traceID,
		Role:       "tool",
		Subtype:    "tool",
		Content:    content,
		ToolCallID: toolUseID,
		IsError:    isError,
	}
}

// Result is the terminal `result` event from a claude -p stream:
// final accounting (cost, duration, turn count) plus the agent's
// last message text and stop reason.
type Result struct {
	IsError    bool
	DurationMs int
	NumTurns   int
	CostUSD    float64
	StopReason string
	Result     string

	// Subtype mirrors the SDK result event's `subtype` field
	// ("success", "error_during_execution", "error_max_turns", ...).
	// The one-shot path doesn't inspect it, but the interactive reader
	// uses "error_during_execution" to corroborate that a turn ended
	// because the bridge called interrupt().
	Subtype string

	// Interrupted marks a turn that ended by interruption rather than
	// completion. Primary source: the result's terminal_reason field
	// ("aborted_streaming" / "aborted_tools") — the SDK's native marker;
	// an interrupted turn deliberately rides an is_error /
	// error_during_execution result, shape-identical to a runtime error,
	// and terminal_reason is what distinguishes it. The interactive
	// reader additionally sets this when the wrapper acks
	// control/interrupted ahead of the result, corroborating the same
	// signal for any path that omits terminal_reason. Consumers (the
	// delegate driver) treat an interrupted turn as a pause, not a
	// failure.
	Interrupted bool
}

func parseResult(raw map[string]any) *Result {
	rc := &Result{}
	rc.IsError, _ = raw["is_error"].(bool)
	if d, ok := raw["duration_ms"].(float64); ok {
		rc.DurationMs = int(d)
	}
	if n, ok := raw["num_turns"].(float64); ok {
		rc.NumTurns = int(n)
	}
	if c, ok := raw["total_cost_usd"].(float64); ok {
		rc.CostUSD = c
	}
	rc.StopReason, _ = raw["stop_reason"].(string)
	rc.Result, _ = raw["result"].(string)
	rc.Subtype, _ = raw["subtype"].(string)
	if tr, _ := raw["terminal_reason"].(string); tr == "aborted_streaming" || tr == "aborted_tools" {
		rc.Interrupted = true
	}
	return rc
}

func intPtr(m map[string]any, key string) *int {
	if v, ok := m[key].(float64); ok {
		i := int(v)
		return &i
	}
	return nil
}
