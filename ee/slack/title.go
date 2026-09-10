// Slack thread title enrichment: best-effort upgrade a freshly-created Slack
// thread entity's title from the anonymous "New thread messages" into
// "<thread author> in #<channel>" once those two display names resolve. Same
// posture as thread permalink resolution (permalink.go) and channel name
// resolution (channels.go): detached from the ingest path, off
// context.Background() plus a short timeout, every failure logged and
// swallowed — a slow or failing Slack round-trip must never touch the
// webhook's ack or the socket receive loop.
//
// The entity is created synchronously with the anonymous form (ingest.go's
// slackThreadTitle), because both names live behind API lookups this path
// resolves after the fact; this resolver upgrades that title in place. It
// runs once, on entity creation only — a re-mention on an existing thread
// reuses the entity and its already-enriched title, so a later channel or
// author rename is not tracked.
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

// titleResolveTimeout bounds the whole resolveTitle chain (bot token read,
// conversations.info, the author lookups, the store write) regardless of what
// ctx a caller passes in — mirrors permalinkResolveTimeout's rationale
// (permalink.go). Three sequential API calls at worst — the summons arm's
// conversations.replies, then users.info, after conversations.info — fit
// comfortably.
const titleResolveTimeout = 10 * time.Second

// entityTitleUpdater is the narrow slice of db.EntityStore the title resolver
// needs. Declared locally (mirrors permalink.go's entityURLUpdater) so tests
// can supply a single-method fake; db.EntityStore satisfies it structurally,
// so the production wiring (install.go) needs no adapter.
type entityTitleUpdater interface {
	UpdateTitleSystem(ctx context.Context, orgID, entityID, title string) (domain.Entity, error)
}

// threadTitleRef names the thread whose title is being resolved: the entity
// to write, the channel it lives in, and enough to find who started it.
// isRoot picks the author arm — true when the mention IS the thread's root
// message, so mentionUser wrote it and no lookup is needed; false on a
// mid-thread summons, where the root is someone else's message and rootTS
// finds it. Those two users differ exactly when isRoot is false, which is why
// the mentioning user alone can't answer this.
type threadTitleRef struct {
	entityID    string
	channel     string
	rootTS      string
	mentionUser string
	isRoot      bool
}

// TitleResolver resolves a Slack thread's author + channel display names and
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

// dispatch starts resolveTitle for (ws.OrgID, ref.entityID) in a detached
// goroutine, unless a resolution for that entity is already in flight — the
// same guard PermalinkResolver.dispatch uses, though in practice a given
// entity only dispatches once (only the FIRST mention on a thread, the one
// that creates the entity, triggers this; see ingest.go's handleEventCallback).
// Callers should call this instead of spawning resolveTitle directly.
func (r *TitleResolver) dispatch(ws slackstore.Workspace, ref threadTitleRef) {
	key := ws.OrgID + "/" + ref.entityID
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
		r.resolveTitle(context.Background(), ws, ref)
	}()
}

// resolveTitle resolves the channel and author display names and writes the
// composed title. Best-effort: every failure is logged and swallowed, there
// is no error return, and it MUST NEVER run inline on the ingest request
// path — callers dispatch it via dispatch (above), never directly. When the
// channel name doesn't resolve, composeThreadTitle yields the anonymous title
// already on the entity whatever the author turns out to be, so the resolution
// stops there rather than spending the author's round-trips to learn that the
// write is redundant.
func (r *TitleResolver) resolveTitle(ctx context.Context, ws slackstore.Workspace, ref threadTitleRef) {
	ctx, cancel := context.WithTimeout(ctx, titleResolveTimeout)
	defer cancel()

	botToken, err := r.secrets.GetSystem(ctx, ws.OrgID, ws.BotTokenRef)
	if err != nil {
		slackLog.Warn("title: bot token read failed", "workspace", ws.WorkspaceID, "entity", ref.entityID, "error", err)
		return
	}
	if botToken == "" {
		slackLog.Warn("title: no bot token configured", "workspace", ws.WorkspaceID)
		return
	}

	var channelName string
	if info, err := slackConversationsInfo(ctx, r.client, botToken, ref.channel); err != nil {
		slackLog.Warn("title: conversations.info failed", "workspace", ws.WorkspaceID, "channel", ref.channel, "error", err)
	} else {
		channelName = info.Name
	}
	if channelName == "" {
		return
	}

	title := composeThreadTitle(r.resolveAuthorName(ctx, ws, botToken, ref), channelName)
	if _, err := r.entities.UpdateTitleSystem(ctx, ws.OrgID, ref.entityID, title); err != nil {
		slackLog.Warn("title: update failed", "workspace", ws.WorkspaceID, "entity", ref.entityID, "error", err)
	}
}

// resolveAuthorName resolves the display name of the human who started the
// thread, or "" when there isn't one to name — which composeThreadTitle turns
// into the channel-only title rather than a failure.
//
// On the root arm the mentioning user authored the root, so their id is
// already in hand. On the summons arm the root is someone else's message,
// fetched with a SINGLE conversations.replies page: Slack returns a thread's
// root first on every page, so one request answers it (the same reason
// resolveMessageRootTS reads only msgs[0]). Private channels need
// groups:history for that call, which the shipped manifest grants alongside
// channels:history.
//
// A root Slack attributes to no human — a bot post, which carries bot_id and
// an empty user — has no author, and neither does a bot or deactivated
// profile; each falls back rather than naming an app on the card.
func (r *TitleResolver) resolveAuthorName(ctx context.Context, ws slackstore.Workspace, botToken string, ref threadTitleRef) string {
	authorID := ref.mentionUser
	if !ref.isRoot {
		msgs, _, _, err := slackConversationsRepliesPage(ctx, r.client, botToken, ref.channel, ref.rootTS, 1, "")
		if err != nil {
			slackLog.Warn("title: conversations.replies failed", "workspace", ws.WorkspaceID, "entity", ref.entityID, "error", err)
			return ""
		}
		if len(msgs) == 0 {
			return ""
		}
		authorID = msgs[0].User
	}
	if authorID == "" {
		return ""
	}

	info, err := slackUsersInfo(ctx, r.client, botToken, authorID)
	if err != nil {
		slackLog.Warn("title: users.info failed", "workspace", ws.WorkspaceID, "slack_user", authorID, "error", err)
		return ""
	}
	if info.IsBot || info.Deleted {
		return ""
	}
	// Same precedence the identity resolver renders a Slack user by
	// (identity.go): the chosen handle first, the profile's real name behind it.
	return nonEmpty(info.DisplayName, info.RealName)
}
