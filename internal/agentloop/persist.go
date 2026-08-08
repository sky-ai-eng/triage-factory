package agentloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// persistAssistant writes one row per completed provider call — message
// granularity, matching the neutral message the bijection round-trips.
//
// Every display column is populated here, not just the assembly-relevant
// ones: the frontend reads one shape and never branches on
// conversations.runtime, so a native row that skipped (say) token counts
// would render as a degraded SDK row rather than an equivalent one.
func (e *Engine) persistAssistant(ctx context.Context, params Params, completion *inference.Completion, durationMs int) (domain.Message, error) {
	row, err := inference.MessageToRow(completion.Message)
	if err != nil {
		return domain.Message{}, fmt.Errorf("map completion to row: %w", err)
	}
	row.Content = sanitizeForStore(row.Content)
	for i := range row.ToolCalls {
		sanitizeInputForStore(row.ToolCalls[i].Input)
	}
	row.ConversationID = params.ConversationID
	row.Model = modelForRow(completion, params)
	stampUsage(&row, completion.Usage)
	stampFinishReason(&row, completion.FinishReason)
	if unreplayableTurn(row) {
		row.WindowState = domain.MessageWindowInactive
	}
	// Wall time from request to complete message, retries included — the
	// same request-to-row span the SDK parser stamps.
	if durationMs > 0 {
		row.DurationMs = &durationMs
	}

	// Cost settles per assistant row on this path — the ledger's stamp
	// density is the only thing that differs between runtimes, and
	// SUM(messages.cost_usd) is the total either way. An unpriceable model
	// leaves the column NULL: 0 means genuinely free, and claiming a run was
	// free is worse than admitting the price is unknown.
	if cost, ok := inference.CostForUsage(row.Model, completion.Usage); ok {
		row.CostUSD = &cost
	}

	id, err := e.Transcript.Insert(ctx, params.OrgID, &row)
	if err != nil {
		return domain.Message{}, err
	}
	row.ID = id
	return row, nil
}

// modelForRow prefers the model the provider reported serving over the one
// requested: an inference profile or an alias can resolve to a concrete id,
// and the ledger should record what was actually billed.
func modelForRow(completion *inference.Completion, params Params) string {
	if completion.Model != "" {
		return completion.Model
	}
	return params.Model
}

// metadataFinishReason is the metadata key carrying the provider's terminal
// stop reason for an assistant row.
const metadataFinishReason = "finish_reason"

// stampFinishReason records why the provider stopped generating. It is an
// observability fact, not an assembly input, which is why it rides metadata
// rather than a column — but it is the fact an investigation needs first, and
// without it a truncated turn has to be inferred from an output-token count
// that happens to equal a cap. An empty reason (the stream reported none) is
// left absent rather than stored as "", so "unknown" and "not reported" stay
// the same thing.
func stampFinishReason(row *domain.Message, reason string) {
	if reason == "" {
		return
	}
	if row.Metadata == nil {
		row.Metadata = map[string]any{}
	}
	row.Metadata[metadataFinishReason] = reason
}

// unreplayableTurn reports an assistant row with nothing a later request
// could replay — no text, no tool call, no content block. Reasoning alone
// does not count: the provider keeps the canonical chain of thought and
// strips non-final blocks on replay, so what reaches the wire from such a row
// is an empty assistant message, which the API rejects.
//
// Such a row is born inactive rather than flipped a moment later, because
// there is no moment in between where it would be legal: any assembly that
// saw it active would 400. Inactive keeps it in the ledger and in the display
// (its cost and tokens are as real as any other call's) while keeping it out
// of every future request.
//
// Deliberately blind to the stop reason. What makes the row unreplayable is
// that nothing came out of it, not what stopped it: a turn whose whole output
// cap went to reasoning is the common producer, but a wall-cut turn is one
// too — and that one is read by the summarize call moments later — and so is
// a decline that returns no content, which would otherwise brick the very
// park it earns.
func unreplayableTurn(row domain.Message) bool {
	return row.Content == "" && len(row.ToolCalls) == 0 && len(row.ContentBlocks) == 0
}

// stampUsage writes the row's four token columns as disjoint counts — the
// neutral usage counts the prompt inclusive of its cache buckets, the ledger
// counts them beside it. Everything that reads these columns back sums all
// four to recover the prompt: the compaction trip's occupancy, the context
// gauge, the approximate-cost footer. Storing the inclusive figure here would
// count every cached token twice in all three.
func stampUsage(row *domain.Message, usage inference.Usage) {
	in, out := usage.NonCachedInputTokens(), usage.OutputTokens
	cr, cc := usage.CacheReadTokens, usage.CacheCreationTokens
	row.InputTokens = &in
	row.OutputTokens = &out
	row.CacheReadTokens = &cr
	row.CacheCreationTokens = &cc
}

// insertToolResult appends one tool-result row. Every result reaches the
// transcript through here — a real dispatch, a synthetic repair, a gate
// denial, a truncated batch — so a tool_use always has exactly one matching
// row and its is_error flag is set the same way regardless of origin.
func (e *Engine) insertToolResult(ctx context.Context, params Params, call domain.ToolCall, content string, isErr bool) error {
	row := &domain.Message{
		ConversationID: params.ConversationID,
		Role:           "tool",
		ToolCallID:     call.ID,
		Content:        sanitizeForStore(content),
		IsError:        isErr,
	}
	id, err := e.Transcript.Insert(ctx, params.OrgID, row)
	if err != nil {
		return fmt.Errorf("insert tool result for %s: %w", call.ID, err)
	}
	row.ID = id
	return nil
}

// insertToolResultWithImages is insertToolResult for a tool that returned
// non-text content (the read tool on an image). The text stays in the flat
// Content column so the display path is unchanged, and the image rides
// ContentBlocks, which is what the assembly bijection replays to the
// provider.
func (e *Engine) insertToolResultWithImages(ctx context.Context, params Params, call domain.ToolCall, content string, images []ToolImage) error {
	blocks := make([]domain.ContentBlock, 0, len(images))
	for _, img := range images {
		blocks = append(blocks, domain.ContentBlock{
			Type: domain.ContentBlockImage,
			ImageURL: &domain.ContentImageURL{
				// Data URI: the neutral image block carries a URL, and a
				// base64 payload is expressed as one. The provider adapters
				// split it back out.
				URL: "data:" + img.MimeType + ";base64," + img.Data,
			},
		})
	}
	row := &domain.Message{
		ConversationID: params.ConversationID,
		Role:           "tool",
		ToolCallID:     call.ID,
		Content:        sanitizeForStore(content),
		ContentBlocks:  blocks,
	}
	id, err := e.Transcript.Insert(ctx, params.OrgID, row)
	if err != nil {
		return fmt.Errorf("insert tool result for %s: %w", call.ID, err)
	}
	row.ID = id
	return nil
}

// sanitizeForStore strips NUL from text bound for the messages table.
// Postgres cannot represent it — TEXT rejects the raw byte, JSONB rejects
// the escaped form — so one NUL fails the whole row insert, which is how a
// live engagement died when a bash call catted a binary. NUL is valid UTF-8,
// so the jail's lossy decode passes it through untouched; representability
// in the store is this layer's problem, not the decoder's.
func sanitizeForStore(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// sanitizeInputForStore applies sanitizeForStore to every string in a tool
// call's decoded arguments, in place. The tool_calls column is JSON, so the
// same NUL that kills a content insert kills the assistant row carrying the
// call — and sanitizing at persist, before dispatch reads the same struct,
// keeps what ran, what was stored, and what later assemblies replay
// byte-identical.
func sanitizeInputForStore(input map[string]any) {
	for k, v := range input {
		input[k] = sanitizeValueForStore(v)
	}
}

func sanitizeValueForStore(v any) any {
	switch t := v.(type) {
	case string:
		return sanitizeForStore(t)
	case map[string]any:
		sanitizeInputForStore(t)
		return t
	case []any:
		for i := range t {
			t[i] = sanitizeValueForStore(t[i])
		}
		return t
	default:
		return v
	}
}
