package agentloop

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// sentSystem is the system text an engagement's requests carry. It starts as
// the conversation's frozen text and becomes Params.CurrentSystem's when a
// compaction commits.
type sentSystem struct {
	prompt, addendum string
	// replaced records that a compaction made CurrentSystem the
	// conversation's text during this engagement.
	replaced bool
}

// systemText is the prompt and addendum the next request carries.
func (p Params) systemText() (prompt, addendum string) {
	if p.sent == nil {
		return p.SystemPrompt, p.SystemAddendum
	}
	return p.sent.prompt, p.sent.addendum
}

// adoptCurrentSystem switches the engagement onto CurrentSystem once a
// compaction has committed it as the conversation's text.
func (p Params) adoptCurrentSystem() {
	if p.CurrentSystem == nil || p.sent == nil {
		return
	}
	p.sent.prompt, p.sent.addendum = p.CurrentSystem.Prompt, p.CurrentSystem.Block
	p.sent.replaced = true
}

// appendSystemUpdate queues the claim's system update as a system row. It is
// queued rather than inserted delivered so the first drain delivers it with
// the claim's input, and appended rather than placed above that input: the
// provider takes a system message mid-conversation only after a user turn.
// A compaction at resume that replaced the system text leaves nothing to
// tell.
func (e *Engine) appendSystemUpdate(ctx context.Context, params Params) error {
	if params.SystemUpdate == "" || (params.sent != nil && params.sent.replaced) {
		return nil
	}
	pending := false
	row := &domain.Message{
		ConversationID: params.ConversationID,
		Role:           "system",
		Subtype:        domain.MessageSubtypeInjectionSystemUpdate,
		Content:        params.SystemUpdate,
		Delivered:      &pending,
	}
	if params.CurrentSystem != nil {
		row.Metadata = map[string]any{domain.SystemUpdateMetadataKey: params.CurrentSystem.Parts}
	}
	_, err := e.Transcript.Insert(ctx, params.OrgID, row)
	return err
}
