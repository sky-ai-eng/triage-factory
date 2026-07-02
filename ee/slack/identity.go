// Slack identity capture (TFAC-531): auto-resolve the human behind an
// @mention to a TF user via a verified-email match against the
// auth-principal bridge, so a later leaf (TFAC-510/511) can grant that user
// run-visibility and render audit attribution. Capture infrastructure only
// — the sender never affects routing or ownership (the settled 3-axis
// model: visibility is channel/team, acting identity is the bot, audit is
// per-message).
//
// Best-effort, never gating: resolution is meant to run in a detached
// goroutine started AFTER the triggering event has already published, off
// context.Background() plus a short timeout — never the request context.
// Every failure here is logged and swallowed; nothing blocks or fails the
// ingest path, and the Events API's 3-second ack budget never touches a
// users.info round-trip. The one call-site TFAC-530 owns
// (handleEventCallback) is not wired here — see TFAC-531's ticket note on
// building ahead of that dependency.
package slack

import (
	"context"
	"net/http"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// slackIdentityRetryTTL bounds how often an unresolved sender re-triggers a
// users.info call. Only a DEFINITIVE no-match (zero or ambiguous verified-
// email hits, or a bot/deleted/no-email sender) writes the negative-cache
// row that starts this clock; a transient API/transport error writes
// nothing at all, so the very next mention retries immediately.
const slackIdentityRetryTTL = 24 * time.Hour

// IdentityResolver resolves the Slack sender of an @mention to a TF user,
// writing the outcome (positive or negative) into user_slack_identities.
type IdentityResolver struct {
	secrets    db.SecretStore
	users      db.UsersStore
	identities slackstore.SlackIdentityStore
	client     *http.Client
}

// NewIdentityResolver builds a resolver from the server's non-tx admin-pool
// store aggregate — the shape ee ExtensionAPI.Stores() (TFAC-528) hands
// every extension. Every store here is used only through its admin-pool
// (`...System`) methods: resolution is system-initiated, with no request
// claims context (see SlackIdentityStore's doc).
func NewIdentityResolver(stores db.Stores) *IdentityResolver {
	return &IdentityResolver{
		secrets:    stores.Secrets,
		users:      stores.Users,
		identities: slackstore.FromStores(stores).Identities,
		client:     slackHTTPClient,
	}
}

// resolveSender resolves the human behind a Slack mention (ws, slackUserID)
// to a TF user, or records that it couldn't. It is best-effort: every
// failure is logged and swallowed, there is no error return, and it MUST
// NEVER run inline on the ingest request path — callers dispatch it as
//
//	go r.resolveSender(detachedCtx, ws, ev.SenderID)
//
// off context.Background() plus a ~10s timeout, never the request ctx, so a
// slow or failing users.info round-trip can't touch the webhook response.
// Concurrent duplicate resolutions for the same sender are harmless — every
// store write here is an idempotent upsert.
func (r *IdentityResolver) resolveSender(ctx context.Context, ws slackstore.Workspace, slackUserID string) {
	if slackUserID == "" {
		return
	}

	existing, err := r.identities.LookupSystem(ctx, ws.WorkspaceID, slackUserID)
	if err != nil {
		slackLog.Warn("identity: lookup failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		return
	}
	if existing != nil {
		if existing.UserID != nil {
			return // already resolved
		}
		if time.Since(existing.LastAttemptAt) < slackIdentityRetryTTL {
			return // negative cache still warm — don't re-hit users.info yet
		}
	}

	keys := integrations.SlackWorkspaceKeysFor(ws.WorkspaceID)
	botToken, err := r.secrets.GetSystem(ctx, ws.OrgID, keys.BotToken)
	if err != nil {
		// Transient: no write, so the next mention retries.
		slackLog.Warn("identity: bot token read failed", "workspace", ws.WorkspaceID, "error", err)
		return
	}
	if botToken == "" {
		slackLog.Warn("identity: no bot token configured", "workspace", ws.WorkspaceID)
		return
	}

	info, err := slackUsersInfo(ctx, r.client, botToken, slackUserID)
	if err != nil {
		// Transient: no write, so the next mention retries.
		slackLog.Warn("identity: users.info failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		return
	}

	displayName := nonEmpty(info.DisplayName, info.RealName)
	email := strings.ToLower(strings.TrimSpace(info.Email))
	if info.IsBot || info.Deleted || email == "" {
		if err := r.identities.MarkAttemptSystem(ctx, ws.WorkspaceID, slackUserID, displayName); err != nil {
			slackLog.Warn("identity: mark attempt failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		}
		return
	}

	userIDs, err := r.users.UserIDsForVerifiedEmailSystem(ctx, email)
	if err != nil {
		// Transient: no write, so the next mention retries.
		slackLog.Warn("identity: verified-email lookup failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		return
	}

	switch len(userIDs) {
	case 1:
		if err := r.identities.UpsertResolvedSystem(ctx, ws.WorkspaceID, slackUserID, userIDs[0], displayName); err != nil {
			slackLog.Warn("identity: upsert resolved failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		}
	case 0:
		if err := r.identities.MarkAttemptSystem(ctx, ws.WorkspaceID, slackUserID, displayName); err != nil {
			slackLog.Warn("identity: mark attempt failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		}
	default:
		// Never guess on ambiguity: leave unresolved, and never log the email
		// itself — only enough to locate the row for manual investigation.
		slackLog.Warn("identity: verified email matched multiple principals, leaving unresolved",
			"workspace", ws.WorkspaceID, "slack_user", slackUserID, "matches", len(userIDs))
		if err := r.identities.MarkAttemptSystem(ctx, ws.WorkspaceID, slackUserID, displayName); err != nil {
			slackLog.Warn("identity: mark attempt failed", "workspace", ws.WorkspaceID, "slack_user", slackUserID, "error", err)
		}
	}
}
