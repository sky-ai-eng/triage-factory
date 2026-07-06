// Slack channel name resolution (TFAC-542): best-effort resolve a
// channel's display name via conversations.info once the sighting registry
// (ingest.go's captureChannelSighting call site, TFAC-541's slack_channels
// table) records a first-seen or stale-named channel. Same posture as
// sender identity capture (identity.go): detached from the ingest path, off
// context.Background() plus a short timeout, every failure logged and
// swallowed — a slow or failing conversations.info round-trip must never
// touch the webhook's ack or the socket receive loop.
package slack

import (
	"context"
	"net/http"
	"sync"
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

	// mu guards inFlight, the per-(org, channel) in-progress set dispatch
	// consults — see dispatch's doc.
	mu       sync.Mutex
	inFlight map[string]bool
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

// dispatch starts resolveChannelName for (ws.OrgID, channelID) in a detached
// goroutine, unless a resolution for that channel is already in flight. This
// is the thundering-herd guard: while conversations.info is slow or failing
// for a channel, every mention in it would otherwise re-check the same stale
// row and spawn another goroutine hitting the Slack API, one per mention,
// until the first one finally succeeds. Callers should call this instead of
// spawning resolveChannelName directly — see ingest.go's
// captureChannelSighting.
func (r *ChannelResolver) dispatch(ws slackstore.Workspace, channelID string) {
	key := ws.OrgID + "/" + channelID
	r.mu.Lock()
	if r.inFlight == nil {
		r.inFlight = map[string]bool{}
	}
	if r.inFlight[key] {
		r.mu.Unlock()
		return
	}
	r.inFlight[key] = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.inFlight, key)
			r.mu.Unlock()
		}()
		r.resolveChannelName(context.Background(), ws, channelID)
	}()
}

// resolveChannelName looks up channelID's current display name and writes
// it into the registry. Best-effort: every failure is logged and
// swallowed, there is no error return, and it MUST NEVER run inline on the
// ingest request path — callers dispatch it via dispatch (above), never
// directly, so a slow/stuck resolution can't spawn duplicates. Never writes
// a name on API failure — an unresolved row renders its raw channel ID
// rather than risk a wrong fallback name.
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
