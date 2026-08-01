package slack

import (
	"context"
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

// workspacesHandler serves /api/slack/workspaces* and /api/slack/manifest —
// the org-admin Slack workspace connect/disconnect lifecycle plus the
// member-visible list. Gates on the `slack` entitlement per-request, plus a
// local-mode 404 (see memberGate) — narrower than SSO's runmode gate (which
// exists because SSO structurally can't work without GoTrue), ours exists
// because the Slack store is Postgres-only: local mode's org-context shim
// would otherwise let a locally-licensed request past the entitlement check
// only to hit db.ErrNotApplicableInLocal as a 500. Revisit if/when local
// mode gets a real Slack story.
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
}

type workspaceView struct {
	WorkspaceID        string    `json:"workspace_id"`
	APIAppID           string    `json:"api_app_id"`
	WorkspaceName      string    `json:"workspace_name"`
	EnterpriseID       string    `json:"enterprise_id,omitempty"`
	Transport          string    `json:"transport"`
	BotUserID          string    `json:"bot_user_id"`
	RegisteredByUserID string    `json:"registered_by_user_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// Connection is the app's live Socket Mode status, when one exists —
	// omitted for a webhook-only app, an unentitled org, or before the
	// reconciler's first pass. Rows sharing an app report the same
	// connection (socket_conn.go's connections are per-app, not per-row).
	Connection *connStatus `json:"connection,omitempty"`
}

func toWorkspaceView(w *slackstore.Workspace, sockets *socketManager) workspaceView {
	v := workspaceView{
		WorkspaceID:        w.WorkspaceID,
		APIAppID:           w.APIAppID,
		WorkspaceName:      w.WorkspaceName,
		EnterpriseID:       w.EnterpriseID,
		Transport:          w.Transport,
		BotUserID:          w.BotUserID,
		RegisteredByUserID: w.RegisteredByUserID,
		CreatedAt:          w.CreatedAt,
		UpdatedAt:          w.UpdatedAt,
	}
	if sockets != nil {
		if status, ok := sockets.StatusFor(w.APIAppID); ok {
			v.Connection = &status
		}
	}
	return v
}

// memberGate resolves the active org and confirms the `slack` entitlement.
// Any org member may reach a member-gated route once past this — used by
// the read-only list. Delegates to slackEntitlementGate (channels_handler.go),
// the shared local-mode-404 + entitlement-404 gate every Slack route runs
// before its own authz — factored out so channelsHandler can share it.
func (h *workspacesHandler) memberGate(w http.ResponseWriter, r *http.Request) (orgID string, ok bool) {
	return slackEntitlementGate(w, r)
}

// adminGate is memberGate plus an org-admin check — 404 (non-disclosure) on
// a non-admin, mirroring ee/sso's adminGate. Used by every route that
// mutates connection state or reads the manifest.
func (h *workspacesHandler) adminGate(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	orgID, ok = h.memberGate(w, r)
	if !ok {
		return "", "", false
	}
	userID = httpx.ClaimsFrom(r.Context()).Subject
	isAdmin, err := h.az.UserIsOrgAdmin(r.Context(), userID, orgID)
	if err != nil {
		httpx.InternalError(w, "slack", err)
		return "", "", false
	}
	if !isAdmin {
		http.NotFound(w, r)
		return "", "", false
	}
	return orgID, userID, true
}

// GET /api/slack/workspaces
func (h *workspacesHandler) handleList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.memberGate(w, r)
	if !ok {
		return
	}
	userID := httpx.ClaimsFrom(r.Context()).Subject

	var list []slackstore.Workspace
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		list, e = slackstore.FromTx(tx).Workspaces.ListForOrg(r.Context(), orgID)
		return e
	}); err != nil {
		httpx.InternalError(w, "slack", err)
		return
	}

	out := make([]workspaceView, len(list))
	for i := range list {
		out[i] = toWorkspaceView(&list[i], h.sockets)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getExisting reads the org's current row for (workspaceID, apiAppID), or
// (nil, nil) when none exists in THIS org (a (workspace, app) pair owned by
// a different org is invisible here — RLS plus the explicit org_id filter —
// which is exactly right: the merge logic below must never see another
// org's stored refs).
func (h *workspacesHandler) getExisting(ctx context.Context, orgID, userID, workspaceID, apiAppID string) (*slackstore.Workspace, error) {
	var ws *slackstore.Workspace
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ws, e = slackstore.FromTx(tx).Workspaces.Get(ctx, orgID, workspaceID, apiAppID)
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

// POST /api/slack/workspaces  body: {bot_token, signing_secret?, app_token?, transport?}
func (h *workspacesHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}

	var body struct {
		BotToken      string `json:"bot_token"`
		SigningSecret string `json:"signing_secret"`
		AppToken      string `json:"app_token"`
		Transport     string `json:"transport"`
	}
	if !httpx.DecodeJSON(w, r, &body, "") {
		return
	}
	botToken := strings.TrimSpace(body.BotToken)
	signingSecret := strings.TrimSpace(body.SigningSecret)
	appToken := strings.TrimSpace(body.AppToken)
	if botToken == "" {
		httpx.BadRequest(w, "bot_token is required")
		return
	}

	// The bot token is the ONLY way the handler learns the workspace id —
	// the admin never types it. Always re-validated: there's no "keep the
	// current bot token" path.
	result, err := slackAuthTest(r.Context(), h.client, botToken)
	if err != nil {
		httpx.BadRequest(w, "could not validate the bot token with Slack: "+err.Error())
		return
	}

	// The app id is likewise never typed by the admin — derived from the
	// same bot token via auth.test's bot_id -> bots.info's app_id. It's key
	// material (the app-single-org invariant), so a failure here is fatal:
	// no fallback, connect refused.
	botsInfo, err := slackBotsInfo(r.Context(), h.client, botToken, result.BotID)
	if err != nil {
		httpx.BadRequest(w, "could not resolve the app id for this bot token: "+err.Error())
		return
	}
	apiAppID := botsInfo.AppID

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
		if err := slackOpenConnection(r.Context(), h.client, appToken); err != nil {
			httpx.BadRequest(w, "could not validate the app-level token with Slack: "+err.Error())
			return
		}
	}

	keys := integrations.SlackWorkspaceKeysFor(result.TeamID, apiAppID)
	ws := slackstore.Workspace{
		WorkspaceID:        result.TeamID,
		APIAppID:           apiAppID,
		OrgID:              orgID,
		WorkspaceName:      result.Team,
		EnterpriseID:       result.EnterpriseID,
		Transport:          transport,
		BotUserID:          result.UserID,
		BotTokenRef:        keys.BotToken,
		SigningSecretRef:   refFor(hasSigningSecret, keys.SigningSecret),
		AppTokenRef:        refFor(hasAppToken, keys.AppToken),
		RegisteredByUserID: userID,
	}

	var persisted *slackstore.Workspace
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		// LockApp MUST run before the AppBoundToOtherOrgSystem check below
		// — on its own, that check-then-Upsert sequence is a race: two
		// concurrent connects for the same api_app_id under different orgs
		// write different workspace_id rows, so they never collide on a
		// unique index, and each could observe "not bound" before either
		// commits. The advisory lock serializes every connect attempt for
		// this api_app_id so that can't happen — see LockApp's doc.
		if e := bundle.Workspaces.LockApp(r.Context(), apiAppID); e != nil {
			return e
		}
		// The app-single-org invariant has no SQL constraint behind it
		// (api_app_id is only unique in combination with workspace_id), so
		// it's enforced here, before any write — see
		// AppBoundToOtherOrgSystem's doc.
		boundElsewhere, e := bundle.Workspaces.AppBoundToOtherOrgSystem(r.Context(), apiAppID, orgID)
		if e != nil {
			return e
		}
		if boundElsewhere {
			return errAppBoundToOtherOrg
		}

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
		if err := bundle.Workspaces.Upsert(r.Context(), ws); err != nil {
			return err
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
		persisted, e = bundle.Workspaces.Get(r.Context(), orgID, result.TeamID, apiAppID)
		return e
	}); err != nil {
		if errors.Is(err, errAppBoundToOtherOrg) {
			// Deliberately generic: a workspace admin may learn "already
			// connected", never which org holds it (the app-single-org
			// invariant).
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "this app is already connected",
			})
			return
		}
		if httpx.IsUniqueViolation(err) {
			// The (workspace, app) composite PK's backstop — see Upsert's
			// doc. Same non-disclosure posture as the app-single-org 409
			// above, distinct wording since it's the narrower conflict.
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "this workspace+app is already connected",
			})
			return
		}
		httpx.InternalError(w, "slack", err)
		return
	}
	if persisted == nil {
		httpx.InternalError(w, "slack", fmt.Errorf("slack workspace upsert reported success but the row is missing"))
		return
	}

	notifyConfigChanged(orgID)
	slackLog.Info("slack workspace connected", "org", orgID, "workspace", result.TeamID, "app", apiAppID, "transport", transport)

	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, toWorkspaceView(persisted, h.sockets))
}

// DELETE /api/slack/workspaces/{workspace_id}/{api_app_id}
func (h *workspacesHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	workspaceID := r.PathValue("workspace_id")
	apiAppID := r.PathValue("api_app_id")
	if workspaceID == "" || apiAppID == "" {
		http.NotFound(w, r)
		return
	}

	var existed bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		ws, e := bundle.Workspaces.Get(r.Context(), orgID, workspaceID, apiAppID)
		if e != nil {
			return e
		}
		if ws == nil {
			return nil
		}
		existed = true
		if e := bundle.Workspaces.Delete(r.Context(), orgID, workspaceID, apiAppID); e != nil {
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
		// app) pair actually populated — deleting an absent key is a
		// no-op, mirrors teardownAppSecrets for GitHub Apps.
		for _, k := range integrations.SlackWorkspaceKeysFor(workspaceID, apiAppID).All() {
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
		http.NotFound(w, r)
		return
	}

	notifyConfigChanged(orgID)
	slackLog.Info("slack workspace disconnected", "org", orgID, "workspace", workspaceID, "app", apiAppID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GET /api/slack/manifest?transport=socket|events_api
func (h *workspacesHandler) handleManifest(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	transport := r.URL.Query().Get("transport")
	if transport != transportSocket && transport != transportEventsAPI {
		httpx.BadRequest(w, `transport must be "socket" or "events_api"`)
		return
	}
	publicURL := h.publicURL()
	if transport == transportEventsAPI && publicURL == "" {
		httpx.WriteJSON(w, http.StatusConflict, map[string]string{
			"error": "set TF_PUBLIC_URL / deploy config first",
		})
		return
	}

	var orgName string
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
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

	httpx.WriteJSON(w, http.StatusOK, buildSlackManifest(orgName, transport, publicURL, orgID))
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
