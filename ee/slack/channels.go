// Slack channel name resolution (TFAC-542): best-effort resolve a
// channel's display name via conversations.info once the sighting
// registry (channels.go's ingest call site, TFAC-541's slack_channels
// table) records a first-seen or stale-named channel. Same posture as
// sender identity capture (identity.go): detached from the ingest path, off
// context.Background() plus a short timeout, every failure logged and
// swallowed — a slow or failing conversations.info round-trip must never
// touch the webhook's ack or the socket receive loop.
package slack

import (
	"context"
	"net/http"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// channelNameResolveTimeout bounds the whole resolveChannelName chain (bot
// token read, conversations.info) regardless of what ctx a caller passes
// in — mirrors slackIdentityResolveTimeout's rationale (identity.go).
const channelNameResolveTimeout = 10 * time.Second

// channelNameStaleAfter is how long a resolved channel name is trusted
// before ingest re-dispatches conversations.info for it. The registry
// name is a display hint, not a source of truth, so a channel rename
// converging lazily over days is an acceptable tradeoff against re-hitting
// conversations.info on every mention.
const channelNameStaleAfter = 7 * 24 * time.Hour

// ChannelResolver resolves and records a Slack channel's display name into
// the channel registry (slack_channels, TFAC-541).
type ChannelResolver struct {
	secrets  db.SecretStore
	channels slackstore.ChannelRegistryStore
	client   *http.Client
}

// NewChannelResolver builds a resolver from the server's non-tx admin-pool
// store aggregate — the same shape NewIdentityResolver takes.
func NewChannelResolver(stores db.Stores) *ChannelResolver {
	return &ChannelResolver{
		secrets:  stores.Secrets,
		channels: slackstore.FromStores(stores).Channels,
		client:   slackHTTPClient,
	}
}

// resolveChannelName looks up channelID's current display name and writes
// it into the registry. Best-effort: every failure is logged and
// swallowed, there is no error return, and it MUST NEVER run inline on the
// ingest request path — callers dispatch it as
//
//	go r.resolveChannelName(context.Background(), ws, channelID)
//
// off context.Background(), never the request ctx (see ingest.go's
// captureChannelSighting). Never writes a name on API failure — an
// unresolved row renders its raw channel ID rather than risk a wrong
// fallback name.
func (r *ChannelResolver) resolveChannelName(ctx context.Context, ws slackstore.Workspace, channelID string) {
	ctx, cancel := context.WithTimeout(ctx, channelNameResolveTimeout)
	defer cancel()

	botToken, err := r.secrets.GetSystem(ctx, ws.OrgID, ws.BotTokenRef)
	if err != nil {
		slackLog.Warn("channel name: bot token read failed", "workspace", ws.WorkspaceID, "channel", channelID, "error", err)
		return
	}
	if botToken == "" {
		slackLog.Warn("channel name: no bot token configured", "workspace", ws.WorkspaceID)
		return
	}

	info, err := slackConversationsInfo(ctx, r.client, botToken, channelID)
	if err != nil {
		slackLog.Warn("channel name: conversations.info failed", "workspace", ws.WorkspaceID, "channel", channelID, "error", err)
		return
	}
	if info.Name == "" {
		return
	}
	if err := r.channels.SetNameSystem(ctx, ws.OrgID, channelID, info.Name); err != nil {
		slackLog.Warn("channel name: set name failed", "workspace", ws.WorkspaceID, "channel", channelID, "error", err)
	}
}
