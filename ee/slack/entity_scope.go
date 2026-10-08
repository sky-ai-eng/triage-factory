package slack

import (
	"context"
	"strings"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entityscope"
)

// init plugs Slack into core's entity-scope resolution. A Slack entity is keyed
// under the workspace of the connection it came through, and an org can
// connect several, so its scope is not a function of org settings the way a
// core source's is. Ingest and the thread-root op know the workspace and pass
// it directly; this answers the core callers that hold only the entity's key —
// the exec recording funnel's touch, memory load, and the produced-entity
// attach.
func init() {
	entityscope.Register("slack", slackEntityScope)
}

// slackEntityScope reads the workspace off the channel the key names, through
// the org's channel registry: a channel id belongs to exactly one workspace,
// and a channel TF has never seen is one no connected workspace has reported,
// so it has no scope here and nothing is recorded against it.
func slackEntityScope(ctx context.Context, stores db.Stores, orgID, sourceID string) (string, error) {
	channelID, _, ok := strings.Cut(sourceID, "/")
	if !ok || channelID == "" {
		return "", nil
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return "", nil
	}
	channel, err := bundle.Channels.GetSystem(ctx, orgID, channelID)
	if err != nil || channel == nil {
		return "", err
	}
	return channel.WorkspaceID, nil
}
