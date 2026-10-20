package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server"
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
	// Channel is the id Slack gave the channel when the message arrived; it
	// can change, and the current view (slackMessageCurrentView) reads it as
	// the id the channel has now.
	Channel string `json:"channel"`
	// ChannelRowID is the channel's registry row (slack_channels.id), which a
	// change of Slack's id does not move — what channel_in filters name.
	ChannelRowID string `json:"channel_row_id"`
	TS           string `json:"ts"`                  // the message's own ts
	ThreadTS     string `json:"thread_ts,omitempty"` // parent thread root; empty on a root-message mention
	SenderID     string `json:"sender_id"`           // Slack user ID of the sender
	Text         string `json:"text"`
	EventID      string `json:"event_id"` // Slack's Ev… id — the dedup key
	// Mentioned records whether this message explicitly @-mentioned the
	// bot, as opposed to arriving as a follow-up in an already-engaged
	// thread. Transport detail, not a different situation — see
	// domain.EventSlackMessage's doc. True for an app_mention delivery, false
	// for an engaged-thread follow-up (message.channels / message.groups).
	Mentioned bool `json:"mentioned"`
}

// SlackMessagePredicate narrows which messages a handler fires on.
type SlackMessagePredicate struct {
	ChannelIn []string `json:"channel_in,omitempty" doc:"Match only messages in these channels, each one the team tracks, by registry id (the id field of GET /api/slack/teams/{team_id}/channels, not Slack's channel id). Empty matches every channel the team tracks."`
	// MentionedOnly, when true, restricts to messages that explicitly
	// @-mentioned the bot — excluding engaged-thread follow-ups that
	// didn't. false (the default) matches both.
	MentionedOnly bool `json:"mentioned_only,omitempty" doc:"Match only messages that explicitly @-mentioned the bot (excludes engaged-thread follow-ups)."`
}

// Matches applies the *_in convention: an empty ChannelIn means "no
// filter," matching everything. ChannelIn names registry rows, compared with
// m.ChannelRowID as the current view reads it, so a filter keeps matching a
// channel whose Slack id changed. MentionedOnly is a one-directional
// narrowing flag, not a *_in-style filter: unset (false) matches every
// message, set (true) requires m.Mentioned.
func (p SlackMessagePredicate) Matches(m SlackMessageMetadata) bool {
	if len(p.ChannelIn) > 0 {
		matched := false
		for _, c := range p.ChannelIn {
			if c == m.ChannelRowID {
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

// slackMessageCurrentView is slack:message's events.CurrentView: the recorded
// channel id resolved to the channel's registry row, and the metadata's
// channel and channel_row_id set from it, so a handler filter, a task context
// or an injected follow-up recorded before Slack moved the channel names the
// channel as it is. Resolving through the Slack id rather than the recorded
// row also covers a row a move absorbed (ChannelMove.Absorbed): its id was the
// new one, which names the surviving row. Metadata that already names the
// channel as it is, or a channel TF holds no row for, is returned as recorded,
// byte for byte.
func slackMessageCurrentView(channels slackstore.ChannelRegistryStore) events.CurrentView {
	return func(ctx context.Context, orgID, metadataJSON string) (string, error) {
		var meta SlackMessageMetadata
		if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
			return "", fmt.Errorf("decode slack message metadata: %w", err)
		}
		if meta.Channel == "" {
			return metadataJSON, nil
		}
		row, err := channels.ResolveSystem(ctx, orgID, meta.Channel)
		if err != nil {
			return "", err
		}
		if row == nil || (row.ChannelID == meta.Channel && row.ID == meta.ChannelRowID) {
			return metadataJSON, nil
		}
		meta.Channel, meta.ChannelRowID = row.ChannelID, row.ID
		out, err := json.Marshal(meta)
		if err != nil {
			return "", fmt.Errorf("encode slack message metadata: %w", err)
		}
		return string(out), nil
	}
}

// init registers slack:message's schema and dormancy gate — the "inert
// declaration" half of TFAC-530. OwnershipOwned is the settled
// classification (channel-primary owns; watch semantics apply);
// registering it now is data-only — resolution behavior (which team
// actually owns a given channel) arrives with TFAC-542's SourceHooks
// (routing.go).
// RegisterPredicateReferenceCheck makes the event-handler write routes run
// channelInFaults against a predicate's channel_in.
// GateEventSource activates every TFAC-524 dormancy surface for slack:*
// (event-types/schemas lists, handler create, router freeze) — FeatureSlack
// is already in entitlements.AllFeatures() (TFAC-529), so the composition-
// root parity test (TestRegisteredFeaturesAreDeclared) passes.
//
// A follow-up message on a task that already holds a live conversation folds
// into that conversation instead of enqueueing a second pending_firing — but
// that is the universal same-task absorption rule in internal/routing's
// tryAutoDelegate (a firing whose own task holds the live conversation
// injects), not a per-type declaration on this schema. A fresh message (the
// task has no live conversation) is unaffected — it still fires a new
// blueprint run exactly as before.
func init() {
	events.Register(events.NewSchema[SlackMessageMetadata, SlackMessagePredicate](
		domain.EventSlackMessage, events.OwnershipOwned))
	server.RegisterPredicateReferenceCheck(domain.EventSlackMessage, channelInFaults)
	entitlements.GateEventSource("slack", entitlements.FeatureSlack)
}

// channelInFaults is slack:message's server.PredicateReferenceCheck: every
// channel_in entry the predicate adds must be the registry id of a channel the
// handler's team tracks. That is the set the predicate editor offers, and the
// only set that can match, because the routing hooks deliver a team's handlers
// only messages from channels the team tracks (slackTeamTracksChannel). A Slack
// channel id, the value a caller would naturally reach for, is refused here
// rather than stored as a filter that never fires.
//
// Local mode tracks no Slack channels, so there every added entry is refused.
func channelInFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error) {
	priorIn, err := predicateChannelIn(prior)
	if err != nil {
		return nil, err
	}
	nextIn, err := predicateChannelIn(predicate)
	if err != nil {
		return nil, err
	}
	// seen starts as the stored entries, which are not re-checked, and also
	// keeps an entry the predicate repeats from being reported twice.
	seen := make(map[string]bool, len(priorIn))
	for _, id := range priorIn {
		seen[id] = true
	}
	var added []string
	for _, id := range nextIn {
		if !seen[id] {
			seen[id] = true
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}

	bundle := slackstore.FromTx(tx)
	if bundle == nil {
		return nil, errors.New("slack store bundle absent from the transaction: the slack store extension is not registered")
	}
	rows, err := bundle.TeamChannels.ListForTeam(ctx, orgID, teamID)
	if err != nil && !errors.Is(err, db.ErrNotApplicableInLocal) {
		return nil, fmt.Errorf("read team tracked channels: %w", err)
	}
	tracked := make(map[string]bool, len(rows))
	for _, row := range rows {
		tracked[row.SlackChannelID] = true
	}
	var faults []string
	for _, id := range added {
		if !tracked[id] {
			faults = append(faults, fmt.Sprintf("channel_in: %q is not the registry id of a channel this team tracks "+
				"(channel_in takes the id field of GET /api/slack/teams/{team_id}/channels, not Slack's channel id)", id))
		}
	}
	return faults, nil
}

// predicateChannelIn reads channel_in from a slack:message predicate; nil for
// match-all.
func predicateChannelIn(predicate *string) ([]string, error) {
	if predicate == nil {
		return nil, nil
	}
	var p SlackMessagePredicate
	if err := json.Unmarshal([]byte(*predicate), &p); err != nil {
		return nil, fmt.Errorf("decode slack message predicate: %w", err)
	}
	return p.ChannelIn, nil
}
