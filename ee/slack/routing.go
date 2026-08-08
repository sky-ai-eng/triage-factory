// Slack routing hooks (TFAC-542): the routing.SourceHooks Slack registers
// at install (install.go) so a slack:message event resolves through the
// same owner-ladder machinery every other Owned event source uses — see
// internal/routing/source_registry.go's doc for the inversion-seam
// rationale core never importing ee/ enables.
package slack

import (
	"context"
	"encoding/json"
	"fmt"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
)

// slackChannelOwner builds routing.SourceHooks.ResolveOwner for the Slack
// source: a mention's owner is its channel's primary tracking team (v1 of
// TFAC-510's owner ladder — channel-primary owns, the mentioning sender is
// audit-only).
//
// Three DATA states resolve to routing.Unowned() — untracked/unclaimed,
// recorded taskless, only an applies_to_unowned watcher can still fire:
// metadata this package can't read (never expected from its own ingest, but
// the hook must not panic the drain goroutine on it), a message carrying no
// channel, and a channel with no primary team. Each SAYS it resolved rather
// than returning the empty value, which is what lets the seam refuse the
// fourth shape — an empty answer nobody claimed.
//
// A failed primary-team lookup is none of those — it is "could not find out" —
// so it propagates and the mention is replayed. Degrading that read to "nobody
// owns this" loses the mention outright (Slack ingest has no snapshot-diff to
// re-derive it from), or hands it to a team that merely watches the channel,
// which then answers — and owns — another team's conversation.
func slackChannelOwner(bundle *slackstore.Bundle) func(ctx context.Context, orgID string, evt domain.Event, entityID string) (routing.OwnerResolution, error) {
	return func(ctx context.Context, orgID string, evt domain.Event, entityID string) (routing.OwnerResolution, error) {
		var meta SlackMessageMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			slackLog.Error("slack owner resolution: message metadata is not valid JSON", "org_id", orgID, "entity_id", entityID, "error", err)
			return routing.Unowned(), nil
		}
		if meta.Channel == "" {
			slackLog.Error("slack owner resolution: message metadata missing channel", "org_id", orgID, "entity_id", entityID)
			return routing.Unowned(), nil
		}
		team, err := bundle.TeamChannels.PrimaryTeamForChannelSystem(ctx, orgID, meta.Channel)
		if err != nil {
			slackLog.Error("slack owner resolution: primary team lookup failed", "org_id", orgID, "channel", meta.Channel, "error", err)
			return routing.OwnerResolution{}, fmt.Errorf("slack primary team for channel %s: %w", meta.Channel, err)
		}
		if team == "" {
			return routing.Unowned(), nil
		}
		return routing.OwnedBy(team), nil
	}
}

// slackTeamTracksChannel builds routing.SourceHooks.TracksScope for the
// Slack source: does teamID track the mention's channel. Fails OPEN — on
// malformed metadata or a store error alike — mirroring
// teamTracksEventRepo (internal/routing/team_routing.go): dropping
// legitimate work on a transient DB blip (or an unexpected metadata shape)
// is worse than a briefly-wide gate.
//
// This is the deliberate posture split from slackChannelOwner next door, not
// an inconsistency: this gate only ever REMOVES a team from a set someone
// else computed, so failing open costs a briefly-too-wide gate the next
// message corrects. The resolver DECIDES who owns the channel, and a wrong
// answer there is answered by the wrong team, in public.
func slackTeamTracksChannel(bundle *slackstore.Bundle) func(ctx context.Context, evt domain.Event, teamID string) bool {
	return func(ctx context.Context, evt domain.Event, teamID string) bool {
		var meta SlackMessageMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil || meta.Channel == "" {
			return true
		}
		tracks, err := bundle.TeamChannels.TracksChannelSystem(ctx, evt.OrgID, teamID, meta.Channel)
		if err != nil {
			slackLog.Warn("slack team-channel tracking lookup failed, allowing", "team_id", teamID, "channel", meta.Channel, "error", err)
			return true
		}
		return tracks
	}
}
