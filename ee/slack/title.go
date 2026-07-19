// Slack thread title enrichment: best-effort upgrade a freshly-created Slack
// thread entity's title from the channel-less "New thread messages" into
// "New thread messages in #<channel>" once the channel name resolves. Same
// posture as thread permalink resolution (permalink.go) and channel name
// resolution (channels.go): detached from the ingest path, off
// context.Background() plus a short timeout, every failure logged and
// swallowed — a slow or failing conversations.info round-trip must never
// touch the webhook's ack or the socket receive loop.
//
// The entity is created synchronously with the channel-less form
// (ingest.go's slackThreadTitle), because the channel name lives behind an
// API lookup this path resolves after the fact; this resolver upgrades that
// title in place. It runs once, on entity creation only — a re-mention on an
// existing thread reuses the entity and its already-enriched title.
package slack

import (
	"context"
	"net/http"
	"sync"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// titleResolveTimeout bounds the whole resolveTitle chain (bot token read,
// users.info, conversations.info, the store write) regardless of what ctx a
// caller passes in — mirrors permalinkResolveTimeout's rationale
// (permalink.go). Two sequential API calls fit comfortably.
const titleResolveTimeout = 10 * time.Second

// entityTitleUpdater is the narrow slice of db.EntityStore the title resolver
// needs. Declared locally (mirrors permalink.go's entityURLUpdater) so tests
// can supply a single-method fake; db.EntityStore satisfies it structurally,
// so the production wiring (install.go) needs no adapter.
type entityTitleUpdater interface {
	UpdateTitleSystem(ctx context.Context, orgID, entityID, title string) error
}

// TitleResolver resolves a Slack mention's sender + channel display names and
// writes the composed title into entities.title via UpdateTitleSystem.
type TitleResolver struct {
	secrets  db.SecretStore
	entities entityTitleUpdater
	client   *http.Client

	// mu guards inFlight, the per-(org, entity) in-progress set dispatch
	// consults — the same thundering-herd guard PermalinkResolver uses.
	mu       sync.Mutex
	inFlight map[string]bool
}

// NewTitleResolver builds a resolver from the server's non-tx admin-pool store
// aggregate — the same shape NewPermalinkResolver takes.
func NewTitleResolver(stores db.Stores) *TitleResolver {
	return &TitleResolver{
		secrets:  stores.Secrets,
		entities: stores.Entities,
		client:   slackHTTPClient,
	}
}

// dispatch starts resolveTitle for (ws.OrgID, entityID) in a detached
// goroutine, unless a resolution for that entity is already in flight — the
// same guard PermalinkResolver.dispatch uses, though in practice a given
// entity only dispatches once (only the FIRST mention on a thread, the one
// that creates the entity, triggers this; see ingest.go's handleEventCallback).
// Callers should call this instead of spawning resolveTitle directly.
func (r *TitleResolver) dispatch(ws slackstore.Workspace, entityID, channel string) {
	key := ws.OrgID + "/" + entityID
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
		r.resolveTitle(context.Background(), ws, entityID, channel)
	}()
}

// resolveTitle resolves the channel display name and writes the composed
// title. Best-effort: every failure is logged and swallowed, there is no
// error return, and it MUST NEVER run inline on the ingest request path —
// callers dispatch it via dispatch (above), never directly. When the channel
// name doesn't resolve, the composed title equals the channel-less
// slackThreadTitle already on the entity, so the redundant write is skipped
// rather than churning the row with an identical value.
func (r *TitleResolver) resolveTitle(ctx context.Context, ws slackstore.Workspace, entityID, channel string) {
	ctx, cancel := context.WithTimeout(ctx, titleResolveTimeout)
	defer cancel()

	botToken, err := r.secrets.GetSystem(ctx, ws.OrgID, ws.BotTokenRef)
	if err != nil {
		slackLog.Warn("title: bot token read failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
		return
	}
	if botToken == "" {
		slackLog.Warn("title: no bot token configured", "workspace", ws.WorkspaceID)
		return
	}

	var channelName string
	if info, err := slackConversationsInfo(ctx, r.client, botToken, channel); err != nil {
		slackLog.Warn("title: conversations.info failed", "workspace", ws.WorkspaceID, "channel", channel, "error", err)
	} else {
		channelName = info.Name
	}

	// Channel unresolved → the entity already carries the equivalent
	// channel-less title; don't rewrite it with the same value.
	if channelName == "" {
		return
	}

	title := composeThreadTitle(channelName)
	if err := r.entities.UpdateTitleSystem(ctx, ws.OrgID, entityID, title); err != nil {
		slackLog.Warn("title: update failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
	}
}
