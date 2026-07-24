package inference

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Client is the TF-owned surface over embedded bifrost core. It always
// streams: a native-loop provider call reassembles the delta stream into one
// neutral assistant message (reasoning with signatures intact, tool calls,
// usage with cache tokens). Construct one per account and reuse it; Close
// shuts the embedded bifrost workers down.
type Client struct {
	bf     *bifrost.Bifrost
	closed atomic.Bool
}

// ErrClientClosed is returned by Stream on a nil, uninitialized, or already-
// closed client, so a misuse surfaces as an error instead of a panic.
var ErrClientClosed = errors.New("inference: client is closed or not initialized")

// New initializes an embedded bifrost client over the given account. The
// account carries the resolved per-provider credentials and model whitelist.
func New(account schemas.Account) (*Client, error) {
	if account == nil {
		return nil, fmt.Errorf("inference: New requires an account")
	}
	bf, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account,
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelWarn),
	})
	if err != nil {
		return nil, fmt.Errorf("inference: bifrost init: %w", err)
	}
	return &Client{bf: bf}, nil
}

// Close shuts down the embedded bifrost workers. Safe to call more than once
// and on a nil client; only the first call shuts bifrost down.
func (c *Client) Close() {
	if c == nil || c.bf == nil {
		return
	}
	if c.closed.CompareAndSwap(false, true) {
		c.bf.Shutdown()
	}
}

// Request is one native-loop provider call. Rows are the conversation's stored
// messages, assembled here into the wire context; SystemPrompt and Tools form
// the cacheable prefix; Effort is the single reasoning knob bifrost maps per
// model (budget tokens on 4.x, adaptive on 5+).
type Request struct {
	Provider     schemas.ModelProvider
	Model        string
	SystemPrompt string
	Rows         []domain.Message
	Tools        []schemas.ChatTool

	// Effort is the reasoning effort ("minimal" | "low" | "medium" | "high" |
	// "none"). Empty leaves reasoning at the provider default.
	Effort string
	// MaxTokens caps the completion. Zero leaves it to the provider.
	MaxTokens int
	// IncludeUndelivered folds delivered=false rows into the assembled context
	// (an injection point that wants the pending queue in-window). Off by
	// default.
	IncludeUndelivered bool
}

// Completion is a reassembled provider response: one neutral assistant
// message, plus the usage/cost inputs and terminal metadata the loop stamps
// onto the persisted row.
type Completion struct {
	// Message is the reassembled assistant message — ready for MessageToRow.
	Message schemas.ChatMessage
	// Usage is the neutral token accounting (cache tokens included), the input
	// to CostForUsage.
	Usage Usage
	// RawUsage is bifrost's full usage struct for callers that need detail the
	// neutral Usage drops.
	RawUsage *schemas.BifrostLLMUsage
	// FinishReason is the terminal stop reason ("stop" | "length" |
	// "tool_calls"), empty if the stream never reported one.
	FinishReason string
	// Model is the model the provider reported serving.
	Model string
}

// Stream runs one streaming chat completion and reassembles it. It respects
// ctx cancellation (the underlying stream is bound to it). A nil, uninitialized,
// or closed client returns ErrClientClosed rather than panicking.
func (c *Client) Stream(ctx context.Context, req Request) (*Completion, error) {
	if c == nil || c.bf == nil || c.closed.Load() {
		return nil, ErrClientClosed
	}
	breq, err := buildChatRequest(req)
	if err != nil {
		return nil, err
	}

	bfCtx, cancel := schemas.NewBifrostContextWithCancel(ctx)
	defer cancel()

	ch, berr := c.bf.ChatCompletionStreamRequest(bfCtx, breq)
	if berr != nil {
		return nil, bifrostError(berr)
	}
	return reassembleStream(ch)
}

// buildChatRequest assembles the wire request: the cacheable system prefix,
// the conversation rows (with the moving cache breakpoint), the tools, and the
// reasoning-effort knob. Streaming usage is on (bifrost defaults it true; set
// explicitly for clarity). It is a pure function of the Request — no client
// state — so golden wire tests can build a request without a live client.
func buildChatRequest(req Request) (*schemas.BifrostChatRequest, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("inference: request has no model")
	}
	if req.Provider == "" {
		return nil, fmt.Errorf("inference: request has no provider")
	}

	assembled, err := RowsToMessages(req.Rows, AssemblyOptions{IncludeUndelivered: req.IncludeUndelivered})
	if err != nil {
		return nil, err
	}

	input := make([]schemas.ChatMessage, 0, len(assembled)+1)
	if sys := withSystemCacheBreakpoint(req.SystemPrompt); sys.Role != "" {
		input = append(input, sys)
	}
	input = append(input, assembled...)

	includeUsage := true
	params := &schemas.ChatParameters{
		StreamOptions: &schemas.ChatStreamOptions{IncludeUsage: &includeUsage},
	}
	if len(req.Tools) > 0 {
		params.Tools = req.Tools
	}
	if req.Effort != "" {
		effort := req.Effort
		params.Reasoning = &schemas.ChatReasoning{Effort: &effort}
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		params.MaxCompletionTokens = &mt
	}

	return &schemas.BifrostChatRequest{
		Provider: req.Provider,
		Model:    req.Model,
		Input:    input,
		Params:   params,
	}, nil
}

// reassembleStream drains a bifrost stream into one assistant Completion. Text
// deltas concatenate; reasoning-detail deltas merge by index (text appends,
// signature/data/type take the latest non-empty — so a signature that arrives
// on the closing delta lands on the right block); tool-call deltas merge by
// index (id/name first, arguments concatenated). Usage and finish reason come
// from the terminal chunks. A mid-stream error chunk aborts with that error.
func reassembleStream(ch chan *schemas.BifrostStreamChunk) (*Completion, error) {
	var content strings.Builder
	reasoning := newReasoningAccumulator()
	tools := newToolCallAccumulator()

	var usage *schemas.BifrostLLMUsage
	var finishReason, model string

	for chunk := range ch {
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			// Drain the rest so the producer goroutine isn't left blocked.
			go drain(ch)
			return nil, bifrostError(chunk.BifrostError)
		}
		resp := chunk.BifrostChatResponse
		if resp == nil {
			continue
		}
		if resp.Model != "" {
			model = resp.Model
		}
		if resp.Usage != nil {
			usage = resp.Usage
		}
		for i := range resp.Choices {
			choice := resp.Choices[i]
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			if choice.ChatStreamResponseChoice == nil || choice.Delta == nil {
				continue
			}
			delta := choice.Delta
			if delta.Content != nil {
				content.WriteString(*delta.Content)
			}
			reasoning.add(delta)
			tools.add(delta.ToolCalls)
		}
	}

	msg := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant}
	if text := content.String(); text != "" {
		s := text
		msg.Content = &schemas.ChatMessageContent{ContentStr: &s}
	}
	details := reasoning.finalize()
	calls := tools.finalize()
	if len(details) > 0 || len(calls) > 0 {
		msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{
			ReasoningDetails: details,
			ToolCalls:        calls,
		}
	}

	return &Completion{
		Message:      msg,
		Usage:        usageFromBifrost(usage),
		RawUsage:     usage,
		FinishReason: finishReason,
		Model:        model,
	}, nil
}

// drain consumes any remaining chunks after an error so the producer goroutine
// can exit.
func drain(ch chan *schemas.BifrostStreamChunk) {
	for range ch {
	}
}

// bifrostError converts a bifrost error pointer into a Go error carrying its
// message.
func bifrostError(e *schemas.BifrostError) error {
	if e == nil {
		return nil
	}
	if msg := e.GetErrorString(); msg != "" {
		return fmt.Errorf("inference: provider error: %s", msg)
	}
	return fmt.Errorf("inference: provider error")
}
