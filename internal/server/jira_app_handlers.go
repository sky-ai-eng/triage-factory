package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Per-org Atlassian OAuth (3LO) app config — the credential layer the per-user
// "Connect Jira" flow runs against. This is the Jira sibling of the GitHub App
// import card (github_app_import.go): an admin enters a bring-your-own
// Atlassian app (client_id + client_secret) and registers the callback URL it
// shows on their Atlassian app. It mirrors the GitHub card but is far simpler —
// an Atlassian OAuth app has no installations, no PEM, and no permission
// preflight, only the OAuth client credentials.
//
// Storage precedence (resolved by jira.OAuthAppResolver): the per-org row this
// card writes is the OVERRIDE; an org with no row falls back to the deployment
// app (multi) or has nothing (local, where the BYO row IS the app). The
// client_secret lands in the secret store (Vault in multi / keychain in local)
// via SecretStore — never plaintext in org_jira_apps.

// jiraOAuthClientSecretKey is the org-scoped secret key the Atlassian OAuth
// app's client_secret is custodied under. One app per org, so a fixed key
// (unlike the GitHub App's per-app-id keys) is sufficient. Sits alongside the
// other org-level jira_* secret keys (jira_url, jira_pat, …).
const jiraOAuthClientSecretKey = "jira_oauth_client_secret"

// jiraConnectCallbackURL is the absolute redirect_uri the Atlassian OAuth app
// must register for the per-user "Connect Jira" flow — in multi
// "{deployment}/api/orgs/{org}/jira/connect/callback", in local
// "http://localhost:{port}/...". deployCfg.publicURL already encodes both (the
// local public URL is the localhost origin), so one expression serves both
// modes — the same source-of-truth shape connectCallbackURL uses for GitHub.
func (s *Server) jiraConnectCallbackURL(orgID string) string {
	return s.deployCfg.publicURL + "/api/orgs/" + orgID + "/jira/connect/callback"
}

// jiraConnectCallbackURLSafe degrades to "" when no deployment identity is
// configured (deployCfg nil — a unit-test server), so the status payload can
// carry the field unconditionally instead of panicking. Mirrors
// connectCallbackURLSafe.
func (s *Server) jiraConnectCallbackURLSafe(orgID string) string {
	if s.deployCfg == nil {
		return ""
	}
	return s.jiraConnectCallbackURL(orgID)
}

// jiraAppInfo is the per-org Atlassian OAuth app override summary. Only the
// public half (client_id) is surfaced; the client_secret never leaves the
// secret store.
type jiraAppInfo struct {
	ClientID                string `json:"client_id"`
	RegisteredAt            string `json:"registered_at"`
	RegisteredByDisplayName string `json:"registered_by_display_name"`
}

// jiraAppStatusResponse is the read-only shape the settings card polls.
//   - App is the per-org override, null when the org has none.
//   - ConnectAvailable is true exactly when an app resolves (override OR, in
//     multi, the deployment app) — the same bit the per-user Jira status
//     endpoint surfaces so the frontend shows the "Connect" button.
//   - UsingDeploymentDefault is true when an app resolves but via the
//     deployment default (no per-org row) — so the card can say "using the
//     deployment app" rather than offering nothing.
//   - ConnectCallbackURL is the redirect_uri the app owner must register.
type jiraAppStatusResponse struct {
	App                    *jiraAppInfo `json:"app"`
	ConnectAvailable       bool         `json:"connect_available"`
	UsingDeploymentDefault bool         `json:"using_deployment_default"`
	ConnectCallbackURL     string       `json:"connect_callback_url"`
}

// newJiraAppStatusResponse assembles the status payload from the loaded per-org
// row and the source the resolver actually resolved from. The summary is driven
// by the resolved source, NOT the raw row, so the three states never diverge:
//   - SourceOrgOverride → the row is the live app; show it, using_deployment_default=false.
//   - SourceDeployment  → app:null + using_deployment_default=true (the
//     deployment app covers the org — including the case where a per-org row
//     exists but its secret has gone missing, so the resolver fell through to
//     the deployment app).
//   - SourceNone        → app:null, connect_available=false.
//
// registeredByName is only meaningful for an override (the row's registrant) and
// is ignored otherwise.
func newJiraAppStatusResponse(app *domain.OrgJiraApp, source jira.OAuthAppSource, registeredByName, connectCallbackURL string) jiraAppStatusResponse {
	resp := jiraAppStatusResponse{
		ConnectAvailable:       source != jira.SourceNone,
		UsingDeploymentDefault: source == jira.SourceDeployment,
		ConnectCallbackURL:     connectCallbackURL,
	}
	if source == jira.SourceOrgOverride && app != nil {
		resp.App = &jiraAppInfo{
			ClientID:                app.ClientID,
			RegisteredAt:            app.RegisteredAt.UTC().Format(time.RFC3339),
			RegisteredByDisplayName: registeredByName,
		}
	}
	return resp
}

// jiraRegistrantDisplayName best-effort resolves the display name of the user
// who registered the org's Atlassian OAuth app. Cosmetic — a missing user or a
// read error degrades to "" (logged), like registrantDisplayName.
func (s *Server) jiraRegistrantDisplayName(ctx context.Context, orgID, userID string, app *domain.OrgJiraApp) string {
	if app == nil || app.RegisteredByUserID == "" {
		return ""
	}
	var name string
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var derr error
		name, derr = tx.Users.GetDisplayName(ctx, app.RegisteredByUserID)
		return derr
	}); err != nil {
		jiraAppLog.Warn("display name lookup failed", "user", app.RegisteredByUserID, "error", err)
		return ""
	}
	return name
}

// resolveOAuthAppSource reports which tier resolves the org's Atlassian OAuth
// app (override / deployment / none). A not-configured outcome is the expected
// SourceNone; a backend read error is logged and degraded to SourceNone (the
// signal fails closed rather than failing the caller — both the card status and
// the per-user Jira status read it). System read: the caller has already
// authorized orgID.
func (s *Server) resolveOAuthAppSource(r *http.Request, orgID string) jira.OAuthAppSource {
	_, source, err := s.jiraOAuthApps.Resolve(r.Context(), orgID)
	if err != nil {
		if !errors.Is(err, jira.ErrNoAtlassianOAuthApp) {
			jiraAppLog.Warn("resolve oauth app failed", "org", orgID, "error", err)
		}
		return jira.SourceNone
	}
	return source
}

// connectAvailableForOrg reports whether an Atlassian OAuth app resolves for the
// org (per-org override or, in multi, the deployment app) — the signal the
// per-user Jira status endpoint surfaces to gate the one-click Connect button.
func (s *Server) connectAvailableForOrg(r *http.Request, orgID string) bool {
	return s.resolveOAuthAppSource(r, orgID) != jira.SourceNone
}

// handleJiraAppStatus returns the org's Atlassian OAuth app config state. Read-
// only; any org member (the card renders for everyone, the import/delete are
// admin-gated).
//
// GET /api/orgs/{org_id}/jira/app
func (s *Server) handleJiraAppStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var app *domain.OrgJiraApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.JiraApps.GetForOrg(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "jira-app", err)
		return
	}

	writeJSON(w, http.StatusOK, s.jiraAppStatusPayload(r, orgID, userID, app))
}

// jiraAppStatusPayload builds the status response for a loaded per-org row,
// resolving the effective source so the summary reflects what actually resolves
// (a row with a missing secret reads as the deployment default, not a live
// override). The registrant lookup runs only when the override is the live app.
func (s *Server) jiraAppStatusPayload(r *http.Request, orgID, userID string, app *domain.OrgJiraApp) jiraAppStatusResponse {
	source := s.resolveOAuthAppSource(r, orgID)
	registeredBy := ""
	if source == jira.SourceOrgOverride {
		registeredBy = s.jiraRegistrantDisplayName(r.Context(), orgID, userID, app)
	}
	return newJiraAppStatusResponse(app, source, registeredBy, s.jiraConnectCallbackURLSafe(orgID))
}

// jiraAppImportRequest is the import endpoint body — a bring-your-own
// Atlassian OAuth app's client credentials.
type jiraAppImportRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// handleJiraAppImport stores (or replaces) the org's bring-your-own Atlassian
// OAuth app. Org-admin only; works in both modes — multi writes a per-org
// override (the secret lands in Vault), local stores the user-supplied app (the
// secret lands in the keychain). There is no validation round trip: Atlassian
// exposes no way to verify an OAuth app's client credentials without running the
// authorize/token flow itself (a separate ticket), so this only stores them.
//
// POST /api/orgs/{org_id}/jira/app
func (s *Server) handleJiraAppImport(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req jiraAppImportRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	clientID := strings.TrimSpace(req.ClientID)
	clientSecret := strings.TrimSpace(req.ClientSecret)
	if clientID == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "An Atlassian OAuth app client ID is required.", Field: "client_id"})
		return
	}
	if clientSecret == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "An Atlassian OAuth app client secret is required.", Field: "client_secret"})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, err := tx.JiraApps.UpsertForOrg(r.Context(), domain.OrgJiraApp{
			OrgID:              orgID,
			ClientID:           clientID,
			ClientSecretRef:    jiraOAuthClientSecretKey,
			RegisteredByUserID: userID,
		}); err != nil {
			return err
		}
		if err := tx.Secrets.Put(r.Context(), orgID, jiraOAuthClientSecretKey, clientSecret, "Atlassian OAuth app client secret"); err != nil {
			return err
		}
		// A bring-your-own OAuth app is an org credential (its client
		// secret is what every user's Jira consent is minted against), so its
		// bind/rotate belongs in the change-log next to the GitHub App's. The
		// client ID is public, so it rides as the row's name.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindJiraOAuthApp, "", clientID),
		})
	}); err != nil {
		// Local-mode SecretStore writes hit the OS keychain outside the SQLite
		// tx; clean up a secret that landed before a later failure so a failed
		// import leaves no orphan credential. Mirrors the GitHub App import.
		if runmode.Current() == runmode.ModeLocal {
			_, _ = s.secrets.Delete(r.Context(), orgID, jiraOAuthClientSecretKey)
		}
		internalError(w, "jira-app", err)
		return
	}

	jiraAppLog.Info("stored atlassian oauth app", "client_id", clientID, "org", orgID, "user", userID)

	// Re-read so the response reflects the freshly-persisted row, in the same
	// shape the status GET serves. connect_available is now necessarily true.
	var app *domain.OrgJiraApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.JiraApps.GetForOrg(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "jira-app", err)
		return
	}
	writeJSON(w, http.StatusOK, s.jiraAppStatusPayload(r, orgID, userID, app))
}

// handleJiraAppDelete removes the org's bring-your-own Atlassian OAuth app —
// the per-org override row AND its stored client_secret. Org-admin only.
// Idempotent: a no-op (200) when the org has no override. After this, the org
// falls back to the deployment app (multi) or has no app (local).
//
// DELETE /api/orgs/{org_id}/jira/app
func (s *Server) handleJiraAppDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Read first: the delete is idempotent, so without knowing whether an
		// override existed we'd log a removal every time an admin hits the
		// button on an org that never had one.
		app, err := tx.JiraApps.GetForOrg(r.Context(), orgID)
		if err != nil {
			return err
		}
		if err := tx.JiraApps.DeleteForOrg(r.Context(), orgID); err != nil {
			return err
		}
		if app == nil {
			return nil
		}
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindJiraOAuthApp, "", app.ClientID),
		})
	}); err != nil {
		internalError(w, "jira-app", err)
		return
	}
	// Delete the secret outside the tx — in local mode it lives in the
	// keychain (not the SQLite tx); in multi mode the row delete already
	// committed, and a lingering secret with no row pointing at it is a
	// harmless orphan, so a best-effort delete here is the right shape.
	if _, err := s.secrets.Delete(r.Context(), orgID, jiraOAuthClientSecretKey); err != nil {
		jiraAppLog.Warn("delete client secret failed", "org", orgID, "error", err)
	}

	jiraAppLog.Info("removed atlassian oauth app", "org", orgID, "user", userID)

	// The row is gone; the payload now reflects whatever still resolves (the
	// deployment app, or nothing).
	writeJSON(w, http.StatusOK, s.jiraAppStatusPayload(r, orgID, userID, nil))
}
