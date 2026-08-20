// Slack thread permalink enrichment (TFAC-595): best-effort resolve a
// freshly-created Slack thread entity's chat.getPermalink URL and write it
// into entities.url. Same posture as channel name resolution (channels.go)
// and sender identity capture (identity.go): detached from the ingest
// path, off context.Background() plus a short timeout, every failure
// logged and swallowed — a slow or failing chat.getPermalink round-trip
// must never touch the webhook's ack or the socket receive loop.
package slack

import (
	"context"
	"net/http"
	"sync"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// permalinkResolveTimeout bounds the whole resolvePermalink chain (bot
// token read, chat.getPermalink, the store write) regardless of what ctx a
// caller passes in — mirrors channelNameResolveTimeout's rationale
// (channels.go).
const permalinkResolveTimeout = 10 * time.Second

// entityURLUpdater is the narrow slice of db.EntityStore the permalink
// resolver needs. Declared locally (mirrors ingest.go's entityFinder) so
// tests can supply a single-method fake instead of implementing
// db.EntityStore's entire surface; db.EntityStore satisfies this
// structurally, so the production wiring (install.go) needs no adapter.
type entityURLUpdater interface {
	UpdateURLSystem(ctx context.Context, orgID, entityID, url string) (domain.Entity, error)
}

// PermalinkResolver resolves a Slack thread entity's chat.getPermalink URL
// and writes it into entities.url via EntityStore.UpdateURLSystem.
type PermalinkResolver struct {
	secrets  db.SecretStore
	entities entityURLUpdater
	client   *http.Client

	// mu guards inFlight, the per-(org, entity) in-progress set dispatch
	// consults — see dispatch's doc.
	mu       sync.Mutex
	inFlight map[string]bool
}

// NewPermalinkResolver builds a resolver from the server's non-tx
// admin-pool store aggregate — the same shape NewChannelResolver takes.
func NewPermalinkResolver(stores db.Stores) *PermalinkResolver {
	return &PermalinkResolver{
		secrets:  stores.Secrets,
		entities: stores.Entities,
		client:   slackHTTPClient,
	}
}

// dispatch starts resolvePermalink for (ws.OrgID, entityID) in a detached
// goroutine, unless a resolution for that entity is already in flight —
// the same thundering-herd guard as ChannelResolver.dispatch, though in
// practice a given entity only dispatches once (only the FIRST mention on
// a thread — the one that creates the entity — triggers this at all; see
// ingest.go's handleEventCallback). Callers should call this instead of
// spawning resolvePermalink directly.
func (r *PermalinkResolver) dispatch(ws slackstore.Workspace, entityID, channel, rootTS string) {
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
		r.resolvePermalink(context.Background(), ws, entityID, channel, rootTS)
	}()
}

// resolvePermalink looks up the thread root's permalink and writes it into
// the entity's url. Best-effort: every failure is logged and swallowed,
// there is no error return, and it MUST NEVER run inline on the ingest
// request path — callers dispatch it via dispatch (above), never directly.
// Never writes on API failure — an unresolved entity keeps its empty url
// rather than risk a wrong fallback.
func (r *PermalinkResolver) resolvePermalink(ctx context.Context, ws slackstore.Workspace, entityID, channel, rootTS string) {
	ctx, cancel := context.WithTimeout(ctx, permalinkResolveTimeout)
	defer cancel()

	botToken, err := r.secrets.GetSystem(ctx, ws.OrgID, ws.BotTokenRef)
	if err != nil {
		slackLog.Warn("permalink: bot token read failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
		return
	}
	if botToken == "" {
		slackLog.Warn("permalink: no bot token configured", "workspace", ws.WorkspaceID)
		return
	}

	permalink, err := slackChatGetPermalink(ctx, r.client, botToken, channel, rootTS)
	if err != nil {
		slackLog.Warn("permalink: chat.getPermalink failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
		return
	}
	if permalink == "" {
		return
	}
	if _, err := r.entities.UpdateURLSystem(ctx, ws.OrgID, entityID, permalink); err != nil {
		slackLog.Warn("permalink: update url failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
	}
}
