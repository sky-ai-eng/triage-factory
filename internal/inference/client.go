package inference

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	bf *bifrost.Bifrost
	// endpoints is the configured base URL per provider, captured at
	// construction so a failed call can say which host it was talking to.
	// A provider mapped to "" was enumerable but left on its built-in
	// endpoint; a provider absent from the map entirely was never
	// enumerable (endpointsOf's best-effort read failed for it), so
	// annotateEndpoint adds nothing rather than guessing which case it is.
	endpoints map[schemas.ModelProvider]string
	closed    atomic.Bool
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
		Logger:  newBifrostLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("inference: bifrost init: %w", err)
	}
	return &Client{bf: bf, endpoints: endpointsOf(account)}, nil
}

// endpointsOf reads each configured provider's base URL off the account.
// Best-effort: an account that declines to enumerate itself just yields no
// annotation, since this exists only to make an error self-explaining.
func endpointsOf(account schemas.Account) map[schemas.ModelProvider]string {
	providers, err := account.GetConfiguredProviders()
	if err != nil {
		return nil
	}
	out := make(map[schemas.ModelProvider]string, len(providers))
	for _, p := range providers {
		cfg, err := account.GetConfigForProvider(p)
		if err != nil || cfg == nil {
			continue
		}
		out[p] = cfg.NetworkConfig.BaseURL
	}
	return out
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
	// MaxTokens caps the completion. Zero resolves the per-provider budget
	// policy (MaxOutputTokens) against this request's provider and model —
	// it never leaves the cap to the provider layer, whose fallback is a
	// small constant a thinking-heavy turn spends entirely on reasoning.
	MaxTokens int
	// Temperature pins the sampling temperature. nil leaves the provider
	// default — which is why it is a pointer: a caller that wants greedy
	// decoding sends 0, and that is not the same request as sending nothing.
	Temperature *float64
	// ToolChoice pins the provider's tool policy for this call (a forced
	// single-tool call). nil leaves the provider default (auto), which is
	// what every conversational call sends — changing tool_choice
	// invalidates the provider's messages cache, so only an out-of-band call
	// that has already forfeited the cache may set this.
	ToolChoice *schemas.ChatToolChoice
	// IncludeUndelivered folds delivered=false rows into the assembled context
	// (an injection point that wants the pending queue in-window). Off by
	// default.
	IncludeUndelivered bool

	// NoConversationCacheBreakpoint suppresses the moving breakpoint on the
	// last message. The system-prefix breakpoint is unaffected.
	//
	// It exists for a single-turn call whose tail is never sent again — a
	// system job's one synthetic user row carrying that call's own data. A
	// breakpoint there buys a cache write premium on tokens no later request
	// can match, so the default (on, for a conversation that grows by
	// appending) is a pure loss for a conversation that does not.
	NoConversationCacheBreakpoint bool
}

// Completion is a reassembled provider response: one neutral assistant
// message, plus the usage/cost inputs and terminal metadata the loop stamps
// onto the persisted row.
type Completion struct {
	// ID is the provider's own id for this response (Anthropic's message id).
	// Empty when the provider reported none — it is a correlation handle, not
	// something to key on without checking.
	ID string
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
		return nil, c.wrapProviderError(req.Provider, berr)
	}
	completion, err := reassembleStream(ch)
	if err != nil {
		return nil, c.annotateEndpoint(req.Provider, err)
	}
	return completion, nil
}

// wrapProviderError renders a bifrost error and names the endpoint it was
// aimed at.
func (c *Client) wrapProviderError(provider schemas.ModelProvider, e *schemas.BifrostError) error {
	return c.annotateEndpoint(provider, bifrostError(e))
}

// annotateEndpoint appends the base URL the call was aimed at. Which host
// answered (or didn't) is the first thing worth knowing about a transport
// failure — a run whose credentials point at its own sidecar proxy fails very
// differently from one that reached the public provider — and bifrost's error
// carries no endpoint of its own.
func (c *Client) annotateEndpoint(provider schemas.ModelProvider, err error) error {
	if err == nil {
		return nil
	}
	base, ok := c.endpoints[provider]
	if !ok {
		return err
	}
	if base == "" {
		return fmt.Errorf("%w [endpoint: %s built-in]", err, provider)
	}
	return fmt.Errorf("%w [endpoint: %s]", err, redactURL(base))
}

// redactURL strips userinfo and query from a base URL before it goes into an
// error string. A customer gateway URL is operator-supplied and may carry a
// token in either position; scheme + host + path is what identifies the hop.
// The path is deliberately kept: nothing in the provider-base-URL vocabulary
// puts a secret there, and a gateway's route prefix is often the one detail
// that distinguishes two hops on the same host.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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

	assembled, err := RowsToMessages(req.Rows, AssemblyOptions{
		IncludeUndelivered: req.IncludeUndelivered,
		NoCacheBreakpoint:  req.NoConversationCacheBreakpoint,
	})
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
	if req.ToolChoice != nil {
		params.ToolChoice = req.ToolChoice
	}
	if req.Effort != "" {
		effort := req.Effort
		params.Reasoning = &schemas.ChatReasoning{Effort: &effort}
	}
	// Always sent, never omitted. Anthropic requires max_tokens, so an
	// omitted cap is filled downstream by a provider-layer default that knows
	// nothing about the model or the account — and a turn that spends that
	// default on reasoning returns no text and no tool calls at all. This is
	// the structural half of that guarantee: a caller that resolves its own
	// cap wins, and one that doesn't still gets the budget policy rather than
	// somebody else's constant.
	mt := req.MaxTokens
	if mt <= 0 {
		mt = MaxOutputTokens(req.Provider, req.Model)
	}
	params.MaxCompletionTokens = &mt
	if req.Temperature != nil {
		temp := *req.Temperature
		params.Temperature = &temp
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
	var finishReason, model, id string

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
		if resp.ID != "" {
			id = resp.ID
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
		ID:           id,
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

// bifrostError converts a bifrost error pointer into a Go error.
//
// It renders the wrapped cause and the status code alongside bifrost's own
// message, because that message is frequently a fixed constant — every
// transport failure in every provider reports "failed to execute HTTP request
// to provider API" — while the cause underneath it holds the dial error, the
// TLS failure, or the reset that actually happened. Dropping it costs twice:
// the operator gets an error with no lead to follow, and the caller's
// transient-vs-permanent classification (agentloop's isTransient) sees no
// "connection refused" or "502" to match on, so a retryable network blip is
// treated as a permanent failure and ends the engagement on the first attempt.
func bifrostError(e *schemas.BifrostError) error {
	if e == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("inference: provider error")
	if msg := e.GetErrorString(); msg != "" {
		b.WriteString(": " + msg)
	}
	if e.Error != nil && e.Error.Error != nil {
		if cause := e.Error.Error.Error(); cause != "" && cause != e.GetErrorString() {
			b.WriteString(": " + cause)
		}
	}
	if e.StatusCode != nil {
		fmt.Fprintf(&b, " (HTTP %d)", *e.StatusCode)
	}
	if e.Error != nil && e.Error.Type != nil && *e.Error.Type != "" {
		fmt.Fprintf(&b, " [%s]", *e.Error.Type)
	}
	if isContextOverflowMessage(e.StatusCode, b.String()) {
		return fmt.Errorf("%w: %s", ErrContextOverflow, b.String())
	}
	return errors.New(b.String())
}

// ErrContextOverflow classifies a provider rejection for context length: the
// assembled prompt (plus max_tokens) does not fit the model's window. It is a
// distinct class because the two callers must treat it opposite ways — the
// retry classifier must NOT retry it (the same request can never succeed),
// and the agent loop's compaction arm treats it as a trigger, not a failure.
var ErrContextOverflow = errors.New("inference: context window exceeded")

// contextOverflowMarkers are the provider spellings of the overflow class.
// Anthropic says "prompt is too long: N tokens > M maximum" or "input length
// and `max_tokens` exceed context limit"; OpenAI-compatible providers say
// "context_length_exceeded" (error code) or "maximum context length".
var contextOverflowMarkers = []string{
	"prompt is too long",
	"context_length_exceeded",
	"maximum context length",
	"input length and `max_tokens` exceed",
}

// isContextOverflowMessage matches the flattened provider error against the
// overflow class. The status gate keeps a marker string quoted inside some
// other failure (a 500 whose body echoes a prior request) from
// classifying: overflow is an invalid-request rejection, so any status other
// than 400 disqualifies. A nil status (a mid-stream error chunk with no HTTP
// code attached) falls through to the markers alone.
func isContextOverflowMessage(status *int, msg string) bool {
	if status != nil && *status != 400 {
		return false
	}
	lower := strings.ToLower(msg)
	for _, m := range contextOverflowMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
