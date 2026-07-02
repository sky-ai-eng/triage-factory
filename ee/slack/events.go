package slack

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// SlackMentionMetadata is the durable audit shape for one app_mention
// delivery — everything the ingest pipeline (ingest.go) captures, JSON-
// marshaled into events.metadata_json.
type SlackMentionMetadata struct {
	// WorkspaceID is the Slack team ID. The entity key (domain.SlackSourceID)
	// deliberately excludes workspace context (channel IDs are stable across
	// Enterprise Grid / Slack Connect shared channels) — downstream
	// (routing, exec verbs) resolves token/workspace context from THIS
	// field, never from the entity key.
	WorkspaceID string `json:"workspace_id"`
	Channel     string `json:"channel"`
	TS          string `json:"ts"`                  // the mention message's own ts
	ThreadTS    string `json:"thread_ts,omitempty"` // parent thread root; empty on a root-message mention
	SenderID    string `json:"sender_id"`           // Slack user ID of the mentioning human
	Text        string `json:"text"`
	EventID     string `json:"event_id"` // Slack's Ev… id — the dedup key
}

// SlackMentionPredicate is deliberately empty in this leaf (match-all).
// TFAC-510 enriches it (channel_in) once routing exists to scope on —
// predicates decide what runs, and nothing routes until then anyway.
type SlackMentionPredicate struct{}

func (SlackMentionPredicate) Matches(SlackMentionMetadata) bool { return true }

// init registers slack:mention's schema and dormancy gate — the "inert
// declaration" half of TFAC-530. OwnershipOwned is the settled
// classification (channel-primary owns; watch semantics apply);
// registering it now is data-only — resolution behavior (which team
// actually owns a given channel) arrives with TFAC-510's SourceHooks.
// GateEventSource activates every TFAC-524 dormancy surface for slack:*
// (event-types/schemas lists, handler create, router freeze) — FeatureSlack
// is already in entitlements.AllFeatures() (TFAC-529), so the composition-
// root parity test (TestRegisteredFeaturesAreDeclared) passes.
func init() {
	events.Register(events.NewSchema[SlackMentionMetadata, SlackMentionPredicate](
		domain.EventSlackMention, events.OwnershipOwned))
	entitlements.GateEventSource("slack", entitlements.FeatureSlack)
}
