// Package agentproc invokes a headless `claude -p` subprocess and turns
// its stream-json output into structured messages + a terminal Result.
// It is the storage-neutral half of the runtime: it knows how to talk
// to Claude Code and parse what comes back, but not where to put the
// results. Callers wire it up with a Sink that decides persistence
// (delegate writes to runs / messages; the curator runtime
// writes to its own tables).
package agentproc

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// StreamState tracks the current assistant message being accumulated
// across multiple NDJSON lines (thinking → text → tool_use all share
// one msg ID). Thinking rides the same message as reasoning entries
// rather than flushing as its own row.
//
// It also holds the timing marks each row's duration_ms is measured from.
// This process watches a live stream, so both ends of every interval are
// moments it observed directly — no row ever gets its duration by
// subtracting a neighbour's created_at.
type StreamState struct {
	currentMsgID string
	current      *domain.Message
	sessionID    string // captured from the system/init event at stream start

	// currentBlocks accumulates the pending message's text/image content
	// blocks (never tool_use or thinking — those land on ToolCalls and
	// Reasoning respectively) in stream order. Promoted onto
	// current.ContentBlocks at flush only when there's more than one block
	// or a non-text block, matching the single-text-block message's
	// existing flat-Content-only shape.
	currentBlocks []domain.ContentBlock

	// now is the wall clock the marks below are read from.
	now func() time.Time

	// requestAt is when the request producing the next assistant message
	// went out: stream start, the end of the previous assistant message, the
	// arrival of the tool results that unblocked the model, or a user
	// message sent into a live run. Mutex-guarded because MarkRequest is
	// called from the caller's send goroutine while ParseLine reads it on
	// the reader goroutine; a time.Time (not unix nanos) so the interval
	// keeps its monotonic reading.
	markMu    sync.Mutex
	requestAt time.Time

	// toolDispatchedAt is when each pending tool_use was handed to the
	// harness, keyed by tool_use id and consumed by the matching result. A
	// call that never resolves (an interrupted turn) leaves its mark behind;
	// the map is bounded by the tool calls of one process's stream.
	toolDispatchedAt map[string]time.Time
}

// NewStreamState returns a fresh state ready for ParseLine, with the
// first request mark set to now — for the one-shot path that is the
// prompt going out with the subprocess's argv, and for a live run the
// first Send re-marks it more precisely.
func NewStreamState() *StreamState {
	s := &StreamState{
		now:              time.Now,
		toolDispatchedAt: map[string]time.Time{},
	}
	s.MarkRequest()
	return s
}

// MarkRequest records that a request to the model has just gone out, so
// the assistant message answering it is measured from here. The live path
// calls this when it sends a user message: without it a follow-up's first
// assistant row would count the whole parked wait as thinking time.
func (s *StreamState) MarkRequest() {
	at := s.now()
	s.markMu.Lock()
	defer s.markMu.Unlock()
	s.requestAt = at
}

// sinceRequest returns the milliseconds elapsed since the last request
// mark, or nil when there is no mark to measure from — nil is "not
// measured", which every consumer must render as absent rather than zero.
func (s *StreamState) sinceRequest() *int {
	s.markMu.Lock()
	at := s.requestAt
	s.markMu.Unlock()
	if at.IsZero() {
		return nil
	}
	ms := int(s.now().Sub(at).Milliseconds())
	return &ms
}

// SessionID returns the Claude Code session_id captured from the stream's
// `system/init` event, or empty if that event hasn't been seen yet.
// Callers persist this once it surfaces so later `--resume` invocations
// can attach to the session.
func (s *StreamState) SessionID() string { return s.sessionID }

// flush returns the accumulated assistant message (if any) and resets state.
// A flushed message ends its own interval, so the next one is measured from
// here rather than from the same mark all over again.
func (s *StreamState) flush() *domain.Message {
	msg := s.current
	if msg != nil && len(s.currentBlocks) > 0 {
		nonText := len(s.currentBlocks) > 1 || s.currentBlocks[0].Type != domain.ContentBlockText
		if nonText {
			msg.ContentBlocks = s.currentBlocks
		}
	}
	s.current = nil
	s.currentMsgID = ""
	s.currentBlocks = nil
	if msg != nil {
		s.MarkRequest()
	}
	return msg
}

// ParseLine processes one NDJSON line from claude's stream-json output.
// Returns messages ready to store and an optional terminal Result.
//
// traceID is stamped onto every emitted message's RunID field — the
// caller's choice of identifier (delegate runs use the agent run ID;
// the curator wires its own message-group ID through). Storage
// decisions live in the Sink, not here.
func (s *StreamState) ParseLine(line []byte, traceID string) ([]*domain.Message, *Result) {
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
		// Tool result(s) — flush any pending assistant message first. A
		// single "user" line can carry multiple tool_result blocks (parallel
		// tool calls from one assistant turn all resolve together), so this
		// walks the whole content array rather than just its first entry.
		var out []*domain.Message
		if flushed := s.flush(); flushed != nil {
			out = append(out, flushed)
		}
		out = append(out, s.parseToolResult(raw, traceID)...)
		// Results going back is the next request going out.
		s.MarkRequest()
		return out, nil

	case "result":
		var out []*domain.Message
		if flushed := s.flush(); flushed != nil {
			out = append(out, flushed)
		}
		return out, parseResult(raw)
	}

	return nil, nil
}

func (s *StreamState) handleAssistant(raw map[string]any, traceID string) []*domain.Message {
	msgObj, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}

	msgID, _ := msgObj["id"].(string)
	if msgID == "" {
		return nil
	}

	var flushed []*domain.Message
	if msgID != s.currentMsgID && s.current != nil {
		flushed = append(flushed, s.flush())
	}

	if s.current == nil {
		model, _ := msgObj["model"].(string)
		s.currentMsgID = msgID
		s.current = &domain.Message{
			ConversationID: traceID,
			Role:           "assistant",
			Subtype:        "text",
			Model:          model,
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
			// Reasoning accumulates onto the pending assistant message in
			// stream order rather than flushing as its own row — the
			// assistant row carries the full chain-of-thought alongside the
			// text/tool_use siblings it belongs to.
			text, _ := b["thinking"].(string)
			signature, _ := b["signature"].(string)
			s.current.Reasoning = append(s.current.Reasoning, domain.ReasoningDetail{
				Index:     len(s.current.Reasoning),
				Type:      "text",
				Text:      text,
				Signature: signature,
			})

		case "text":
			// Multiple text blocks concatenate (newline-joined) rather than
			// the last one winning; the full block list rides ContentBlocks
			// (promoted at flush) whenever there's more than one.
			text, _ := b["text"].(string)
			if s.current.Content != "" && text != "" {
				s.current.Content += "\n"
			}
			s.current.Content += text
			s.currentBlocks = append(s.currentBlocks, domain.ContentBlock{
				Type: domain.ContentBlockText,
				Text: text,
			})

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
			// The harness runs the call as soon as the block lands, so this
			// is the dispatch its result is measured against.
			if toolID != "" {
				s.toolDispatchedAt[toolID] = s.now()
			}
		}
	}

	// Re-stamped on every line of the same message so the value settles on
	// the arrival of its last one.
	s.current.DurationMs = s.sinceRequest()

	if stopReason, _ := msgObj["stop_reason"].(string); stopReason != "" {
		if msg := s.flush(); msg != nil {
			flushed = append(flushed, msg)
		}
	}

	return flushed
}

// parseToolResult walks every tool_result block in a "user" line's content
// array — a single line can carry multiple parallel tool results, one per
// tool_use_id from the preceding assistant turn — and returns one message per
// block. Each block's own content is walked in turn: text sub-blocks flatten
// into the display Content string, non-text sub-blocks (images) land in
// ContentBlocks so they aren't silently dropped.
//
// Each result is timed against its own dispatch mark, so parallel calls
// resolving in one line still get the durations they individually earned.
// A result whose dispatch this process never saw (a resumed session's
// in-flight call) carries no duration rather than a made-up one.
func (s *StreamState) parseToolResult(raw map[string]any, traceID string) []*domain.Message {
	msgObj, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}

	contentBlocks, _ := msgObj["content"].([]any)
	var toolResults []map[string]any
	for _, block := range contentBlocks {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "tool_result" {
			continue
		}
		toolResults = append(toolResults, b)
	}

	var out []*domain.Message
	for _, b := range toolResults {
		toolUseID, _ := b["tool_use_id"].(string)
		isError, _ := b["is_error"].(bool)
		content, blocks := flattenToolResultContent(b["content"])

		// The legacy fallback only applies unambiguously when this line
		// carries exactly one tool_result — a single top-level string can't
		// be attributed to one of several parallel results.
		if content == "" && len(toolResults) == 1 {
			if r, ok := raw["tool_use_result"].(string); ok {
				content = r
			}
		}

		var durationMs *int
		if at, ok := s.toolDispatchedAt[toolUseID]; ok {
			ms := int(s.now().Sub(at).Milliseconds())
			durationMs = &ms
			delete(s.toolDispatchedAt, toolUseID)
		}

		out = append(out, &domain.Message{
			ConversationID: traceID,
			Role:           "tool",
			Subtype:        "tool",
			Content:        content,
			ToolCallID:     toolUseID,
			IsError:        isError,
			ContentBlocks:  blocks,
			DurationMs:     durationMs,
		})
	}
	return out
}

// flattenToolResultContent normalizes a tool_result block's `content` field,
// which arrives as either a plain string or an array of content blocks
// (text/image). Text blocks flatten into the display string (newline-
// joined, matching the plain-string case); image blocks are preserved in
// full as ContentBlocks so results like screenshots or a Read on an image
// aren't silently dropped.
func flattenToolResultContent(raw any) (string, []domain.ContentBlock) {
	if s, ok := raw.(string); ok {
		return s, nil
	}

	blocks, ok := raw.([]any)
	if !ok {
		return "", nil
	}

	var sb strings.Builder
	var out []domain.ContentBlock
	for _, bl := range blocks {
		m, ok := bl.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			t, _ := m["text"].(string)
			if t == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(t)
		case "image":
			if cb, ok := parseImageBlock(m); ok {
				out = append(out, cb)
			}
		}
	}
	return sb.String(), out
}

// parseImageBlock converts an Anthropic-shaped image content block
// ({"type":"image","source":{...}}) into the neutral domain.ContentBlock
// shape, supporting both the base64 source form (data: URI) and the url
// source form.
func parseImageBlock(m map[string]any) (domain.ContentBlock, bool) {
	source, ok := m["source"].(map[string]any)
	if !ok {
		return domain.ContentBlock{}, false
	}

	switch source["type"] {
	case "base64":
		data, _ := source["data"].(string)
		if data == "" {
			return domain.ContentBlock{}, false
		}
		mediaType, _ := source["media_type"].(string)
		if mediaType == "" {
			mediaType = "image/png"
		}
		return domain.ContentBlock{
			Type:     domain.ContentBlockImage,
			ImageURL: &domain.ContentImageURL{URL: "data:" + mediaType + ";base64," + data},
		}, true
	case "url":
		url, _ := source["url"].(string)
		if url == "" {
			return domain.ContentBlock{}, false
		}
		return domain.ContentBlock{
			Type:     domain.ContentBlockImage,
			ImageURL: &domain.ContentImageURL{URL: url},
		}, true
	}
	return domain.ContentBlock{}, false
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
