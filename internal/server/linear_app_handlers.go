package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The org's own Linear OAuth app — the Linear sibling of the Atlassian app
// card (jira_app_handlers.go). An admin creates an app in Linear from a
// pre-filled link, pastes its client id and secret here, and the install
// ceremony (linear_install.go) then runs against it.
//
//   GET    /api/orgs/{org_id}/linear/app
//   POST   /api/orgs/{org_id}/linear/app
//   DELETE /api/orgs/{org_id}/linear/app
//
// The row is the per-org override in linear.OAuthAppResolver's precedence; the
// client secret lives in the secret store under linearOAuthClientSecretKey.

// linearOAuthClientSecretKey is the org secret the app's client secret is
// stored under. One app per org, so a fixed key.
const linearOAuthClientSecretKey = "linear_oauth_client_secret"

// The two redirect URIs a Linear OAuth app registers. Linear matches redirect
// URIs exactly, and one app may serve many orgs, so both are static: the org
// and user a ceremony belongs to travel in its signed state cookie.
const (
	linearInstallCallbackPath = "/api/linear/install/callback"
	linearConnectCallbackPath = "/api/linear/connect/callback"
)

// linearAppCreateURL is Linear's app-creation page, which pre-fills its form
// from these query parameters. It does not hand the credentials back, so the
// admin copies them from the created app's page.
const linearAppCreateURL = "https://linear.app/settings/api/applications/new"

// linearDeveloperNameFallback names the app's developer when the org's own
// name is outside Linear's 2–80 character bounds.
const linearDeveloperNameFallback = "Triage Factory workspace"

// linearAppInfo is the org's own app, public half only.
type linearAppInfo struct {
	ClientID                string `json:"client_id"`
	RegisteredAt            string `json:"registered_at"`
	RegisteredByDisplayName string `json:"registered_by_display_name"`
}

// linearAppStatusResponse is the GET body, and what a successful POST or
// DELETE answers with. Like jiraAppStatusResponse it is driven by the
// resolved source rather than the raw row: a row whose secret is gone is not
// reported as the org's app.
type linearAppStatusResponse struct {
	App                    *linearAppInfo `json:"app"`
	InstallAvailable       bool           `json:"install_available"`
	UsingDeploymentDefault bool           `json:"using_deployment_default"`
	CreateURL              string         `json:"create_url"`
	RedirectURIs           []string       `json:"redirect_uris"`
}

// linearAppImportRequest is the POST body.
type linearAppImportRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// errLinearInstallStranded is a change to the org's app that would leave a
// live install unable to refresh: its refresh token is only accepted with the
// secret of the app that minted it.
var errLinearInstallStranded = errors.New("linear: the change would strand the live install")

// linearCallbackURL is the absolute redirect URI for path, "" when the server
// has no deployment identity (a unit-test server).
func (s *Server) linearCallbackURL(path string) string {
	if s.deployCfg == nil {
		return ""
	}
	return s.deployCfg.publicURL + path
}

// linearRedirectURIs are the two URIs an app owner registers, in the order
// the card lists them.
func (s *Server) linearRedirectURIs() []string {
	if s.deployCfg == nil {
		return []string{}
	}
	return []string{s.linearCallbackURL(linearInstallCallbackPath), s.linearCallbackURL(linearConnectCallbackPath)}
}

// linearDeveloperName is the org's name clamped to Linear's 2–80 characters,
// or the fallback when what is left is too short.
func linearDeveloperName(orgName string) string {
	name := strings.TrimSpace(orgName)
	if utf8.RuneCountInString(name) > 80 {
		name = strings.TrimSpace(string([]rune(name)[:80]))
	}
	if utf8.RuneCountInString(name) < 2 {
		return linearDeveloperNameFallback
	}
	return name
}

// linearAppCreateLink is the pre-filled app-creation URL for the org. The
// client name may contain neither "Linear" nor a URL, so it is fixed.
func (s *Server) linearAppCreateLink(orgName string) string {
	if s.deployCfg == nil {
		return ""
	}
	q := url.Values{}
	q.Set("distribution", "private")
	q.Set("developer.name", linearDeveloperName(orgName))
	q.Set("oauth.client_name", "Triage Factory")
	q.Set("oauth.client_uri", s.deployCfg.publicURL)
	for _, uri := range s.linearRedirectURIs() {
		q.Add("oauth.redirect_uris", uri)
	}
	q.Set("oauth.grant_types", "authorization_code")
	return linearAppCreateURL + "?" + q.Encode()
}

// resolveLinearAppSource reports which tier resolves the org's Linear OAuth
// app. A read failure is logged and reads as SourceNone: the status surfaces
// fail closed rather than failing the read.
func (s *Server) resolveLinearAppSource(ctx context.Context, orgID string) linear.OAuthAppSource {
	_, source, err := s.linearOAuthApps.Resolve(ctx, orgID)
	if err != nil {
		if !errors.Is(err, linear.ErrNoLinearOAuthApp) {
			linearAppLog.Warn("resolve oauth app failed", "org", orgID, "error", err)
		}
		return linear.SourceNone
	}
	return source
}

// linearAppStatus reads the org's app row and name and assembles the status
// body.
func (s *Server) linearAppStatus(ctx context.Context, orgID, userID string) (linearAppStatusResponse, error) {
	var (
		app          *domain.OrgLinearApp
		org          *domain.Org
		registeredBy string
	)
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if app, err = tx.LinearApps.GetForOrg(ctx, orgID); err != nil {
			return err
		}
		if org, err = tx.Orgs.GetOrg(ctx, orgID); err != nil {
			return err
		}
		if app != nil && app.RegisteredByUserID != "" {
			// Cosmetic: a registrant whose name cannot be read costs the
			// card a name, not the read.
			name, nerr := tx.Users.GetDisplayName(ctx, app.RegisteredByUserID)
			if nerr != nil {
				linearAppLog.Warn("registrant display name lookup failed", "user", app.RegisteredByUserID, "error", nerr)
			}
			registeredBy = name
		}
		return nil
	}); err != nil {
		return linearAppStatusResponse{}, err
	}

	source := s.resolveLinearAppSource(ctx, orgID)
	orgName := ""
	if org != nil {
		orgName = org.Name
	}
	resp := linearAppStatusResponse{
		InstallAvailable:       source != linear.SourceNone,
		UsingDeploymentDefault: source == linear.SourceDeployment,
		CreateURL:              s.linearAppCreateLink(orgName),
		RedirectURIs:           s.linearRedirectURIs(),
	}
	if source == linear.SourceOrgOverride && app != nil {
		resp.App = &linearAppInfo{
			ClientID:                app.ClientID,
			RegisteredAt:            app.RegisteredAt.UTC().Format(time.RFC3339),
			RegisteredByDisplayName: registeredBy,
		}
	}
	return resp, nil
}

// handleLinearAppStatus reports the org's Linear OAuth app. Any org member:
// the card renders for everyone, and nothing in it is secret.
//
// GET /api/orgs/{org_id}/linear/app
func (s *Server) handleLinearAppStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	resp, err := s.linearAppStatus(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// liveLinearInstall is the org's install row when it is live, nil otherwise.
func (s *Server) liveLinearInstall(ctx context.Context, orgID string) (*domain.OrgLinearInstall, error) {
	inst, err := s.linearInstalls.GetForOrgSystem(ctx, orgID)
	if err != nil || inst == nil || !inst.Live() {
		return nil, err
	}
	return inst, nil
}

// handleLinearAppImport stores (or replaces) the org's Linear OAuth app.
// Org admin. Nothing is validated against Linear: a client id and secret can
// only be checked by running the ceremony itself.
//
// Replacing the app with one of a different client id while an install
// minted by another app is live is refused 409, for the reason the delete
// gives. Rotating the secret of the same app is a replace like any other.
//
// POST /api/orgs/{org_id}/linear/app
func (s *Server) handleLinearAppImport(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var req linearAppImportRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	clientID := strings.TrimSpace(req.ClientID)
	clientSecret := strings.TrimSpace(req.ClientSecret)
	var faults []httpx.ErrorItem
	if clientID == "" {
		faults = append(faults, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "A Linear OAuth app client ID is required.", Field: "client_id"})
	}
	if clientSecret == "" {
		faults = append(faults, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "A Linear OAuth app client secret is required.", Field: "client_secret"})
	}
	if len(faults) > 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, faults...)
		return
	}

	// Held across the install check and the write: an install completing on
	// another pod lands either before the check, which then sees it, or after
	// the write, and the ceremony refuses an app that changed under it.
	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	defer release()

	inst, err := s.liveLinearInstall(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	if inst != nil && inst.AppClientID != clientID {
		writeLinearInstallStranded(w, "client_id")
		return
	}

	// In local mode the secret lands in the keychain outside the transaction,
	// so a failed import puts back the secret that was there before rather
	// than leaving the new one beside the old row.
	restore, unlock, err := s.guardLocalLinearAppWrite(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	defer unlock()

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		if _, err := tx.LinearApps.UpsertForOrg(ctx, domain.OrgLinearApp{
			OrgID:              orgID,
			ClientID:           clientID,
			ClientSecretRef:    linearOAuthClientSecretKey,
			RegisteredByUserID: userID,
		}); err != nil {
			return err
		}
		if err := tx.Secrets.Put(ctx, orgID, linearOAuthClientSecretKey, clientSecret, "Linear OAuth app client secret"); err != nil {
			return err
		}
		// Every install and refresh authenticates with this secret, so its
		// bind and rotation belong in the change log beside the org's other
		// credentials. The client id is public and rides as the row's name.
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearOAuthApp, "", clientID),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		internalError(w, "linear-app", err)
		return
	}
	linearAppLog.Info("stored linear oauth app", "client_id", clientID, "org", orgID, "user", userID)

	resp, err := s.linearAppStatus(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLinearAppDelete removes the org's Linear OAuth app — the row and its
// client secret. Org admin; idempotent, with no audit row when there was no
// app.
//
// A live install minted by this app is refused 409: refreshing its token
// needs the client secret, so deleting the secret would strand the install.
// The admin disconnects the install first.
//
// DELETE /api/orgs/{org_id}/linear/app
func (s *Server) handleLinearAppDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Held across the install check and the delete, for the reason the import
	// gives.
	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	defer release()

	restore, unlock, err := s.guardLocalLinearAppWrite(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	defer unlock()

	inst, err := s.liveLinearInstall(ctx, orgID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		app, err := tx.LinearApps.GetForOrg(ctx, orgID)
		if err != nil {
			return err
		}
		if app == nil {
			return nil
		}
		if inst != nil && inst.AppClientID == app.ClientID {
			return errLinearInstallStranded
		}
		if err := tx.LinearApps.DeleteForOrg(ctx, orgID); err != nil {
			return err
		}
		if _, err := tx.Secrets.Delete(ctx, orgID, linearOAuthClientSecretKey); err != nil {
			return err
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearOAuthApp, "", app.ClientID),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		if errors.Is(err, errLinearInstallStranded) {
			writeLinearInstallStranded(w, "")
			return
		}
		internalError(w, "linear-app", err)
		return
	}
	linearAppLog.Info("removed linear oauth app", "org", orgID, "user", userID)

	resp, err := s.linearAppStatus(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-app", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeLinearInstallStranded answers a change to the app that the live
// install depends on.
func writeLinearInstallStranded(w http.ResponseWriter, field string) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonConflict,
		Message: "Triage Factory is installed in Linear with this app — disconnect the installed app first",
		Field:   field,
	})
}
