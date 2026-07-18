// Slack routing hooks (TFAC-542): the routing.SourceHooks Slack registers
// at install (install.go) so a slack:message event resolves through the
// same owner-ladder machinery every other Owned event source uses — see
// internal/routing/source_registry.go's doc for the inversion-seam
// rationale core never importing ee/ enables.
package slack

import (
	"context"
	"encoding/json"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// slackChannelOwner builds routing.SourceHooks.ResolveOwner for the Slack
// source: a mention's owner is its channel's primary tracking team (v1 of
// TFAC-510's owner ladder — channel-primary owns, the mentioning sender is
// audit-only). Malformed metadata (never expected from this package's own
// ingest, but the hook must not panic the drain goroutine on it) and "no
// primary" both resolve to ("", nil) — untracked/unclaimed, recorded
// taskless; only an applies_to_unowned watcher can still fire.
func slackChannelOwner(bundle *slackstore.Bundle) func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string) {
	return func(ctx context.Context, orgID string, evt domain.Event, entityID string) (string, []string) {
		var meta SlackMessageMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			slackLog.Error("slack owner resolution: mention metadata is not valid JSON", "org_id", orgID, "entity_id", entityID, "error", err)
			return "", nil
		}
		if meta.Channel == "" {
			slackLog.Error("slack owner resolution: mention metadata missing channel", "org_id", orgID, "entity_id", entityID)
			return "", nil
		}
		team, err := bundle.TeamChannels.PrimaryTeamForChannelSystem(ctx, orgID, meta.Channel)
		if err != nil {
			slackLog.Error("slack owner resolution: primary team lookup failed", "org_id", orgID, "channel", meta.Channel, "error", err)
			return "", nil
		}
		if team == "" {
			return "", nil
		}
		return team, []string{team}
	}
}

// slackTeamTracksChannel builds routing.SourceHooks.TracksScope for the
// Slack source: does teamID track the mention's channel. Fails OPEN — on
// malformed metadata or a store error alike — mirroring
// teamTracksEventRepo (internal/routing/team_routing.go): dropping
// legitimate work on a transient DB blip (or an unexpected metadata shape)
// is worse than a briefly-wide gate.
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
