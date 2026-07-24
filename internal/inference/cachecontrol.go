package inference

import "github.com/maximhq/bifrost/core/schemas"

// cache_control policy v1. Two breakpoints, and this package is their only
// writer:
//
//   - the system-prompt breakpoint (withSystemCacheBreakpoint), on the last
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

// withSystemCacheBreakpoint returns a system ChatMessage carrying the given
// prompt with the ephemeral marker on its last block, so Anthropic caches the
// system+tools prefix. An empty prompt yields a zero message the caller drops.
func withSystemCacheBreakpoint(systemPrompt string) schemas.ChatMessage {
	if systemPrompt == "" {
		return schemas.ChatMessage{}
	}
	msg := schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleSystem,
		Content: &schemas.ChatMessageContent{ContentStr: &systemPrompt},
	}
	stampLastBlock(&msg)
	return msg
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
