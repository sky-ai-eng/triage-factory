package agentloop

import (
	"context"
	"fmt"

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
func (e *Engine) persistAssistant(ctx context.Context, spec Spec, completion *inference.Completion) (domain.Message, error) {
	row, err := inference.MessageToRow(completion.Message)
	if err != nil {
		return domain.Message{}, fmt.Errorf("map completion to row: %w", err)
	}
	row.ConversationID = spec.ConversationID
	row.Model = modelForRow(completion, spec)
	stampUsage(&row, completion.Usage)

	// Cost settles per assistant row on this path — the ledger's stamp
	// density is the only thing that differs between runtimes, and
	// SUM(messages.cost_usd) is the total either way. An unpriceable model
	// leaves the column NULL: 0 means genuinely free, and claiming a run was
	// free is worse than admitting the price is unknown.
	if cost, ok := inference.CostForUsage(row.Model, completion.Usage); ok {
		row.CostUSD = &cost
	}

	id, err := e.Transcript.Insert(ctx, spec.OrgID, &row)
	if err != nil {
		return domain.Message{}, err
	}
	row.ID = id
	return row, nil
}

// modelForRow prefers the model the provider reported serving over the one
// requested: an inference profile or an alias can resolve to a concrete id,
// and the ledger should record what was actually billed.
func modelForRow(completion *inference.Completion, spec Spec) string {
	if completion.Model != "" {
		return completion.Model
	}
	return spec.Model
}

func stampUsage(row *domain.Message, usage inference.Usage) {
	in, out := usage.InputTokens, usage.OutputTokens
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
func (e *Engine) insertToolResult(ctx context.Context, spec Spec, call domain.ToolCall, content string, isErr bool) error {
	row := &domain.Message{
		ConversationID: spec.ConversationID,
		Role:           "tool",
		Subtype:        "tool",
		ToolCallID:     call.ID,
		Content:        content,
		IsError:        isErr,
	}
	id, err := e.Transcript.Insert(ctx, spec.OrgID, row)
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
func (e *Engine) insertToolResultWithImages(ctx context.Context, spec Spec, call domain.ToolCall, content string, images []ToolImage) error {
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
		ConversationID: spec.ConversationID,
		Role:           "tool",
		Subtype:        "tool",
		ToolCallID:     call.ID,
		Content:        content,
		ContentBlocks:  blocks,
	}
	id, err := e.Transcript.Insert(ctx, spec.OrgID, row)
	if err != nil {
		return fmt.Errorf("insert tool result for %s: %w", call.ID, err)
	}
	row.ID = id
	return nil
}
