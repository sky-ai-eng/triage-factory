package delegate

import (
	"encoding/json"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// streamState tracks the current assistant message being accumulated
// across multiple NDJSON lines (thinking → text → tool_use all share one msg ID).
type streamState struct {
	currentMsgID string
	current      *domain.AgentMessage
	sessionID    string // captured from the system/init event at stream start
}

func newStreamState() *streamState {
	return &streamState{}
}

// SessionID returns the Claude Code session_id captured from the stream's
// `system/init` event, or empty if that event hasn't been seen yet.
// Used by the spawner to persist the id on agent_runs so later `--resume`
// invocations (write-gate retry, SKY-139 yield) can attach to the session.
func (s *streamState) SessionID() string { return s.sessionID }

// flush returns the accumulated assistant message (if any) and resets state.
func (s *streamState) flush() *domain.AgentMessage {
	msg := s.current
	s.current = nil
	s.currentMsgID = ""
	return msg
}

// parseLine processes one NDJSON line from claude's stream-json output.
// Returns messages ready to store and an optional run completion signal.
func (s *streamState) parseLine(line []byte, runID string) ([]*domain.AgentMessage, *runCompletion) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, nil
	}

	lineType, _ := raw["type"].(string)

	switch lineType {
	case "system":
		// system/init carries session_id we need for --resume. Other system
		// subtypes are ignored — they're metadata for the harness, not
		// content the spawner needs to persist.
		if subtype, _ := raw["subtype"].(string); subtype == "init" {
			if sid, ok := raw["session_id"].(string); ok {
				s.sessionID = sid
			}
		}
		return nil, nil

	case "assistant":
		return s.handleAssistant(raw, runID), nil

	case "user":
		// Tool result — flush any pending assistant message first
		var out []*domain.AgentMessage
		if flushed := s.flush(); flushed != nil {
			out = append(out, flushed)
		}
		if msg := parseToolResult(raw, runID); msg != nil {
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

func (s *streamState) handleAssistant(raw map[string]any, runID string) []*domain.AgentMessage {
	msgObj, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}

	msgID, _ := msgObj["id"].(string)
	if msgID == "" {
		return nil
	}

	// If this is a new message ID, flush the previous one
	var flushed []*domain.AgentMessage
	if msgID != s.currentMsgID && s.current != nil {
		flushed = append(flushed, s.flush())
	}

	// Initialize if needed
	if s.current == nil {
		model, _ := msgObj["model"].(string)
		s.currentMsgID = msgID
		s.current = &domain.AgentMessage{
			RunID:   runID,
			Role:    "assistant",
			Subtype: "text",
			Model:   model,
		}
	}

	// Extract token usage (take latest — each line repeats cumulative usage)
	if usage, ok := msgObj["usage"].(map[string]any); ok {
		s.current.InputTokens = intPtr(usage, "input_tokens")
		s.current.OutputTokens = intPtr(usage, "output_tokens")
		s.current.CacheReadTokens = intPtr(usage, "cache_read_input_tokens")
		s.current.CacheCreationTokens = intPtr(usage, "cache_creation_input_tokens")
	}

	// Process content blocks
	contentBlocks, _ := msgObj["content"].([]any)
	for _, block := range contentBlocks {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}

		switch b["type"] {
		case "thinking":
			// Skip thinking content — too verbose to store

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

	// Check if this message is complete (stop_reason present = final turn)
	if stopReason, _ := msgObj["stop_reason"].(string); stopReason != "" {
		if msg := s.flush(); msg != nil {
			flushed = append(flushed, msg)
		}
	}

	return flushed
}

func parseToolResult(raw map[string]any, runID string) *domain.AgentMessage {
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

	// Fallback to top-level convenience field
	if content == "" {
		if r, ok := raw["tool_use_result"].(string); ok {
			content = r
		}
	}

	return &domain.AgentMessage{
		RunID:      runID,
		Role:       "tool",
		Subtype:    "tool",
		Content:    content,
		ToolCallID: toolUseID,
		IsError:    isError,
	}
}

type runCompletion struct {
	IsError    bool
	DurationMs int
	NumTurns   int
	CostUSD    float64
	StopReason string
	Result     string
}

func parseResult(raw map[string]any) *runCompletion {
	rc := &runCompletion{}
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
	return rc
}

func intPtr(m map[string]any, key string) *int {
	if v, ok := m[key].(float64); ok {
		i := int(v)
		return &i
	}
	return nil
}
