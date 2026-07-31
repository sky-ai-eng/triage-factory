package inference

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// elidedStubText is the fixed content a window_state='elided' row renders as.
// Elision drops a row's bulk content at a cold moment but keeps the transcript
// replayable — the role, a tool row's tool_call_id/is_error, and an assistant
// row's tool_calls survive so nothing downstream is orphaned; everything else
// collapses to this deterministic marker.
const elidedStubText = "[elided]"

// AssemblyOptions tunes RowsToMessages. The zero value is the request-building
// default: undelivered rows excluded, the moving cache breakpoint applied.
type AssemblyOptions struct {
	// IncludeUndelivered keeps delivered=false rows in the assembled context.
	// Off by default: pending input (a queued follow-up, a staged injection)
	// is not part of context until the loop's injection point folds it in.
	IncludeUndelivered bool

	// NoCacheBreakpoint suppresses the moving conversation cache breakpoint on
	// the final message's last block. Off by default so a normal request gets
	// the breakpoint; tests that assert pure content assembly set it.
	NoCacheBreakpoint bool
}

// RowsToMessages assembles a conversation's stored rows into the bifrost
// ChatMessage list a provider call sends. It is a pure function of its input:
// it never reads the database or any process state.
//
// The rows are sorted by their effective assembly key COALESCE(seq, id) — the
// same order the messages store reads them in — so a compaction result placed
// at a fractional seq lands between the two rows it belongs between. Rows in
// window_state 'inactive' (superseded by compaction) are dropped; 'elided'
// rows render as the deterministic stub; undelivered rows are excluded unless
// IncludeUndelivered is set. cache_control is written here and only here
// (AssemblyOptions.NoCacheBreakpoint opts out): the moving conversation
// breakpoint lands on the last block of the final message.
func RowsToMessages(rows []domain.Message, opts AssemblyOptions) ([]schemas.ChatMessage, error) {
	sorted := make([]domain.Message, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		return effectiveKey(sorted[i]) < effectiveKey(sorted[j])
	})

	out := make([]schemas.ChatMessage, 0, len(sorted))
	for i := range sorted {
		r := sorted[i]
		if r.WindowState == domain.MessageWindowInactive {
			continue
		}
		if !isDelivered(r) && !opts.IncludeUndelivered {
			continue
		}
		if r.WindowState == domain.MessageWindowElided {
			out = append(out, elidedStub(r))
			continue
		}
		m, err := rowToMessage(r)
		if err != nil {
			return nil, fmt.Errorf("assemble message (row %d): %w", r.ID, err)
		}
		out = append(out, m)
	}

	out = consolidateAdjacentUsers(out)

	if !opts.NoCacheBreakpoint {
		applyMovingCacheBreakpoint(out)
	}
	return out, nil
}

// consolidateAdjacentUsers merges each run of two or more adjacent
// `role=user` messages into one message carrying their content blocks in
// order. Persistence and display stay row-granular — this is a packing step
// on the way to the wire, run over the assembled list and nowhere else.
//
// It exists because bifrost's Anthropic packer groups consecutive TOOL
// messages into one user turn but passes adjacent plain-user messages
// through unmerged, and the Anthropic API rejects (or degrades on)
// back-to-back user turns. A drain that flushes a follow-up next to a
// steer, or an injection landing beside the delegation prompt, produces
// exactly that shape.
//
// Two properties the loop depends on:
//
//   - Deterministic: the output is a pure function of the input order, so
//     two executors assembling the same rows build the same request.
//   - Append-stable: a newly drained row can only extend the final run, so
//     every message before the last user run keeps its exact packed form
//     and the cached prefix survives. This is also why it runs BEFORE the
//     moving cache breakpoint is stamped — the breakpoint must land on the
//     merged message's last block, not on a block that a later merge moves.
//
// A run of one is returned untouched (a lone string-content user message
// keeps its string form, so the common case is byte-identical on the wire).
func consolidateAdjacentUsers(msgs []schemas.ChatMessage) []schemas.ChatMessage {
	out := make([]schemas.ChatMessage, 0, len(msgs))
	for i := 0; i < len(msgs); {
		if msgs[i].Role != schemas.ChatMessageRoleUser {
			out = append(out, msgs[i])
			i++
			continue
		}
		j := i
		for j < len(msgs) && msgs[j].Role == schemas.ChatMessageRoleUser {
			j++
		}
		if j-i == 1 {
			out = append(out, msgs[i])
			i = j
			continue
		}
		var blocks []schemas.ChatContentBlock
		for _, m := range msgs[i:j] {
			blocks = append(blocks, contentAsBlocks(m.Content)...)
		}
		merged := schemas.ChatMessage{Role: schemas.ChatMessageRoleUser}
		if len(blocks) > 0 {
			merged.Content = &schemas.ChatMessageContent{ContentBlocks: blocks}
		}
		out = append(out, merged)
		i = j
	}
	return out
}

// contentAsBlocks normalizes a message's content union to a block slice so
// consolidation can concatenate across the two representations. String
// content becomes one text block; nil content contributes nothing.
func contentAsBlocks(c *schemas.ChatMessageContent) []schemas.ChatContentBlock {
	if c == nil {
		return nil
	}
	if c.ContentStr != nil {
		if *c.ContentStr == "" {
			return nil
		}
		text := *c.ContentStr
		return []schemas.ChatContentBlock{{
			Type: schemas.ChatContentBlockTypeText,
			Text: &text,
		}}
	}
	return append([]schemas.ChatContentBlock(nil), c.ContentBlocks...)
}

// effectiveKey is COALESCE(seq, id) as a float so a fractional seq interleaves
// with integer ids.
func effectiveKey(m domain.Message) float64 {
	if m.Seq != nil {
		return *m.Seq
	}
	return float64(m.ID)
}

// isDelivered mirrors the schema default: a nil Delivered means the row is
// delivered (the overwhelmingly common case), only an explicit false is
// pending.
func isDelivered(m domain.Message) bool {
	return m.Delivered == nil || *m.Delivered
}

// rowToMessage is the pure per-row half of the bijection: one domain row to
// one bifrost ChatMessage, carrying content, reasoning (signatures intact),
// tool calls, tool-result linkage and the is_error marker. It writes no
// cache_control and never elides — RowsToMessages layers those on. Its exact
// inverse is MessageToRow.
func rowToMessage(r domain.Message) (schemas.ChatMessage, error) {
	content, err := rowContent(r)
	if err != nil {
		return schemas.ChatMessage{}, err
	}

	switch r.Role {
	case "user":
		return schemas.ChatMessage{Role: schemas.ChatMessageRoleUser, Content: wrapSteer(r, content)}, nil

	case "tool":
		return schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleTool,
			Content: content,
			ChatToolMessage: &schemas.ChatToolMessage{
				ToolCallID: strPtr(r.ToolCallID),
				IsError:    isErrorPtr(r.IsError),
			},
		}, nil

	case "assistant":
		msg := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: content}
		asst := assistantPayload(r)
		if asst != nil {
			msg.ChatAssistantMessage = asst
		}
		return msg, nil

	default:
		return schemas.ChatMessage{}, fmt.Errorf("unsupported role %q", r.Role)
	}
}

// assistantPayload builds the embedded assistant struct from a row's reasoning
// and tool calls, or nil when the row is plain assistant text (so a bare text
// turn marshals without an empty assistant object).
func assistantPayload(r domain.Message) *schemas.ChatAssistantMessage {
	details := reasoningToSchema(r.Reasoning)
	calls, _ := toolCallsToSchema(r.ToolCalls)
	if len(details) == 0 && len(calls) == 0 {
		return nil
	}
	return &schemas.ChatAssistantMessage{
		ReasoningDetails: details,
		ToolCalls:        calls,
	}
}

// MessageToRow is the exact inverse of rowToMessage: one bifrost ChatMessage
// to one domain row, carrying the same content/reasoning/tool-call/is_error
// facts back. It populates only the assembly-relevant fields — the caller
// stamps identity (conversation, claim, user), the model, token usage and
// cost. Subtype stays blank: every shape here is normal behavior for its
// role, already fully described by the role and tool-call columns.
func MessageToRow(msg schemas.ChatMessage) (domain.Message, error) {
	content, blocks := contentToRow(msg.Content)

	row := domain.Message{
		Role:          string(msg.Role),
		Content:       content,
		ContentBlocks: blocks,
	}

	switch msg.Role {
	case schemas.ChatMessageRoleUser:

	case schemas.ChatMessageRoleTool:
		if msg.ChatToolMessage != nil {
			row.ToolCallID = derefStr(msg.ToolCallID)
			row.IsError = derefBool(msg.IsError)
		}

	case schemas.ChatMessageRoleAssistant:
		if msg.ChatAssistantMessage != nil {
			row.Reasoning = reasoningToDomain(msg.ReasoningDetails)
			calls, err := toolCallsToDomain(msg.ToolCalls)
			if err != nil {
				return domain.Message{}, err
			}
			row.ToolCalls = calls
		}

	default:
		return domain.Message{}, fmt.Errorf("unsupported role %q", msg.Role)
	}
	return row, nil
}

// steerOpen / steerClose bracket a row that was drained mid-turn — input
// that arrived while the model was working. The tag tells the model the
// message is not a fresh instruction replacing what it was doing, which is
// how an unwrapped user turn between tool batches would otherwise read.
const (
	steerOpen  = "<steer note=\"sent while you were working; keep going and respond if appropriate\">\n"
	steerClose = "\n</steer>"
)

// wrapSteer brackets an `injection:steer` row's content. It is a function of
// the row's own columns and nothing else — the loop stamps the subtype at
// flush time and assembly reads it back, so two executors assembling the
// same rows produce the same request. A steer row with no content is left
// alone: an empty envelope would be pure noise.
func wrapSteer(r domain.Message, content *schemas.ChatMessageContent) *schemas.ChatMessageContent {
	if r.Subtype != domain.MessageSubtypeInjectionSteer || content == nil {
		return content
	}
	if content.ContentStr != nil {
		if *content.ContentStr == "" {
			return content
		}
		wrapped := steerOpen + *content.ContentStr + steerClose
		return &schemas.ChatMessageContent{ContentStr: &wrapped}
	}
	if len(content.ContentBlocks) == 0 {
		return content
	}
	open, closing := steerOpen, steerClose
	blocks := make([]schemas.ChatContentBlock, 0, len(content.ContentBlocks)+2)
	blocks = append(blocks, schemas.ChatContentBlock{Type: schemas.ChatContentBlockTypeText, Text: &open})
	blocks = append(blocks, content.ContentBlocks...)
	blocks = append(blocks, schemas.ChatContentBlock{Type: schemas.ChatContentBlockTypeText, Text: &closing})
	return &schemas.ChatMessageContent{ContentBlocks: blocks}
}

// rowContent maps a row's content into the bifrost content union. The row's
// canonical shape keeps text authoritative in flat Content (so the display
// column is always populated) and reserves ContentBlocks for non-text content
// (an image or file, e.g. an image tool result). A row with no blocks carries
// its text as a plain string; a row with blocks carries a leading text block
// (when it has text) followed by the non-text blocks, so nothing is lost.
func rowContent(r domain.Message) (*schemas.ChatMessageContent, error) {
	if len(r.ContentBlocks) == 0 {
		if r.Content != "" {
			s := r.Content
			return &schemas.ChatMessageContent{ContentStr: &s}, nil
		}
		return nil, nil
	}

	blocks := make([]schemas.ChatContentBlock, 0, len(r.ContentBlocks)+1)
	if r.Content != "" {
		text := r.Content
		blocks = append(blocks, schemas.ChatContentBlock{
			Type: schemas.ChatContentBlockTypeText,
			Text: &text,
		})
	}
	mapped, err := contentBlocksToSchema(r.ContentBlocks)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, mapped...)
	return &schemas.ChatMessageContent{ContentBlocks: blocks}, nil
}

// contentToRow is rowContent's inverse: the string arm becomes flat Content;
// the block arm splits back into flat Content (text blocks, newline-joined) and
// ContentBlocks (the non-text blocks), reconstructing the canonical row shape.
func contentToRow(c *schemas.ChatMessageContent) (string, []domain.ContentBlock) {
	if c == nil {
		return "", nil
	}
	if c.ContentStr != nil {
		return *c.ContentStr, nil
	}
	if len(c.ContentBlocks) == 0 {
		return "", nil
	}

	var text []string
	var blocks []domain.ContentBlock
	for _, b := range c.ContentBlocks {
		if b.Type == schemas.ChatContentBlockTypeText {
			text = append(text, derefStr(b.Text))
			continue
		}
		blocks = append(blocks, contentBlocksToDomain([]schemas.ChatContentBlock{b})...)
	}
	return strings.Join(text, "\n"), blocks
}

// elidedStub renders a window_state='elided' row: a deterministic marker that
// keeps the transcript replayable. is_error and tool_call_id survive on a tool
// row, tool_calls survive on an assistant row, and the bulk content —
// reasoning, blocks, text — collapses to the marker.
func elidedStub(r domain.Message) schemas.ChatMessage {
	stub := elidedStubText
	content := &schemas.ChatMessageContent{ContentStr: &stub}

	switch r.Role {
	case "tool":
		return schemas.ChatMessage{
			Role:    schemas.ChatMessageRoleTool,
			Content: content,
			ChatToolMessage: &schemas.ChatToolMessage{
				ToolCallID: strPtr(r.ToolCallID),
				IsError:    isErrorPtr(r.IsError),
			},
		}
	case "assistant":
		msg := schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: content}
		if calls, _ := toolCallsToSchema(r.ToolCalls); len(calls) > 0 {
			msg.ChatAssistantMessage = &schemas.ChatAssistantMessage{ToolCalls: calls}
		}
		return msg
	default:
		return schemas.ChatMessage{Role: schemas.ChatMessageRoleUser, Content: content}
	}
}

// ---------------------------------------------------------------------------
// reasoning

func reasoningToSchema(details []domain.ReasoningDetail) []schemas.ChatReasoningDetails {
	if len(details) == 0 {
		return nil
	}
	out := make([]schemas.ChatReasoningDetails, len(details))
	for i, d := range details {
		out[i] = schemas.ChatReasoningDetails{
			Index:     d.Index,
			Type:      schemas.BifrostReasoningDetailsType(d.Type),
			Text:      strPtr(d.Text),
			Signature: strPtr(d.Signature),
			Data:      strPtr(d.Data),
		}
	}
	return out
}

func reasoningToDomain(details []schemas.ChatReasoningDetails) []domain.ReasoningDetail {
	if len(details) == 0 {
		return nil
	}
	out := make([]domain.ReasoningDetail, len(details))
	for i, d := range details {
		out[i] = domain.ReasoningDetail{
			Index:     d.Index,
			Type:      domain.ReasoningDetailType(d.Type),
			Text:      derefStr(d.Text),
			Signature: derefStr(d.Signature),
			Data:      derefStr(d.Data),
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// tool calls

func toolCallsToSchema(calls []domain.ToolCall) ([]schemas.ChatAssistantMessageToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]schemas.ChatAssistantMessageToolCall, len(calls))
	for i, c := range calls {
		args, err := marshalToolInput(c.Input)
		if err != nil {
			return nil, fmt.Errorf("marshal tool input for %q: %w", c.Name, err)
		}
		name := c.Name
		fnType := "function"
		out[i] = schemas.ChatAssistantMessageToolCall{
			Index: uint16(i),
			Type:  &fnType,
			ID:    strPtr(c.ID),
			Function: schemas.ChatAssistantMessageToolCallFunction{
				Name:      &name,
				Arguments: args,
			},
		}
	}
	return out, nil
}

func toolCallsToDomain(calls []schemas.ChatAssistantMessageToolCall) ([]domain.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]domain.ToolCall, len(calls))
	for i, c := range calls {
		input, err := unmarshalToolInput(c.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("unmarshal tool arguments for %q: %w", derefStr(c.Function.Name), err)
		}
		out[i] = domain.ToolCall{
			ID:    derefStr(c.ID),
			Name:  derefStr(c.Function.Name),
			Input: input,
		}
	}
	return out, nil
}

// marshalToolInput renders a tool call's input map to the stringified-JSON
// arguments bifrost carries. encoding/json sorts map keys, so the output is
// byte-stable for a stable input — the property prompt caching relies on. A
// nil/empty input renders as "{}" so the wire always carries a JSON object.
func marshalToolInput(input map[string]any) (string, error) {
	if len(input) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalToolInput parses stringified-JSON arguments back into an input map.
// Empty/"{}" arguments yield a nil map (the round-trip inverse of an empty
// input). Non-object or malformed arguments are an error: the loop must not
// silently drop a tool call's parameters.
func unmarshalToolInput(args string) (map[string]any, error) {
	if args == "" || args == "{}" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// content blocks

func contentBlocksToSchema(blocks []domain.ContentBlock) ([]schemas.ChatContentBlock, error) {
	out := make([]schemas.ChatContentBlock, len(blocks))
	for i, b := range blocks {
		cb := schemas.ChatContentBlock{Type: schemas.ChatContentBlockType(b.Type)}
		switch b.Type {
		case domain.ContentBlockText:
			cb.Text = strPtr(b.Text)
		case domain.ContentBlockImage:
			if b.ImageURL != nil {
				cb.ImageURLStruct = &schemas.ChatInputImage{
					URL:    b.ImageURL.URL,
					FileID: strPtr(b.ImageURL.FileID),
					Detail: strPtr(b.ImageURL.Detail),
				}
			}
		case domain.ContentBlockFile:
			if b.File != nil {
				cb.File = &schemas.ChatInputFile{
					FileData: strPtr(b.File.FileData),
					FileURL:  strPtr(b.File.FileURL),
					FileID:   strPtr(b.File.FileID),
					Filename: strPtr(b.File.Filename),
					FileType: strPtr(b.File.FileType),
				}
			}
		default:
			// The domain content-block vocabulary is OURS and closed
			// (text/image/file). A value outside it is a real invariant
			// violation — a new domain type left unmapped, or a corrupt row —
			// so fail loud rather than emit a payload-less block to the
			// provider. This is the strict half of a deliberate asymmetry with
			// contentBlocksToDomain, which is liberal toward the provider's open
			// vocabulary; it never produces an out-of-vocabulary domain block,
			// so this default only fires on a genuine bug.
			return nil, fmt.Errorf("unsupported content block type %q", b.Type)
		}
		out[i] = cb
	}
	return out, nil
}

func contentBlocksToDomain(blocks []schemas.ChatContentBlock) []domain.ContentBlock {
	out := make([]domain.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		db := domain.ContentBlock{Type: domain.ContentBlockType(b.Type)}
		switch b.Type {
		case schemas.ChatContentBlockTypeText:
			db.Text = derefStr(b.Text)
		case schemas.ChatContentBlockTypeImage:
			if b.ImageURLStruct != nil {
				db.ImageURL = &domain.ContentImageURL{
					URL:    b.ImageURLStruct.URL,
					FileID: derefStr(b.ImageURLStruct.FileID),
					Detail: derefStr(b.ImageURLStruct.Detail),
				}
			}
		case schemas.ChatContentBlockTypeFile:
			if b.File != nil {
				db.File = &domain.ContentFile{
					FileData: derefStr(b.File.FileData),
					FileURL:  derefStr(b.File.FileURL),
					FileID:   derefStr(b.File.FileID),
					Filename: derefStr(b.File.Filename),
					FileType: derefStr(b.File.FileType),
				}
			}
		default:
			// The provider's block vocabulary is open (audio, refusal, whatever
			// they add next); the domain models only text/image/file. Drop what
			// we can't represent rather than keep a stub carrying an
			// unrepresentable Type — a kept stub would be an out-of-vocabulary
			// domain block that contentBlocksToSchema then rejects, bricking any
			// follow-up request assembled from this row. The payload is
			// unrepresentable regardless (the domain type has no field for it),
			// so dropping it loses nothing a stub would have preserved.
			continue
		}
		out = append(out, db)
	}
	return out
}

// ---------------------------------------------------------------------------
// small pointer helpers — the whole bijection uses one convention: an empty
// string is the nil pointer, so empty ↔ nil ↔ empty is identity on the domain
// side and omitempty keeps a genuinely-absent field off the wire.

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// isErrorPtr maps the domain's plain bool onto bifrost's *bool: true becomes a
// set pointer (so the is_error marker reaches Anthropic), false becomes nil (so
// omitempty keeps it off the wire — the OpenAI surface has no such field).
func isErrorPtr(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

func derefBool(p *bool) bool {
	return p != nil && *p
}
