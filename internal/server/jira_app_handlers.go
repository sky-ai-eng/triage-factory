package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// first-party app (hosted) or has nothing (local, where the BYO row IS the
// app). The client_secret lands in the secret store (Vault hosted / keychain
// local) via SecretStore — never plaintext in org_jira_apps.

// jiraOAuthClientSecretKey is the org-scoped secret key the Atlassian OAuth
// app's client_secret is custodied under. One app per org, so a fixed key
// (unlike the GitHub App's per-app-id keys) is sufficient. Sits alongside the
// other org-level jira_* secret keys (jira_url, jira_pat, …).
const jiraOAuthClientSecretKey = "jira_oauth_client_secret"

// jiraConnectCallbackURL is the absolute redirect_uri the Atlassian OAuth app
// must register for the per-user "Connect Jira" flow — hosted
// "{deployment}/api/orgs/{org}/jira/connect/callback", local
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
//     hosted, the deployment first-party) — the same bit the per-user Jira
//     status endpoint surfaces so the frontend shows the "Connect" button.
//   - UsingHostedDefault is true when an app resolves but via the first-party
//     default (no per-org row) — so the card can say "using the hosted app"
//     rather than offering nothing.
//   - ConnectCallbackURL is the redirect_uri the app owner must register.
type jiraAppStatusResponse struct {
	App                *jiraAppInfo `json:"app"`
	ConnectAvailable   bool         `json:"connect_available"`
	UsingHostedDefault bool         `json:"using_hosted_default"`
	ConnectCallbackURL string       `json:"connect_callback_url"`
}

// newJiraAppStatusResponse assembles the status payload from a loaded per-org
// override, the resolver's connect-available verdict, and the registrant's
// display name. A nil app yields app:null (no per-org override). When no
// per-org app is set but connect is still available, that's the hosted
// first-party app covering the org.
func newJiraAppStatusResponse(app *domain.OrgJiraApp, connectAvailable bool, registeredByName, connectCallbackURL string) jiraAppStatusResponse {
	resp := jiraAppStatusResponse{
		ConnectAvailable:   connectAvailable,
		UsingHostedDefault: app == nil && connectAvailable,
		ConnectCallbackURL: connectCallbackURL,
	}
	if app != nil {
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
		log.Printf("[jira-app] display name for %s: %v", app.RegisteredByUserID, err)
		return ""
	}
	return name
}

// connectAvailableForOrg reports whether an Atlassian OAuth app resolves for
// the org (per-org override or, in hosted, the deployment first-party). A
// not-configured outcome is the expected false; a backend read error is logged
// and treated as false (the signal degrades closed rather than failing the
// caller — both the card status and the per-user Jira status read it). System
// read: the caller has already authorized orgID.
func (s *Server) connectAvailableForOrg(r *http.Request, orgID string) bool {
	if _, err := s.jiraOAuthApps.Resolve(r.Context(), orgID); err != nil {
		if !errors.Is(err, jira.ErrNoAtlassianOAuthApp) {
			log.Printf("[jira-app] resolve oauth app for org %s: %v", orgID, err)
		}
		return false
	}
	return true
}

// handleJiraAppStatus returns the org's Atlassian OAuth app config state. Read-
// only; any org member (the card renders for everyone, the import/delete are
// admin-gated).
//
// GET /api/orgs/{org_id}/jira-app
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

	writeJSON(w, http.StatusOK, newJiraAppStatusResponse(
		app,
		s.connectAvailableForOrg(r, orgID),
		s.jiraRegistrantDisplayName(r.Context(), orgID, userID, app),
		s.jiraConnectCallbackURLSafe(orgID),
	))
}

// jiraAppImportRequest is the import endpoint body — a bring-your-own
// Atlassian OAuth app's client credentials.
type jiraAppImportRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// handleJiraAppImport stores (or replaces) the org's bring-your-own Atlassian
// OAuth app. Org-admin only; works in both modes — hosted writes a per-org
// override (the secret lands in Vault), local stores the user-supplied app (the
// secret lands in the keychain). There is no validation round trip: Atlassian
// exposes no way to verify an OAuth app's client credentials without running the
// authorize/token flow itself (a separate ticket), so this only stores them.
//
// POST /api/orgs/{org_id}/jira-app
func (s *Server) handleJiraAppImport(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	var req jiraAppImportRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	clientID := strings.TrimSpace(req.ClientID)
	clientSecret := strings.TrimSpace(req.ClientSecret)
	if clientID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "An Atlassian OAuth app client ID is required.", "field": "client_id",
		})
		return
	}
	if clientSecret == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "An Atlassian OAuth app client secret is required.", "field": "client_secret",
		})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.JiraApps.UpsertForOrg(r.Context(), domain.OrgJiraApp{
			OrgID:              orgID,
			ClientID:           clientID,
			ClientSecretRef:    jiraOAuthClientSecretKey,
			RegisteredByUserID: userID,
		}); err != nil {
			return err
		}
		return tx.Secrets.Put(r.Context(), orgID, jiraOAuthClientSecretKey, clientSecret, "Atlassian OAuth app client secret")
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

	log.Printf("[jira-app] stored atlassian oauth app client_id=%s for org=%s (by user=%s)", clientID, orgID, userID)

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
	writeJSON(w, http.StatusOK, newJiraAppStatusResponse(
		app,
		s.connectAvailableForOrg(r, orgID),
		s.jiraRegistrantDisplayName(r.Context(), orgID, userID, app),
		s.jiraConnectCallbackURLSafe(orgID),
	))
}

// handleJiraAppDelete removes the org's bring-your-own Atlassian OAuth app —
// the per-org override row AND its stored client_secret. Org-admin only.
// Idempotent: a no-op (200) when the org has no override. After this, the org
// falls back to the deployment first-party app (hosted) or has no app (local).
//
// DELETE /api/orgs/{org_id}/jira-app
func (s *Server) handleJiraAppDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.JiraApps.DeleteForOrg(r.Context(), orgID)
	}); err != nil {
		internalError(w, "jira-app", err)
		return
	}
	// Delete the secret outside the tx — in local mode it lives in the
	// keychain (not the SQLite tx); in multi mode the row delete already
	// committed, and a lingering secret with no row pointing at it is a
	// harmless orphan, so a best-effort delete here is the right shape.
	if _, err := s.secrets.Delete(r.Context(), orgID, jiraOAuthClientSecretKey); err != nil {
		log.Printf("[jira-app] delete client secret for org %s: %v", orgID, err)
	}

	log.Printf("[jira-app] removed atlassian oauth app for org=%s (by user=%s)", orgID, userID)

	writeJSON(w, http.StatusOK, newJiraAppStatusResponse(
		nil,
		s.connectAvailableForOrg(r, orgID),
		"",
		s.jiraConnectCallbackURLSafe(orgID),
	))
}
