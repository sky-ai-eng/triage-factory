package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// workspacesHandler serves /api/orgs/{org_id}/slack/connections/* and
// /api/orgs/{org_id}/slack/manifest — the org-admin connect, switch, and
// disconnect lifecycle of an org's Slack connections plus the member-visible
// reads. Every route is addressed by the org in its path and gated per request
// (see memberGate): a local-mode 404, the caller's standing in that org, and the
// `slack` entitlement. The local-mode 404 is narrower than SSO's runmode gate
// (which exists because SSO structurally can't work without GoTrue); ours
// exists because the Slack store is Postgres-only, so a local request that got
// past the entitlement check would hit db.ErrNotApplicableInLocal as a 500.
type workspacesHandler struct {
	tx     db.TxRunner
	az     *authz.Checker
	client *http.Client
	// publicURL returns the deployment's externally-visible base, used to
	// build the events_api manifest's request_url.
	publicURL func() string
	// sockets reports live Socket Mode connection status for handleList's
	// response. nil in tests that don't exercise it (TestHandlers_LocalMode404
	// constructs a zero-value handler) — toWorkspaceView is nil-safe.
	sockets *socketManager
	// deployment is the deployment's own Slack app, the zero app when there
	// is none. Connect refuses its bot token, because a pasted-token
	// connection is always an org's own app.
	deployment deploymentApp
	// stores is the admin-pool aggregate the switch reads through: a
	// connection's bot token, and every team's watched channels, which the
	// tracking rows' RLS hides from an admin outside the team.
	stores db.Stores
	// channels enumerates a workspace's channels with one connection's bot
	// token, joins channels, and holds the org's cached channel candidates,
	// for the switch (activation.go).
	channels *channelsHandler
}

type workspaceView struct {
	WorkspaceID   string `json:"workspace_id" doc:"The Slack workspace's id."`
	APIAppID      string `json:"api_app_id" doc:"The Slack app's id."`
	Source        string `json:"source" doc:"Whose Slack app the connection is through: org for an app the organization created, or deployment for the deployment's own app."`
	Active        bool   `json:"active" doc:"Whether this is the connection the organization uses in the workspace, which is the one whose events are processed."`
	WorkspaceName string `json:"workspace_name" doc:"The Slack workspace's name."`
	AppName       string `json:"app_name" doc:"The Slack app's bot name, empty when Slack gave none."`
	EnterpriseID  string `json:"enterprise_id,omitempty" doc:"The id of the Enterprise Grid organization the workspace belongs to, absent for a standalone workspace."`
	// Transport is null on a deployment connection, which receives through
	// the transport the operator's environment configures.
	Transport          *string   `json:"transport" doc:"How Slack delivers the app's events, socket or events_api, or null on a deployment connection, which uses the transport the deployment configures."`
	BotUserID          string    `json:"bot_user_id" doc:"The Slack user id of the app's bot."`
	RegisteredByUserID string    `json:"registered_by_user_id,omitempty" doc:"The id of the person who connected the app, absent when that person has since been removed."`
	CreatedAt          time.Time `json:"created_at" doc:"When the connection was created, as an RFC 3339 timestamp."`
	UpdatedAt          time.Time `json:"updated_at" doc:"When the connection was last changed, as an RFC 3339 timestamp."`
	// Connection is the app's live Socket Mode status, when one exists —
	// omitted for a webhook-only app or before the reconciler's first pass.
	// Rows sharing an app report the same
	// connection (socket_conn.go's connections are per-app, not per-row).
	Connection *connectionView `json:"connection,omitempty" doc:"The app's live Socket Mode connection status, absent for an app that receives events over HTTP and before the first connection attempt."`
}

// connectionView is a Socket Mode connection's status as the connection reads
// show it. For a connection through the deployment's app it is the state
// alone: that socket carries every org's traffic, so its timestamps, failures,
// and sightings describe other orgs as well as the reader's.
type connectionView struct {
	status    connStatus
	stateOnly bool
}

func (v connectionView) MarshalJSON() ([]byte, error) {
	if v.stateOnly {
		return json.Marshal(struct {
			State string `json:"state"`
		}{v.status.State})
	}
	return json.Marshal(v.status)
}

// OpenAPISchema describes both shapes MarshalJSON writes. A deployment
// connection's socket carries every organization's events, so its view holds
// the state alone.
func (connectionView) OpenAPISchema() map[string]any {
	timestamp := func(doc string) map[string]any {
		return map[string]any{"type": "string", "format": "date-time", "description": doc}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"state": map[string]any{"type": "string",
				"description": "The connection's state: dialing, open, draining, backing_off, or auth_failed."},
			"connected_at":  timestamp("When the connection last opened, as an RFC 3339 timestamp, absent when it has not."),
			"last_event_at": timestamp("When the connection last received an event, as an RFC 3339 timestamp, absent when it has not."),
			"consecutive_failures": map[string]any{"type": "integer",
				"description": "How many connection attempts in a row have failed."},
			"last_error": map[string]any{"type": "string",
				"description": "The error of the last failed attempt, absent when there is none."},
			"sightings": map[string]any{
				"type":        "array",
				"description": "Workspaces the app received events from that have no connection in the organization, absent when there are none.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"team_id":   map[string]any{"type": "string", "description": "The Slack workspace's id."},
						"count":     map[string]any{"type": "integer", "description": "How many events arrived from the workspace."},
						"last_seen": timestamp("When the last of them arrived, as an RFC 3339 timestamp."),
					},
					"required": []string{"team_id", "count", "last_seen"},
				},
			},
		},
		"required": []string{"state"},
	}
}

func toWorkspaceView(w *slackstore.Connection, sockets *socketManager) workspaceView {
	v := workspaceView{
		WorkspaceID:        w.WorkspaceID,
		APIAppID:           w.APIAppID,
		Source:             w.Source,
		Active:             w.Active,
		WorkspaceName:      w.WorkspaceName,
		AppName:            w.AppName,
		EnterpriseID:       w.EnterpriseID,
		BotUserID:          w.BotUserID,
		RegisteredByUserID: w.RegisteredByUserID,
		CreatedAt:          w.CreatedAt,
		UpdatedAt:          w.UpdatedAt,
	}
	if w.Transport != "" {
		transport := w.Transport
		v.Transport = &transport
	}
	if sockets != nil {
		if status, ok := sockets.StatusFor(w.APIAppID); ok {
			v.Connection = &connectionView{status: status, stateOnly: w.Source == slackstore.SourceDeployment}
		}
	}
	return v
}

// memberGate runs the gates a member-readable org route runs, in order: local
// mode 404s, the caller must be a member of the org the path names (core's
// {org_id} gate, which also answers 404 to an API token minted for another
// org), and that org must hold the `slack` entitlement, else 404.
func (h *workspacesHandler) memberGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if refuseInLocalMode(w) {
		return "", "", false
	}
	if orgID, userID, ok = h.az.RequireOrgMember(w, r); !ok {
		return "", "", false
	}
	if refuseUnentitled(w, orgID) {
		return "", "", false
	}
	return orgID, userID, true
}

// adminGate is memberGate with the caller an admin of the path's org: 403 to a
// member who is not one, 404 to anyone else. Every route that changes an org's
// connections, or reads the manifest, runs it.
func (h *workspacesHandler) adminGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if refuseInLocalMode(w) {
		return "", "", false
	}
	if orgID, userID, ok = h.az.RequireOrgAdmin(w, r); !ok {
		return "", "", false
	}
	if refuseUnentitled(w, orgID) {
		return "", "", false
	}
	return orgID, userID, true
}

// connectionsListRequest is POST …/connections/list's body: the paging fields
// and nothing else.
type connectionsListRequest struct {
	httpx.PageRequest
}

// connectionsListKey is the list's page-token fingerprint. It names the org,
// so a token one org's list minted is refused on another's.
type connectionsListKey struct {
	List  string `json:"list"`
	OrgID string `json:"org_id"`
}

// slackAccess is the sentence every Slack route's description ends with,
// but the deployment app's, which no organization's licence gates.
const slackAccess = " It needs an Enterprise licence that includes Slack, and is available only in multi mode."

var opList = httpx.NewListOp[connectionsListRequest, workspaceView](httpx.OpSpec{
	Pattern:     "POST /api/orgs/{org_id}/slack/connections/list",
	ID:          "list_slack_connections",
	Summary:     "List Slack connections",
	Description: "Returns one page of the organization's Slack connections, active and inactive, ordered by workspace name, then workspace id, then app id. Each connection carries its Socket Mode status when it has one." + slackAccess,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleList answers one page of the org's connections, active and inactive,
// ordered by workspace name, then workspace id, then app id. Any member of the
// org may read it.
//
// POST /api/orgs/{org_id}/slack/connections/list
func (h *workspacesHandler) handleList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.memberGate(w, r)
	if !ok {
		return
	}

	req, ok := opList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(connectionsListKey{List: "slack_connections", OrgID: orgID}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		list  []slackstore.Connection
		total int
	)
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		list, total, e = slackstore.FromTx(tx).Connections.ListForOrg(r.Context(), orgID,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}

	out := make([]workspaceView, len(list))
	for i := range list {
		out[i] = toWorkspaceView(&list[i], h.sockets)
	}
	opList.Write(w, page, out, total)
}

var opGet = httpx.NewJSONOp[httpx.NoBody, workspaceView](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}",
	ID:          "get_slack_connection",
	Summary:     "Get a Slack connection",
	Description: "Returns one of the organization's Slack connections, by the workspace and the app it connects, with its Socket Mode status when it has one." + slackAccess,
	PathParams: map[string]string{
		"org_id":       "The organization's id.",
		"workspace_id": "The Slack workspace's id.",
		"api_app_id":   "The Slack app's id.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleGet reads one of the org's connections, by the workspace and the app
// it connects. Any member of the org may read it.
//
// GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}
func (h *workspacesHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.memberGate(w, r)
	if !ok {
		return
	}
	conn, err := h.getExisting(r.Context(), orgID, userID, r.PathValue("workspace_id"), r.PathValue("api_app_id"))
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if conn == nil {
		httpx.NotFound(w, "connection")
		return
	}
	opGet.Write(w, toWorkspaceView(conn, h.sockets))
}

// getExisting reads the org's current row for (workspaceID, apiAppID), or
// (nil, nil) when none exists in THIS org (a (workspace, app) pair owned by
// a different org is invisible here — RLS plus the explicit org_id filter —
// which is exactly right: the merge logic below must never see another
// org's stored refs).
func (h *workspacesHandler) getExisting(ctx context.Context, orgID, userID, workspaceID, apiAppID string) (*slackstore.Connection, error) {
	var ws *slackstore.Connection
	if err := h.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ws, e = slackstore.FromTx(tx).Connections.Get(ctx, orgID, workspaceID, apiAppID)
		return e
	}); err != nil {
		return nil, err
	}
	return ws, nil
}

// mergeCredentialPresence implements the "leave blank to keep current"
// token-field convention (CLAUDE.md) for one credential: a non-blank
// submission always wins; a blank submission falls back to whether the org
// already has one stored. keep=true tells the caller NOT to re-Put the
// secret — nothing changed, the stored value stays exactly as it was.
func mergeCredentialPresence(submitted, alreadyStored bool) (has, keep bool) {
	if submitted {
		return true, false
	}
	return alreadyStored, alreadyStored
}

func refFor(has bool, ref string) string {
	if has {
		return ref
	}
	return ""
}

// errAppBoundToOtherOrg is the sentinel the connect handler's tx closure
// returns when AppBoundToOtherOrgSystem finds the derived api_app_id
// already owned by a different org — mapped to a generic 409 below, never
// naming the owning org (the app-single-org invariant).
var errAppBoundToOtherOrg = errors.New("slack app already connected to a different org")

// connectionImportRequest is the body of POST …/slack/connections/import. A blank
// signing_secret or app_token keeps the one stored for the same workspace and
// app, and the transport is inferred from which of the two the connection
// then holds.
type connectionImportRequest struct {
	BotToken      string `json:"bot_token" doc:"The app's bot token, which starts xoxb-, from which the workspace and the app are read. Required."`
	SigningSecret string `json:"signing_secret" doc:"The app's signing secret, which an app that receives events over HTTP needs; blank or absent keeps the one stored for the same workspace and app."`
	AppToken      string `json:"app_token" doc:"An app-level token with the connections:write scope, which starts xapp- and which an app that receives events over Socket Mode needs; blank or absent keeps the one stored for the same workspace and app."`
	Transport     string `json:"transport" doc:"How the app receives events, socket or events_api. It is read only when the connection holds both a signing secret and an app-level token, and is required then; otherwise the transport is the one whose credential the connection holds."`
}

var opConnect = httpx.NewJSONOp[connectionImportRequest, workspaceView](httpx.OpSpec{
	Pattern:       "POST /api/orgs/{org_id}/slack/connections/import",
	ID:            "import_slack_connection",
	Summary:       "Connect the organization's own Slack app",
	Description:   "Connects an organization's own Slack app from its tokens, which are checked with Slack, and returns the connection. A new connection answers 201, and is stored inactive when the workspace already has an active connection of another app; connecting an app the workspace already has replaces its stored tokens and answers 200." + slackAccess,
	PathParams:    map[string]string{"org_id": "The organization's id."},
	Status:        http.StatusCreated,
	OtherStatuses: []int{http.StatusOK},
	Hints:         httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintYes},
})

// handleConnect connects an org's own Slack app from the tokens an org admin
// pasted. In a workspace where the org already has an active connection of
// another app, the new connection is stored inactive: the first step of a
// switch, which activate completes. Connecting an app the workspace already
// has keeps that connection's active flag.
//
// POST /api/orgs/{org_id}/slack/connections/import  body: {bot_token, signing_secret?, app_token?, transport?}
func (h *workspacesHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	body, ok := opConnect.Decode(w, r)
	if !ok {
		return
	}
	botToken := strings.TrimSpace(body.BotToken)
	signingSecret := strings.TrimSpace(body.SigningSecret)
	appToken := strings.TrimSpace(body.AppToken)
	// The bot and app-level tokens' shapes are checked before any call to
	// Slack, so a token of the wrong kind is refused by field rather than by
	// whatever Slack answers for it. No message echoes a token. A missing bot
	// token makes the response a 400, the status of a missing field; shape
	// faults alone are a 422, the status of the other bot token refusals here.
	var v httpx.Validation
	faultStatus := http.StatusUnprocessableEntity
	switch {
	case botToken == "":
		v.Missing("bot_token")
		faultStatus = http.StatusBadRequest
	case isRotatingBotToken(botToken):
		// TF stores a bot token once and never refreshes it, so a token that
		// expires in 12 hours would leave a connection that stops working.
		v.Invalid("bot_token", "This bot token expires, because its Slack app has token rotation turned on. Triage Factory needs an app without token rotation, and Slack does not allow turning it off, so create a new app from the manifest.")
	case !isBotToken(botToken):
		v.Invalid("bot_token", "This is not a bot token. Copy the Bot User OAuth Token, which starts xoxb-, from your Slack app's OAuth & Permissions page.")
	}
	if appToken != "" && !isAppLevelToken(appToken) {
		v.Invalid("app_token", "This is not an app-level token. Copy a token with the connections:write scope, which starts xapp-, from your Slack app's Basic Information → App-Level Tokens.")
	}
	if v.Flush(w, faultStatus) {
		return
	}

	// The bot token is the ONLY way the handler learns the workspace id —
	// the admin never types it. Always re-validated: there's no "keep the
	// current bot token" path. The tokens checked here are not stored yet, so
	// their calls name no credential.
	caller := slackCaller{orgID: orgID}
	result, err := slackAuthTest(r.Context(), h.client, caller, botToken)
	if err != nil {
		httpx.BadRequest(w, "could not validate the bot token with Slack: "+err.Error())
		return
	}

	// The app id is likewise never typed by the admin — derived from the
	// same bot token via auth.test's bot_id -> bots.info's app_id. It's key
	// material (the app-single-org invariant), so a failure here is fatal:
	// no fallback, connect refused.
	botsInfo, err := slackBotsInfo(r.Context(), h.client, caller, botToken, result.BotID)
	if err != nil {
		httpx.BadRequest(w, "could not resolve the app id for this bot token: "+err.Error())
		return
	}
	apiAppID := botsInfo.AppID
	if h.deployment.Configured() && apiAppID == h.deployment.AppID {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "This bot token belongs to this deployment's Slack app. Connect it with Add to Slack instead.",
			Field:   "bot_token",
		})
		return
	}

	existing, err := h.getExisting(r.Context(), orgID, userID, result.TeamID, apiAppID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}

	hasSigningSecret, keepSigningSecret := mergeCredentialPresence(signingSecret != "", existing != nil && existing.SigningSecretRef != "")
	hasAppToken, keepAppToken := mergeCredentialPresence(appToken != "", existing != nil && existing.AppTokenRef != "")

	transport, err := inferTransport(hasSigningSecret, hasAppToken, strings.TrimSpace(body.Transport))
	if err != nil {
		httpx.BadRequest(w, err.Error())
		return
	}

	// Only a FRESHLY submitted app token can be live-validated — a kept
	// (blank + already stored) token has no plaintext available here, and
	// re-validating stored credentials on every unrelated edit isn't this
	// leaf's job.
	if transport == transportSocket && appToken != "" {
		if err := slackOpenConnection(r.Context(), h.client, caller, appToken); err != nil {
			httpx.BadRequest(w, "could not validate the app-level token with Slack: "+err.Error())
			return
		}
	}

	keys := integrations.SlackConnectionKeysFor(result.TeamID, apiAppID)
	ws := slackstore.Connection{
		WorkspaceID:        result.TeamID,
		APIAppID:           apiAppID,
		OrgID:              orgID,
		Source:             slackstore.SourceOrg,
		WorkspaceName:      result.Team,
		AppName:            botsInfo.Name,
		EnterpriseID:       result.EnterpriseID,
		Transport:          transport,
		BotUserID:          result.UserID,
		BotTokenRef:        keys.BotToken,
		SigningSecretRef:   refFor(hasSigningSecret, keys.SigningSecret),
		AppTokenRef:        refFor(hasAppToken, keys.AppToken),
		RegisteredByUserID: userID,
	}

	var persisted slackstore.Connection
	write := func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		// LockApp MUST run before the AppBoundToOtherOrgSystem check below
		// — on its own, that check-then-Upsert sequence is a race: two
		// concurrent connects for the same api_app_id under different orgs
		// write different workspace_id rows, so they never collide on a
		// unique index, and each could observe "not bound" before either
		// commits. The advisory lock serializes every connect attempt for
		// this api_app_id so that can't happen — see LockApp's doc.
		if e := bundle.Connections.LockApp(r.Context(), apiAppID); e != nil {
			return e
		}
		// The app-single-org invariant has no SQL constraint behind it
		// (api_app_id is only unique in combination with workspace_id), so
		// it's enforced here, before any write — see
		// AppBoundToOtherOrgSystem's doc.
		boundElsewhere, e := bundle.Connections.AppBoundToOtherOrgSystem(r.Context(), apiAppID, orgID)
		if e != nil {
			return e
		}
		if boundElsewhere {
			return errAppBoundToOtherOrg
		}
		conns, _, e := bundle.Connections.ListForOrg(r.Context(), orgID, db.Unwindowed)
		if e != nil {
			return e
		}
		own, active := connectionsInWorkspace(conns, result.TeamID, apiAppID)
		// Upsert reads Active only when it inserts: a row this app already
		// has keeps its own. It inserts for an own row too when a disconnect
		// deleted that row after this read, and then the row was the active
		// one. A concurrent write making another app active in the workspace,
		// which this read cannot see, is refused by the one-active index
		// (slackstore.ErrActiveConnectionExists), and the second pass below
		// stores this one inactive.
		ws.Active = active == nil || active == own

		if err := tx.Secrets.Put(r.Context(), orgID, keys.BotToken, botToken, "Slack bot token"); err != nil {
			return fmt.Errorf("put bot token: %w", err)
		}
		if !keepSigningSecret && signingSecret != "" {
			if err := tx.Secrets.Put(r.Context(), orgID, keys.SigningSecret, signingSecret, "Slack signing secret"); err != nil {
				return fmt.Errorf("put signing secret: %w", err)
			}
		}
		if !keepAppToken && appToken != "" {
			if err := tx.Secrets.Put(r.Context(), orgID, keys.AppToken, appToken, "Slack app-level token"); err != nil {
				return fmt.Errorf("put app token: %w", err)
			}
		}
		if persisted, e = bundle.Connections.Upsert(r.Context(), ws); e != nil {
			return e
		}
		// Unconditional, including on a re-connect: the bot token is always
		// re-submitted and re-Put above (there is no keep-current path for it),
		// so every successful connect really is a bind or a rotation. The
		// signing secret and app token may have been kept, but they are part of
		// the same workspace credential — the row records the workspace, not
		// which of its three secrets moved.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  domain.AccessDetailSlackWorkspace(result.Team, result.TeamID, apiAppID),
		}); err != nil {
			return fmt.Errorf("audit slack connect: %w", err)
		}
		return nil
	}
	err = h.tx.WithTx(r.Context(), orgID, userID, write)
	if errors.Is(err, slackstore.ErrActiveConnectionExists) {
		// Another app became the workspace's active connection after this
		// transaction read the workspace. Its commit is visible now, so a
		// second pass stores this one inactive.
		err = h.tx.WithTx(r.Context(), orgID, userID, write)
	}
	if err != nil {
		if errors.Is(err, errAppBoundToOtherOrg) {
			// Deliberately generic: the caller may learn "already
			// connected", never which org holds it (the app-single-org
			// invariant).
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this app is already connected"})
			return
		}
		if errors.Is(err, slackstore.ErrActiveConnectionExists) {
			// The second pass met yet another change to which app is
			// active; nothing was stored.
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonConflict,
				Message: "This Slack workspace's active connection changed while connecting; try again.",
			})
			return
		}
		if httpx.IsUniqueViolation(err) {
			// The (workspace, app) composite PK's backstop — see Upsert's
			// doc. Same non-disclosure posture as the app-single-org 409
			// above, distinct wording since it's the narrower conflict.
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this workspace+app is already connected"})
			return
		}
		httpx.InternalError(w, "slack", err)
		return
	}
	notifyConfigChanged(orgID)
	slackLog.Info("slack workspace connected", "org", orgID, "workspace", result.TeamID, "app", apiAppID, "transport", transport)

	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	opConnect.WriteStatus(w, status, toWorkspaceView(&persisted, h.sockets))
}

// activeConnections returns the active members of conns, in order: the
// connections outbound calls use, at most one per (org, workspace).
func activeConnections(conns []slackstore.Connection) []slackstore.Connection {
	var out []slackstore.Connection
	for _, c := range conns {
		if c.Active {
			out = append(out, c)
		}
	}
	return out
}

// connectionsInWorkspace finds, among conns, the connection of apiAppID in
// workspaceID and the workspace's active connection; either is nil when there
// is none.
func connectionsInWorkspace(conns []slackstore.Connection, workspaceID, apiAppID string) (own, active *slackstore.Connection) {
	for i := range conns {
		c := &conns[i]
		if c.WorkspaceID != workspaceID {
			continue
		}
		if c.APIAppID == apiAppID {
			own = c
		}
		if c.Active {
			active = c
		}
	}
	return own, active
}

// connectionDeleteResponse is the body of DELETE on a connection.
type connectionDeleteResponse struct {
	Status string `json:"status" doc:"Always deleted."`
}

var opDelete = httpx.NewJSONOp[httpx.NoBody, connectionDeleteResponse](httpx.OpSpec{
	Pattern:     "DELETE /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}",
	ID:          "delete_slack_connection",
	Summary:     "Disconnect a Slack connection",
	Description: "Disconnects one of the organization's Slack connections and deletes its stored tokens. The app stays installed in Slack, and deleting a workspace's active connection leaves the workspace with none." + slackAccess,
	PathParams: map[string]string{
		"org_id":       "The organization's id.",
		"workspace_id": "The Slack workspace's id.",
		"api_app_id":   "The Slack app's id.",
	},
	Hints: httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleDelete disconnects one of the org's connections: TF forgets its row and
// its secrets, and the app stays installed in Slack, for the workspace's admin
// to remove there. A deployment connection is deleted the same way. Deleting a
// workspace's active connection leaves the workspace with none, whatever else
// is connected in it: a connection becomes active only by being activated.
//
// DELETE /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}
func (h *workspacesHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	workspaceID, apiAppID, ok := connectionFromPath(w, r)
	if !ok {
		return
	}

	var existed bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		ws, e := bundle.Connections.Get(r.Context(), orgID, workspaceID, apiAppID)
		if e != nil {
			return e
		}
		if ws == nil {
			return nil
		}
		existed = true
		if e := bundle.Connections.Delete(r.Context(), orgID, workspaceID, apiAppID); e != nil {
			return fmt.Errorf("delete workspace row: %w", e)
		}
		// Named from the row we just read, not from the path params: the
		// workspace name is what the log is read for, and after this tx there
		// is nothing left to resolve it from. Guarded by the ws == nil return
		// above, so a delete of an absent workspace records no phantom
		// revocation.
		if e := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  domain.AccessDetailSlackWorkspace(ws.WorkspaceName, workspaceID, apiAppID),
		}); e != nil {
			return fmt.Errorf("audit slack disconnect: %w", e)
		}
		// Sweep the full keyset regardless of which refs this (workspace,
		// app) pair actually populated — a deployment connection holds only
		// its bot token, and deleting an absent key is a no-op. Mirrors
		// teardownAppSecrets for GitHub Apps.
		for _, k := range integrations.SlackConnectionKeysFor(workspaceID, apiAppID).All() {
			if _, e := tx.Secrets.Delete(r.Context(), orgID, k); e != nil {
				return fmt.Errorf("delete slack secret %s: %w", k, e)
			}
		}
		return nil
	}); err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}
	if !existed {
		httpx.NotFound(w, "connection")
		return
	}

	notifyConfigChanged(orgID)
	slackLog.Info("slack workspace disconnected", "org", orgID, "workspace", workspaceID, "app", apiAppID)
	opDelete.Write(w, connectionDeleteResponse{Status: "deleted"})
}

var opManifest = httpx.NewJSONOp[httpx.NoBody, slackManifest](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/slack/manifest",
	ID:          "get_slack_manifest",
	Summary:     "Get the manifest for the organization's Slack app",
	Description: "Returns the Slack app manifest to create the organization's own Slack app from, for the transport given. An events_api manifest names the deployment's public URL, so it needs one configured." + slackAccess,
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Query: []httpx.QueryParam{{
		Name: "transport", Type: httpx.ParamString,
		Doc: "How the app is to receive events: socket for Socket Mode, or events_api for HTTP requests to the deployment. Required.",
	}},
	Hints: httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleManifest answers the manifest an org admin creates the org's own Slack
// app from.
//
// GET /api/orgs/{org_id}/slack/manifest?transport=socket|events_api
func (h *workspacesHandler) handleManifest(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	q, ok := opManifest.Query(w, r)
	if !ok {
		return
	}
	transport := q.Get("transport")
	if transport != transportSocket && transport != transportEventsAPI {
		httpx.BadRequest(w, `transport must be "socket" or "events_api"`)
		return
	}
	publicURL := h.publicURL()
	if transport == transportEventsAPI && publicURL == "" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "set TF_PUBLIC_URL / deploy config first"})
		return
	}

	var orgName string
	if err := h.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		org, e := tx.Orgs.GetOrg(r.Context(), orgID)
		if e != nil {
			return e
		}
		if org != nil {
			orgName = org.Name
		}
		return nil
	}); err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}

	opManifest.Write(w, buildSlackManifest(orgName, transport, publicURL, orgID))
}

// notifyConfigChanged signals the socket connection manager to reconcile
// now (a row was connected or disconnected) rather than waiting for its
// next socketReconcileInterval tick. Deliberately NOT core reloader
// plumbing (SetOnGitHubChanged-style hooks are for core subsystems): both
// the producer (here) and the consumer (socket.go's socketManager.run) live
// entirely inside ee/slack. orgID is unused — reconcile always recomputes
// the full desired set (one ListAllSystem scan) rather than an org-scoped
// delta, so there's nothing org-specific to signal; kept as a parameter so
// call sites read as "this org's config changed" without every caller
// needing to know that reconcile happens to be global.
func notifyConfigChanged(orgID string) {
	_ = orgID
	select {
	case socketConfigChanged <- struct{}{}:
	default:
	}
}
