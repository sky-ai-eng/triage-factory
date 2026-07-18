package slack

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// SlackMessageMetadata is the durable audit shape for one inbound Slack
// message addressed to the bot — everything the ingest pipeline
// (ingest.go) captures, JSON-marshaled into events.metadata_json. A delivery
// is either an explicit @-mention (Mentioned=true) or an engaged-thread
// follow-up in a thread the bot already owns (Mentioned=false).
type SlackMessageMetadata struct {
	// WorkspaceID is the Slack team ID. The entity key (domain.SlackSourceID)
	// deliberately excludes workspace context (channel IDs are stable across
	// Enterprise Grid / Slack Connect shared channels) — downstream
	// (routing, exec verbs) resolves token/workspace context from THIS
	// field, never from the entity key.
	WorkspaceID string `json:"workspace_id"`
	// APIAppID is the Slack app id, alongside WorkspaceID, needed to resolve
	// the exact org_slack_workspaces row this message belongs to — since
	// TFAC-533's re-key, one org can hold two Slack apps in the same
	// workspace, so WorkspaceID alone can't select the row (and the wrong
	// row means acting as the wrong bot identity). Downstream consumers
	// resolve via WorkspaceStore.GetByWorkspaceAppSystem(workspaceID,
	// apiAppID). No fallback for pre-release events — none exist.
	APIAppID string `json:"api_app_id"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`                  // the message's own ts
	ThreadTS string `json:"thread_ts,omitempty"` // parent thread root; empty on a root-message mention
	SenderID string `json:"sender_id"`           // Slack user ID of the sender
	Text     string `json:"text"`
	EventID  string `json:"event_id"` // Slack's Ev… id — the dedup key
	// Mentioned records whether this message explicitly @-mentioned the
	// bot, as opposed to arriving as a follow-up in an already-engaged
	// thread. Transport detail, not a different situation — see
	// domain.EventSlackMessage's doc. True for an app_mention delivery, false
	// for an engaged-thread follow-up (message.channels / message.groups).
	Mentioned bool `json:"mentioned"`
}

// SlackMessagePredicate narrows which messages a handler fires on.
type SlackMessagePredicate struct {
	ChannelIn []string `json:"channel_in,omitempty" doc:"Match only messages in these Slack channel IDs (empty = no filter, matches any channel)."`
	// MentionedOnly, when true, restricts to messages that explicitly
	// @-mentioned the bot — excluding engaged-thread follow-ups that
	// didn't. false (the default) matches both.
	MentionedOnly bool `json:"mentioned_only,omitempty" doc:"Match only messages that explicitly @-mentioned the bot (excludes engaged-thread follow-ups)."`
}

// Matches applies the *_in convention: an empty ChannelIn means "no
// filter," matching everything. Channel IDs are case-sensitive Slack
// tokens, so this is a strict == rather than events.stringInSliceFold's
// case-fold (that helper is also unexported outside package events).
// MentionedOnly is a one-directional narrowing flag, not a *_in-style
// filter: unset (false) matches every message, set (true) requires
// m.Mentioned.
func (p SlackMessagePredicate) Matches(m SlackMessageMetadata) bool {
	if len(p.ChannelIn) > 0 {
		matched := false
		for _, c := range p.ChannelIn {
			if c == m.Channel {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if p.MentionedOnly && !m.Mentioned {
		return false
	}
	return true
}

// init registers slack:message's schema and dormancy gate — the "inert
// declaration" half of TFAC-530. OwnershipOwned is the settled
// classification (channel-primary owns; watch semantics apply);
// registering it now is data-only — resolution behavior (which team
// actually owns a given channel) arrives with TFAC-542's SourceHooks
// (routing.go).
// GateEventSource activates every TFAC-524 dormancy surface for slack:*
// (event-types/schemas lists, handler create, router freeze) — FeatureSlack
// is already in entitlements.AllFeatures() (TFAC-529), so the composition-
// root parity test (TestRegisteredFeaturesAreDeclared) passes.
//
// Additive=true (TFAC-597) flips the auto-delegation defer/inject
// decision: a follow-up message on an entity whose task already has an
// active auto run injects a <system-note> into that live run
// (internal/routing's tryAutoDelegate, via events.AdditiveFor) instead of
// enqueueing a second pending_firing. A fresh message (no active run) is
// unaffected — it still fires a new run exactly as before.
func init() {
	schema := events.NewSchema[SlackMessageMetadata, SlackMessagePredicate](
		domain.EventSlackMessage, events.OwnershipOwned)
	schema.Additive = true
	events.Register(schema)
	entitlements.GateEventSource("slack", entitlements.FeatureSlack)
}
