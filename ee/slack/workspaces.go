package slack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// workspacesHandler serves /api/slack/workspaces* and /api/slack/manifest —
// the org-admin Slack workspace connect/disconnect lifecycle plus the
// member-visible list. No runmode branching anywhere here (unlike SSO):
// every route gates purely on the `slack` entitlement, since a licensed
// local install is not structurally excluded the way SSO's GoTrue
// dependency excludes it.
type workspacesHandler struct {
	tx     db.TxRunner
	az     *authz.Checker
	client *http.Client
	// publicURL returns the deployment's externally-visible base, used to
	// build the events_api manifest's request_url.
	publicURL func() string
}

type workspaceView struct {
	WorkspaceID        string    `json:"workspace_id"`
	WorkspaceName      string    `json:"workspace_name"`
	EnterpriseID       string    `json:"enterprise_id,omitempty"`
	Transport          string    `json:"transport"`
	BotUserID          string    `json:"bot_user_id"`
	RegisteredByUserID string    `json:"registered_by_user_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func toWorkspaceView(w *slackstore.Workspace) workspaceView {
	return workspaceView{
		WorkspaceID:        w.WorkspaceID,
		WorkspaceName:      w.WorkspaceName,
		EnterpriseID:       w.EnterpriseID,
		Transport:          w.Transport,
		BotUserID:          w.BotUserID,
		RegisteredByUserID: w.RegisteredByUserID,
		CreatedAt:          w.CreatedAt,
		UpdatedAt:          w.UpdatedAt,
	}
}

// memberGate resolves the active org and confirms the `slack` entitlement.
// Any org member may reach a member-gated route once past this — used by
// the read-only list.
func (h *workspacesHandler) memberGate(w http.ResponseWriter, r *http.Request) (orgID string, ok bool) {
	orgID, ok = httpx.RequireOrg(w, r)
	if !ok {
		return "", false
	}
	if !entitlements.For(orgID).Has(entitlements.FeatureSlack) {
		http.NotFound(w, r)
		return "", false
	}
	return orgID, true
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
		out[i] = toWorkspaceView(&list[i])
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getExisting reads the org's current row for workspaceID, or (nil, nil)
// when none exists in THIS org (a workspace_id owned by a different org is
// invisible here — RLS plus the explicit org_id filter — which is exactly
// right: the merge logic below must never see another org's stored refs).
func (h *workspacesHandler) getExisting(ctx context.Context, orgID, userID, workspaceID string) (*slackstore.Workspace, error) {
	var ws *slackstore.Workspace
	if err := h.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		ws, e = slackstore.FromTx(tx).Workspaces.Get(ctx, orgID, workspaceID)
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
	// the admin never types it (TFAC-529 decision). Always re-validated:
	// there's no "keep the current bot token" path.
	result, err := slackAuthTest(r.Context(), h.client, botToken)
	if err != nil {
		httpx.BadRequest(w, "could not validate the bot token with Slack: "+err.Error())
		return
	}

	existing, err := h.getExisting(r.Context(), orgID, userID, result.TeamID)
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

	keys := integrations.SlackWorkspaceKeysFor(result.TeamID)
	ws := slackstore.Workspace{
		WorkspaceID:        result.TeamID,
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
		bundle := slackstore.FromTx(tx)
		if err := bundle.Workspaces.Upsert(r.Context(), ws); err != nil {
			return err
		}
		var e error
		persisted, e = bundle.Workspaces.Get(r.Context(), orgID, result.TeamID)
		return e
	}); err != nil {
		if httpx.IsUniqueViolation(err) {
			// Deliberately generic: a workspace admin may learn "already
			// connected", never which org holds it (TFAC-529 Part 2).
			httpx.WriteJSON(w, http.StatusConflict, map[string]string{
				"error": "this workspace is already connected",
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
	slackLog.Info("slack workspace connected", "org", orgID, "workspace", result.TeamID, "transport", transport)

	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, toWorkspaceView(persisted))
}

// DELETE /api/slack/workspaces/{workspace_id}
func (h *workspacesHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.adminGate(w, r)
	if !ok {
		return
	}
	workspaceID := r.PathValue("workspace_id")
	if workspaceID == "" {
		http.NotFound(w, r)
		return
	}

	var existed bool
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		bundle := slackstore.FromTx(tx)
		ws, e := bundle.Workspaces.Get(r.Context(), orgID, workspaceID)
		if e != nil {
			return e
		}
		if ws == nil {
			return nil
		}
		existed = true
		if e := bundle.Workspaces.Delete(r.Context(), orgID, workspaceID); e != nil {
			return fmt.Errorf("delete workspace row: %w", e)
		}
		// Sweep the full keyset regardless of which refs this workspace
		// actually populated — deleting an absent key is a no-op, mirrors
		// teardownAppSecrets for GitHub Apps.
		for _, k := range integrations.SlackWorkspaceKeysFor(workspaceID).All() {
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
	slackLog.Info("slack workspace disconnected", "org", orgID, "workspace", workspaceID)
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

// notifyConfigChanged is the in-package seam the (future) socket connection
// manager leaf replaces with a real signal — a no-op today. Deliberately NOT
// core reloader plumbing (SetOnGitHubChanged-style hooks are for core
// subsystems): both the producer (here) and the eventual consumer live
// entirely inside ee/slack.
func notifyConfigChanged(orgID string) {
	_ = orgID
}
