package slack

// Switching a workspace between the org's connected apps. The preview says
// what an activation would change and hands back a fingerprint of the stored
// state it read; the activation refuses unless that state is unchanged, then
// makes the target the workspace's active connection and joins its bot to the
// watched channels.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// A watched channel's status under the target's bot.
const (
	// channelMember: the bot is in the channel.
	channelMember = "member"
	// channelWillJoin: the channel is public and the bot is not in it;
	// activate joins it.
	channelWillJoin = "will_join"
	// channelNeedsInvite: the bot is not in the channel and cannot see it as
	// public, so someone invites it in Slack.
	channelNeedsInvite = "needs_invite"
	// channelUnknown: the enumeration stopped at slackConversationsListCap
	// before reaching the channel, or a blocker stopped it from running.
	channelUnknown = "unknown"
)

// What stops an activation.
const (
	// blockerTokenRejected: Slack refused the target's bot token.
	blockerTokenRejected = "token_rejected"
	// blockerMissingScopes: the token lacks scopes the app needs.
	blockerMissingScopes = "missing_scopes"
	// blockerAppTokenRejected: Slack refused the app-level token an org's own
	// Socket Mode app receives events with, so once active the workspace would
	// receive nothing.
	blockerAppTokenRejected = "app_token_rejected"
)

// noteHistoryRateLimit is the preview's note on switching to the deployment's
// app.
const noteHistoryRateLimit = "history_rate_limit"

// historyRateLimitMessage is noteHistoryRateLimit's message.
const historyRateLimitMessage = "Slack limits conversations.history and conversations.replies to one call a minute " +
	"for an app distributed outside the Slack Marketplace. Triage Factory cannot tell whether this deployment's app " +
	"is approved there; until it is, reading threads through it is slow."

// staleActivationMessage is activate's refusal when what the preview read has
// changed.
const staleActivationMessage = "the preview is out of date"

// activationChannel is one watched channel in a preview.
type activationChannel struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

// activationBlocker is one reason the target cannot be activated. Scopes is
// set on missing_scopes only.
type activationBlocker struct {
	Code   string   `json:"code"`
	Scopes []string `json:"scopes,omitempty"`
}

// activationNote is something the preview tells the caller that does not stop
// the activation.
type activationNote struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// activationPreview is GET …/activation-preview's answer. From is null when
// the workspace has no active connection.
type activationPreview struct {
	From        *workspaceView      `json:"from"`
	To          workspaceView       `json:"to"`
	Channels    []activationChannel `json:"channels"`
	Blockers    []activationBlocker `json:"blockers"`
	Notes       []activationNote    `json:"notes"`
	Fingerprint string              `json:"fingerprint"`
}

// activateRequest is POST …/activate's body. Fingerprint is a pointer so an
// absent field reads apart from a present one.
type activateRequest struct {
	Fingerprint *string `json:"fingerprint"`
}

// activationResult is POST …/activate's answer: the target, now active, and
// each watched channel its bot did not join.
type activationResult struct {
	Connection workspaceView     `json:"connection"`
	Warnings   []channelsWarning `json:"warnings"`
}

// Refusals the activation's transaction returns, for the handler to answer.
var (
	errActivationTargetActive = errors.New("slack: the connection to activate is already active")
	errActivationStale        = errors.New("slack: the activation preview is out of date")
)

// handleActivationPreview answers what making a connection the workspace's
// active one would change, and the fingerprint activate takes. It reads
// Slack with the target's bot token and writes nothing.
//
// GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activation-preview
func (h *workspacesHandler) handleActivationPreview(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	workspaceID, apiAppID, ok := connectionFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	target, active, err := h.readWorkspace(ctx, orgID, userID, workspaceID, apiAppID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if refuseActivationTarget(w, target) {
		return
	}
	watched, err := slackstore.FromStores(h.stores).Channels.ListWatchedInWorkspaceSystem(ctx, orgID, workspaceID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	tokens, err := h.readTargetTokens(ctx, orgID, target)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	blockers, err := h.activationBlockers(ctx, orgID, target, tokens)
	if err != nil {
		writeSlackUnreachable(w, orgID, "the token check", err)
		return
	}

	var channels []activationChannel
	switch {
	case len(blockers) > 0:
		channels = watchedChannelStatuses(watched, nil, false, true)
	case len(watched) == 0:
		channels = []activationChannel{}
	default:
		candidates, failed, truncated := h.channels.liveSlackCandidates(ctx, orgID, []slackstore.Connection{*target})
		if failed {
			writeSlackUnreachable(w, orgID, "the channel listing", nil)
			return
		}
		channels = watchedChannelStatuses(watched, candidates, truncated, false)
	}

	resp := activationPreview{
		To:          toWorkspaceView(target, h.sockets),
		Channels:    channels,
		Blockers:    blockers,
		Notes:       []activationNote{},
		Fingerprint: activationFingerprint(orgID, workspaceID, active, target, watched),
	}
	if active != nil {
		from := toWorkspaceView(active, h.sockets)
		resp.From = &from
	}
	if resp.Blockers == nil {
		resp.Blockers = []activationBlocker{}
	}
	if target.Source == slackstore.SourceDeployment {
		resp.Notes = append(resp.Notes, activationNote{Code: noteHistoryRateLimit, Message: historyRateLimitMessage})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleActivate makes a connection the workspace's active one, in place of
// the active connection it has, if any. The body's fingerprint is the one a
// preview answered; activate refuses unless the stored state the preview read
// is unchanged. The replaced connection stays connected, inactive, and its
// token and its app in Slack are left alone. After the commit the target's bot
// joins every watched public channel; a channel it does not join is a warning,
// not a failure, since the switch has been made.
//
// POST /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activate  body: {fingerprint}
func (h *workspacesHandler) handleActivate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	workspaceID, apiAppID, ok := connectionFromPath(w, r)
	if !ok {
		return
	}
	var req activateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	if req.Fingerprint == nil || *req.Fingerprint == "" {
		v.Missing("fingerprint")
	}
	if v.Flush(w, http.StatusUnprocessableEntity) {
		return
	}
	ctx := r.Context()

	target, _, err := h.readWorkspace(ctx, orgID, userID, workspaceID, apiAppID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if refuseActivationTarget(w, target) {
		return
	}
	tokens, err := h.readTargetTokens(ctx, orgID, target)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	blockers, err := h.activationBlockers(ctx, orgID, target, tokens)
	if err != nil {
		writeSlackUnreachable(w, orgID, "the token check", err)
		return
	}
	if len(blockers) > 0 {
		items := make([]httpx.ErrorItem, len(blockers))
		for i, b := range blockers {
			items[i] = httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: blockerMessage(b)}
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, items...)
		return
	}

	var (
		activated slackstore.Connection
		watched   []slackstore.Channel
		fromAppID string
	)
	err = h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		// Every activation in the workspace waits here for the one before it
		// to commit, then reads the rows it left, so the fingerprint below
		// is checked against the state this transaction writes over.
		rows, e := bundle.Connections.LockWorkspace(ctx, orgID, workspaceID)
		if e != nil {
			return e
		}
		own, active := connectionsInWorkspace(rows, workspaceID, apiAppID)
		switch {
		case own == nil:
			return slackstore.ErrNoSuchConnection
		case own.Active:
			return errActivationTargetActive
		}
		if watched, e = bundle.Channels.ListWatchedInWorkspaceSystem(ctx, orgID, workspaceID); e != nil {
			return e
		}
		if activationFingerprint(orgID, workspaceID, active, own, watched) != *req.Fingerprint {
			return errActivationStale
		}
		var fromAppName string
		if active != nil {
			fromAppID, fromAppName = active.APIAppID, active.AppName
		}
		if activated, e = bundle.Connections.Activate(ctx, orgID, workspaceID, apiAppID); e != nil {
			return e
		}
		if e := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionSlackConnectionActivated,
			DetailJSON: domain.AccessDetailSlackConnectionActivated(
				activated.WorkspaceName, workspaceID, fromAppID, fromAppName, apiAppID, activated.AppName),
		}); e != nil {
			return fmt.Errorf("audit slack activation: %w", e)
		}
		return nil
	})
	switch {
	case errors.Is(err, slackstore.ErrNoSuchConnection):
		httpx.NotFound(w, "connection")
		return
	case errors.Is(err, errActivationTargetActive):
		writeAlreadyActive(w)
		return
	case errors.Is(err, errActivationStale), errors.Is(err, slackstore.ErrActiveConnectionExists):
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: staleActivationMessage})
		return
	case err != nil:
		httpx.InternalError(w, "slack", err)
		return
	}
	slackLog.Info("slack connection activated", "org", orgID, "user", userID, "workspace", workspaceID, "from_app", fromAppID, "app", apiAppID)

	// What the bot is in decides which channels it joins: joining a private
	// channel it is already in fails, and would be reported as needing an
	// invite. A failed listing leaves every channel to the join, so a private
	// channel the bot is already in then comes back as invite_required.
	warnings := []channelsWarning{}
	if len(watched) > 0 {
		conns := []slackstore.Connection{activated}
		candidates, _, _ := h.channels.liveSlackCandidates(ctx, orgID, conns)
		warnings = append(warnings, h.channels.autoJoin(ctx, orgID, conns, candidates, watched)...)
	}
	h.channels.candidates.Forget(orgID)
	notifyConfigChanged(orgID)

	httpx.WriteJSON(w, http.StatusOK, activationResult{Connection: toWorkspaceView(&activated, h.sockets), Warnings: warnings})
}

// connectionFromPath reads the {workspace_id} and {api_app_id} a connection
// route names, answering 404 when either is empty.
func connectionFromPath(w http.ResponseWriter, r *http.Request) (workspaceID, apiAppID string, ok bool) {
	workspaceID, apiAppID = r.PathValue("workspace_id"), r.PathValue("api_app_id")
	if workspaceID == "" || apiAppID == "" {
		httpx.NotFound(w, "connection")
		return "", "", false
	}
	return workspaceID, apiAppID, true
}

// readWorkspace reads, under the caller's claims, orgID's connection of
// apiAppID in workspaceID and the workspace's active connection; either is nil
// when there is none.
func (h *workspacesHandler) readWorkspace(ctx context.Context, orgID, userID, workspaceID, apiAppID string) (target, active *slackstore.Connection, err error) {
	var conns []slackstore.Connection
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		conns, _, e = slackstore.FromTx(tx).Connections.ListForOrg(ctx, orgID, db.Unwindowed)
		return e
	}); err != nil {
		return nil, nil, err
	}
	target, active = connectionsInWorkspace(conns, workspaceID, apiAppID)
	return target, active, nil
}

// refuseActivationTarget answers 404 for a connection the org does not have
// and 409 for one that is already the workspace's active connection, and
// reports whether it answered.
func refuseActivationTarget(w http.ResponseWriter, target *slackstore.Connection) bool {
	switch {
	case target == nil:
		httpx.NotFound(w, "connection")
		return true
	case target.Active:
		writeAlreadyActive(w)
		return true
	}
	return false
}

func writeAlreadyActive(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonConflict,
		Message: "This connection is already the workspace's active one.",
	})
}

// writeSlackUnreachable answers 502 for a Slack call that met no answer about
// the token: a transport failure, a non-200, or a rate limit. The cause goes
// to the log, not the response.
func writeSlackUnreachable(w http.ResponseWriter, orgID, what string, err error) {
	slackLog.Warn("slack switch: Slack could not be reached", "org", orgID, "call", what, "error", err)
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: "Slack could not be reached; try again.",
	})
}

// targetTokens are the stored tokens an activation's checks use: the target's
// bot token, and the app-level token of an org's own app that receives over
// Socket Mode, "" for any other. A deployment connection receives through the
// operator's app and its environment, which no org's activation checks.
type targetTokens struct{ bot, app string }

// readTargetTokens reads conn's tokens. A token the row names but that is not
// stored is an error: the connect writes the row and its secrets together.
func (h *workspacesHandler) readTargetTokens(ctx context.Context, orgID string, conn *slackstore.Connection) (targetTokens, error) {
	read := func(ref, what string) (string, error) {
		v, err := h.stores.Secrets.GetSystem(ctx, orgID, ref)
		if err != nil {
			return "", fmt.Errorf("read %s of %s/%s: %w", what, conn.WorkspaceID, conn.APIAppID, err)
		}
		if v == "" {
			return "", fmt.Errorf("%s of %s/%s is not stored", what, conn.WorkspaceID, conn.APIAppID)
		}
		return v, nil
	}
	var t targetTokens
	var err error
	if t.bot, err = read(conn.BotTokenRef, "bot token"); err != nil {
		return targetTokens{}, err
	}
	if conn.Source == slackstore.SourceOrg && conn.Transport == transportSocket {
		if t.app, err = read(conn.AppTokenRef, "app-level token"); err != nil {
			return targetTokens{}, err
		}
	}
	return t, nil
}

// activationBlockers checks target's tokens with Slack. auth.test refusing the
// bot token is token_rejected, and a bot token granted fewer scopes than
// slackBotScopes is missing_scopes. apps.connections.open refusing the
// app-level token, when there is one, is app_token_rejected; the URL it mints
// is never dialed and expires unused. An error is a call that met no answer
// about a token, Slack failing on its own side included.
func (h *workspacesHandler) activationBlockers(ctx context.Context, orgID string, target *slackstore.Connection, tokens targetTokens) ([]activationBlocker, error) {
	var blockers []activationBlocker
	result, err := slackAuthTest(ctx, h.client, slackBotCaller(orgID, target.WorkspaceID, target.APIAppID), tokens.bot)
	switch {
	case isSlackRejected(err):
		blockers = append(blockers, activationBlocker{Code: blockerTokenRejected})
	case err != nil:
		return nil, err
	default:
		if missing := missingBotScopes(result.Scopes); len(missing) > 0 {
			blockers = append(blockers, activationBlocker{Code: blockerMissingScopes, Scopes: missing})
		}
	}
	if tokens.app != "" {
		err := slackOpenConnection(ctx, h.client, slackAppCaller(orgID, target.APIAppID), tokens.app)
		switch {
		case isSlackAuthError(err), isSlackRejected(err):
			blockers = append(blockers, activationBlocker{Code: blockerAppTokenRejected})
		case err != nil:
			return nil, err
		}
	}
	return blockers, nil
}

// blockerMessage is the refusal activate answers for b.
func blockerMessage(b activationBlocker) string {
	switch b.Code {
	case blockerTokenRejected:
		return "Slack rejected the bot token of the connection to switch to. Connect its app again before switching to it."
	case blockerMissingScopes:
		return "The connection to switch to lacks the Slack scopes " + strings.Join(b.Scopes, ", ") +
			". Reinstall its app with them before switching to it."
	case blockerAppTokenRejected:
		return "Slack rejected the app-level token of the connection to switch to, so it would receive no events. " +
			"Connect its app again with a working app-level token before switching to it."
	default:
		return "The connection cannot be switched to."
	}
}

// watchedChannelStatuses answers each watched channel's status under the
// target's bot, from the target's live candidates, ordered by name, then
// channel id. truncated says the enumeration stopped at its cap, so a channel
// it did not reach may be in Slack; stopped says no enumeration ran.
func watchedChannelStatuses(watched []slackstore.Channel, candidates []slackCandidate, truncated, stopped bool) []activationChannel {
	byID := make(map[string]slackCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ChannelID] = c
	}
	out := make([]activationChannel, 0, len(watched))
	for _, ch := range watched {
		item := activationChannel{ChannelID: ch.ChannelID, Name: ch.Name}
		cand, listed := byID[ch.ChannelID]
		if item.Name == "" && listed {
			item.Name = cand.Name
		}
		switch {
		case stopped:
			item.Status = channelUnknown
		case listed && cand.BotIsMember:
			item.Status = channelMember
		case listed:
			item.Status = channelWillJoin
		case truncated:
			item.Status = channelUnknown
		default:
			item.Status = channelNeedsInvite
		}
		out = append(out, item)
	}
	slices.SortStableFunc(out, func(a, b activationChannel) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ChannelID, b.ChannelID)
	})
	return out
}

// activationFingerprint is what activate checks the preview against: SHA-256,
// base64url, over a canonical encoding of the org, the workspace, the active
// connection's app id and updated_at (empty when there is none), the target's,
// and the sorted registry ids of the watched channels. It covers stored state
// only, so activate recomputes it inside its transaction with no Slack call.
// A connection's updated_at moves on every write to its row, a token rotation
// included.
func activationFingerprint(orgID, workspaceID string, from, to *slackstore.Connection, watched []slackstore.Channel) string {
	ids := make([]string, len(watched))
	for i, ch := range watched {
		ids[i] = ch.ID
	}
	slices.Sort(ids)
	state := struct {
		OrgID         string   `json:"org_id"`
		WorkspaceID   string   `json:"workspace_id"`
		FromAppID     string   `json:"from_api_app_id"`
		FromUpdatedAt string   `json:"from_updated_at"`
		ToAppID       string   `json:"to_api_app_id"`
		ToUpdatedAt   string   `json:"to_updated_at"`
		Channels      []string `json:"channels"`
	}{
		OrgID:       orgID,
		WorkspaceID: workspaceID,
		ToAppID:     to.APIAppID,
		ToUpdatedAt: to.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Channels:    ids,
	}
	if from != nil {
		state.FromAppID = from.APIAppID
		state.FromUpdatedAt = from.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(state)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
