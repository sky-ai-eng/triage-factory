package inference

import (
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// reasoningAccumulator reassembles streamed reasoning into the final
// ChatReasoningDetails list. Details are keyed by their stream index; text
// appends across deltas while type/signature/data take the latest non-empty
// value, so a signature delivered on a reasoning block's closing delta lands
// on the right block. A provider that streams only the flat Reasoning string
// (no structured details) is folded into a single synthesized text detail.
type reasoningAccumulator struct {
	order   []int
	byIndex map[int]*reasoningEntry
	flat    strings.Builder
}

type reasoningEntry struct {
	index     int
	typ       schemas.BifrostReasoningDetailsType
	text      strings.Builder
	signature string
	data      string
}

func newReasoningAccumulator() *reasoningAccumulator {
	return &reasoningAccumulator{byIndex: map[int]*reasoningEntry{}}
}

func (r *reasoningAccumulator) add(delta *schemas.ChatStreamResponseChoiceDelta) {
	for i := range delta.ReasoningDetails {
		d := delta.ReasoningDetails[i]
		e, ok := r.byIndex[d.Index]
		if !ok {
			e = &reasoningEntry{index: d.Index, typ: d.Type}
			r.byIndex[d.Index] = e
			r.order = append(r.order, d.Index)
		}
		if d.Type != "" {
			e.typ = d.Type
		}
		if d.Text != nil {
			e.text.WriteString(*d.Text)
		}
		if d.Signature != nil && *d.Signature != "" {
			e.signature = *d.Signature
		}
		if d.Data != nil && *d.Data != "" {
			e.data = *d.Data
		}
	}
	// Flat reasoning (a provider that streams reasoning as plain text with no
	// structured details) accumulates separately and is only used when no
	// structured details arrived.
	if delta.Reasoning != nil {
		r.flat.WriteString(*delta.Reasoning)
	}
}

func (r *reasoningAccumulator) finalize() []schemas.ChatReasoningDetails {
	if len(r.order) == 0 {
		if flat := r.flat.String(); flat != "" {
			return []schemas.ChatReasoningDetails{{
				Index: 0,
				Type:  schemas.BifrostReasoningDetailsTypeText,
				Text:  &flat,
			}}
		}
		return nil
	}
	out := make([]schemas.ChatReasoningDetails, 0, len(r.order))
	for _, idx := range r.order {
		e := r.byIndex[idx]
		d := schemas.ChatReasoningDetails{Index: e.index, Type: e.typ}
		if t := e.text.String(); t != "" {
			d.Text = &t
		}
		if e.signature != "" {
			s := e.signature
			d.Signature = &s
		}
		if e.data != "" {
			dt := e.data
			d.Data = &dt
		}
		out = append(out, d)
	}
	return out
}

// toolCallAccumulator reassembles streamed tool calls. Calls are keyed by
// their stream index; id/name/type arrive on the first fragment and arguments
// concatenate across fragments (bifrost streams arguments as a growing
// stringified-JSON fragment).
type toolCallAccumulator struct {
	order   []uint16
	byIndex map[uint16]*toolCallEntry
}

type toolCallEntry struct {
	index    uint16
	id       string
	typ      string
	name     string
	argsText strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIndex: map[uint16]*toolCallEntry{}}
}

func (t *toolCallAccumulator) add(calls []schemas.ChatAssistantMessageToolCall) {
	for i := range calls {
		c := calls[i]
		e, ok := t.byIndex[c.Index]
		if !ok {
			e = &toolCallEntry{index: c.Index}
			t.byIndex[c.Index] = e
			t.order = append(t.order, c.Index)
		}
		if c.ID != nil && *c.ID != "" {
			e.id = *c.ID
		}
		if c.Type != nil && *c.Type != "" {
			e.typ = *c.Type
		}
		if c.Function.Name != nil && *c.Function.Name != "" {
			e.name = *c.Function.Name
		}
		e.argsText.WriteString(c.Function.Arguments)
	}
}

func (t *toolCallAccumulator) finalize() []schemas.ChatAssistantMessageToolCall {
	if len(t.order) == 0 {
		return nil
	}
	out := make([]schemas.ChatAssistantMessageToolCall, 0, len(t.order))
	for _, idx := range t.order {
		e := t.byIndex[idx]
		call := schemas.ChatAssistantMessageToolCall{
			Index: e.index,
			Function: schemas.ChatAssistantMessageToolCallFunction{
				Arguments: e.argsText.String(),
			},
		}
		if e.id != "" {
			id := e.id
			call.ID = &id
		}
		typ := e.typ
		if typ == "" {
			typ = "function"
		}
		call.Type = &typ
		if e.name != "" {
			name := e.name
			call.Function.Name = &name
		}
		out = append(out, call)
	}
	return out
}
