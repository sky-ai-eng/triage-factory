package inference

import "github.com/maximhq/bifrost/core/schemas"

// cache_control policy v1. Two breakpoints, and this package is their only
// writer:
//
//   - the system-prompt breakpoint (withSystemCacheBreakpoint), on the FIRST
//     block of the system message, caches the stable system+tools prefix;
//   - the moving conversation breakpoint (applyMovingCacheBreakpoint), on the
//     last block of the final message, caches everything up to the newest turn.
//
// Anthropic caches the prefix ending at each ephemeral breakpoint. The policy
// is deliberately small; the loop ticket revisits it. It lives here so
// assembly stays the sole owner of cache_control placement.

// ephemeralCacheControl is the marker both breakpoints stamp. The default
// (5-minute) TTL is left unset — bifrost/Anthropic apply their default; a
// longer TTL is a policy decision for the loop ticket, not v1.
func ephemeralCacheControl() *schemas.CacheControl {
	return &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral}
}

// applyMovingCacheBreakpoint stamps the ephemeral marker on the last content
// block of the final message. A string-content message is promoted to a
// single text block so the marker has somewhere to live; a message with no
// content at all (a bare tool-call assistant turn) is left untouched.
func applyMovingCacheBreakpoint(msgs []schemas.ChatMessage) {
	if len(msgs) == 0 {
		return
	}
	stampLastBlock(&msgs[len(msgs)-1])
}

// withSystemCacheBreakpoint returns the system ChatMessage for one call: the
// shared prompt, then the per-conversation addendum when there is one, with
// the ephemeral marker on the FIRST block. An empty addendum yields a single
// block; two empty strings yield a zero message the caller drops.
//
// The marker's position is the point of the split. Block 1 is byte-identical
// across every conversation on the same model and tool schemas, so the entry
// written at its end is the one they all read; block 2 is this conversation's
// alone. Stamping the last block instead would put the only system entry after
// the per-conversation bytes, and the shared entry would never be written.
//
// Block 2 gets no marker of its own. A write happens only at a breakpoint, and
// the moving conversation breakpoint already writes an entry covering block 2
// on the first turn; every later turn's lookback finds it. A marker here would
// write an entry at a position only this conversation can match, which its own
// later turns already beat with a longer one.
func withSystemCacheBreakpoint(systemPrompt, addendum string) schemas.ChatMessage {
	texts := make([]string, 0, 2)
	for _, s := range []string{systemPrompt, addendum} {
		if s != "" {
			texts = append(texts, s)
		}
	}
	if len(texts) == 0 {
		return schemas.ChatMessage{}
	}
	blocks := make([]schemas.ChatContentBlock, len(texts))
	for i := range texts {
		blocks[i] = schemas.ChatContentBlock{
			Type: schemas.ChatContentBlockTypeText,
			Text: &texts[i],
		}
	}
	blocks[0].CacheControl = ephemeralCacheControl()
	return schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleSystem,
		Content: &schemas.ChatMessageContent{ContentBlocks: blocks},
	}
}

// stampLastBlock puts the ephemeral marker on a message's last content block,
// promoting string content to a one-block array first. A contentless message
// is a no-op.
func stampLastBlock(msg *schemas.ChatMessage) {
	if msg.Content == nil {
		return
	}
	if msg.Content.ContentStr != nil {
		msg.Content = &schemas.ChatMessageContent{
			ContentBlocks: []schemas.ChatContentBlock{{
				Type:         schemas.ChatContentBlockTypeText,
				Text:         msg.Content.ContentStr,
				CacheControl: ephemeralCacheControl(),
			}},
		}
		return
	}
	if n := len(msg.Content.ContentBlocks); n > 0 {
		msg.Content.ContentBlocks[n-1].CacheControl = ephemeralCacheControl()
	}
}
