// Slack mention title enrichment: best-effort recompose a freshly-created
// Slack thread entity's title into a human-readable "<sender> said
// "<message>" in #<channel>" once the sender's display name and the channel
// name resolve. Same posture as thread permalink resolution (permalink.go),
// sender identity capture (identity.go), and channel name resolution
// (channels.go): detached from the ingest path, off context.Background()
// plus a short timeout, every failure logged and swallowed — a slow or
// failing users.info / conversations.info round-trip must never touch the
// webhook's ack or the socket receive loop.
//
// The entity is created synchronously with the humanized message text alone
// (ingest.go's mentionTitle), because the two names live behind API lookups
// this path resolves after the fact; this resolver upgrades that title in
// place. It runs once, on entity creation only — a re-mention on an existing
// thread reuses the entity and its already-enriched title.
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
func (r *TitleResolver) dispatch(ws slackstore.Workspace, entityID, channel, senderID, rawText string) {
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
		r.resolveTitle(context.Background(), ws, entityID, channel, senderID, rawText)
	}()
}

// resolveTitle resolves the sender + channel display names and writes the
// composed title. Best-effort: every failure is logged and swallowed, there
// is no error return, and it MUST NEVER run inline on the ingest request path
// — callers dispatch it via dispatch (above), never directly. Each name is
// independent: a failed users.info still yields a "…" in #channel" title, and
// vice versa. When NEITHER name resolves, the composed title equals the
// synchronous mentionTitle already on the entity, so the redundant write is
// skipped rather than churning the row with an identical value.
func (r *TitleResolver) resolveTitle(ctx context.Context, ws slackstore.Workspace, entityID, channel, senderID, rawText string) {
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

	var senderName string
	if senderID != "" {
		if info, err := slackUsersInfo(ctx, r.client, botToken, senderID); err != nil {
			slackLog.Warn("title: users.info failed", "workspace", ws.WorkspaceID, "slack_user", senderID, "error", err)
		} else {
			senderName = nonEmpty(info.DisplayName, info.RealName)
		}
	}

	var channelName string
	if info, err := slackConversationsInfo(ctx, r.client, botToken, channel); err != nil {
		slackLog.Warn("title: conversations.info failed", "workspace", ws.WorkspaceID, "channel", channel, "error", err)
	} else {
		channelName = info.Name
	}

	// Nothing resolved → the entity already carries the equivalent
	// humanized-text title; don't rewrite it with the same value.
	if senderName == "" && channelName == "" {
		return
	}

	title := composeMentionTitle(senderName, channelName, rawText)
	if err := r.entities.UpdateTitleSystem(ctx, ws.OrgID, entityID, title); err != nil {
		slackLog.Warn("title: update failed", "workspace", ws.WorkspaceID, "entity", entityID, "error", err)
	}
}
