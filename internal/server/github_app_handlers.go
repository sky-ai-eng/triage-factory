package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// githubAppStatusResponse is the read-only shape the Workspace Settings
// "GitHub access" panel polls to render its state machine. `app` is null
// when the org has no registered App (the panel offers registration);
// `using_hosted_default` is true only when a deployment-default App
// covers the org — always false in local + self-host.
type githubAppStatusResponse struct {
	App                *githubAppInfo          `json:"app"`
	Installations      []githubAppInstallation `json:"installations"`
	UsingHostedDefault bool                    `json:"using_hosted_default"`
	// ConnectCallbackURL is the absolute redirect_uri the App owner must register
	// on the App for per-user "Connect GitHub" OAuth to work. It's the
	// same URL buildManifestAndState bakes into a manifest-created App's
	// callback_urls — harmless there (already registered at creation), load-bearing
	// for a bring-your-own-App import whose owner must register it by hand. Empty
	// when no deployment identity is configured (deployCfg nil — e.g. a unit-test
	// server); the field is only actioned by an admin enabling OAuth.
	ConnectCallbackURL string `json:"connect_callback_url"`
}

type githubAppInfo struct {
	AppID                   string `json:"app_id"`
	Slug                    string `json:"slug"`
	OwnerType               string `json:"owner_type"`
	RegisteredAt            string `json:"registered_at"`
	RegisteredByDisplayName string `json:"registered_by_display_name"`
	// Active is false while the registration is STAGED — registered during a
	// PAT→App switch but not yet cut over, so the PAT is still the live
	// credential (TFAC-328). The Setup/Settings UX reads this to resolve the
	// live mode, paint the "switch pending" mode-card state, and show the
	// staged-switch banner; without it a staged-app-plus-PAT org is
	// indistinguishable from a live App. true once a cutover activates it.
	Active bool `json:"active"`
}

type githubAppInstallation struct {
	InstallationID string `json:"installation_id"`
	AccountType    string `json:"account_type"`
	AccountLogin   string `json:"account_login"`
	InstalledAt    string `json:"installed_at"`
}

// newGitHubAppStatusResponse assembles the read-only status payload from a
// loaded App registration, its installations, and the registrant's display
// name. Shared by the member-readable status GET (handleGitHubAppStatus) and
// the admin-only refresh POST (handleGitHubAppInstallationsRefresh) so the two
// can never drift in shape. A nil app yields app:null (the org has no
// registration); registeredByName may be empty when the registrant is unknown
// or the lookup was skipped.
// connectCallbackURL is the org's Connect OAuth redirect_uri (or "" when no
// deployment identity is configured), carried so the import/connect form can
// show the exact URL the App owner must register.
func newGitHubAppStatusResponse(app *domain.OrgGitHubApp, insts []domain.OrgGitHubAppInstallation, registeredByName, connectCallbackURL string) githubAppStatusResponse {
	resp := githubAppStatusResponse{
		Installations:      make([]githubAppInstallation, 0, len(insts)),
		UsingHostedDefault: false,
		ConnectCallbackURL: connectCallbackURL,
	}
	if app != nil {
		resp.App = &githubAppInfo{
			AppID:                   app.AppID,
			Slug:                    app.Slug,
			OwnerType:               app.NormalizedOwnerType(),
			RegisteredAt:            app.RegisteredAt.UTC().Format(time.RFC3339),
			RegisteredByDisplayName: registeredByName,
			Active:                  app.Active,
		}
	}
	for _, inst := range insts {
		resp.Installations = append(resp.Installations, githubAppInstallation{
			InstallationID: inst.InstallationID,
			AccountType:    inst.AccountType,
			AccountLogin:   inst.AccountLogin,
			InstalledAt:    inst.InstalledAt.UTC().Format(time.RFC3339),
		})
	}
	return resp
}

// registrantDisplayName best-effort resolves the display name of the user who
// registered the org's App, for the status payload's
// registered_by_display_name. The field is cosmetic, so a missing user or a
// read error degrades to "" (logged) rather than failing the caller; "" is
// also returned when the App records no registrant.
func (s *Server) registrantDisplayName(ctx context.Context, orgID, userID string, app *domain.OrgGitHubApp) string {
	if app == nil || app.RegisteredByUserID == "" {
		return ""
	}
	var name string
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var derr error
		name, derr = tx.Users.GetDisplayName(ctx, app.RegisteredByUserID)
		return derr
	}); err != nil {
		log.Printf("[github-app] display name for %s: %v", app.RegisteredByUserID, err)
		return ""
	}
	return name
}

// handleGitHubAppStatus returns the org's GitHub App registration +
// installation state. Read-only; any org member (or local-mode user).
//
// GET /api/orgs/{org_id}/github-app
func (s *Server) handleGitHubAppStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var app *domain.OrgGitHubApp
	var insts []domain.OrgGitHubAppInstallation
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		insts, lerr = tx.GitHubApps.ListInstallationsForOrg(ctx, orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}

	writeJSON(w, http.StatusOK, newGitHubAppStatusResponse(app, insts, s.registrantDisplayName(ctx, orgID, userID, app), s.connectCallbackURLSafe(orgID)))
}

// connectCallbackURLSafe returns the org's Connect OAuth callback URL, or ""
// when no deployment identity is configured (deployCfg nil). connectCallbackURL
// itself dereferences s.deployCfg.publicURL and so assumes a configured
// deployment (its other callers gate on s.deployCfg != nil first); this wrapper
// lets the status payload carry the field unconditionally, degrading to "" for a
// deployment-less server (a unit-test rig) instead of panicking.
func (s *Server) connectCallbackURLSafe(orgID string) string {
	if s.deployCfg == nil {
		return ""
	}
	return s.connectCallbackURL(orgID)
}

// handleGitHubAppInstallationsRefresh reconciles the org's App installation
// mirror against GitHub on demand, then returns the refreshed status payload.
//
// This is the UI-driven half of installation discovery the D11 umbrella always
// planned ("API backfill on poller cycle + UI panel refresh") — the refresh
// call site that was never built. It breaks a local-mode chicken-and-egg: App
// installations are otherwise only discovered inside the GitHub poll cycle,
// which returns early when the org has zero configured repos — exactly the
// first-run state in which the repo picker needs them. The setup wizard's
// install step and the Settings App panel are the (admin-only) callers.
//
// Mode-agnostic by design: multi-mode keeps the mirror fresh via webhooks, but
// a manual reconcile is the same harmless GET /app/installations the poller
// runs, so it's offered everywhere rather than gated on runmode.
//
// POST /api/orgs/{org_id}/github-app/installations/refresh
func (s *Server) handleGitHubAppInstallationsRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// A reconcile only makes sense for an org that has registered an App; with
	// none there are no live installations to list. 404 with the same shape
	// handleGitHubAppInstallURL uses. The App mirror is read through the System
	// (claims-free) door here — the admin gate already authorized orgID, and
	// the backfill below is itself a System operation.
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	// The reconcile: mint an App JWT, GET /app/installations, upsert every live
	// installation and soft-remove any GitHub no longer reports — the same call
	// the poller cycle makes. A failure here is GitHub or the App credential,
	// not the request, so it's a 502 (and logged: the silent failure path is
	// what made the original picker dead-end untraceable).
	if err := s.githubApps.BackfillInstallationsFromAPI(ctx, orgID); err != nil {
		log.Printf("[github-app] refresh installations for org %s: %v", orgID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// Re-read the freshly-reconciled mirror so the caller gets current
	// installation state in one round trip, in the same shape the status GET
	// serves.
	insts, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	writeJSON(w, http.StatusOK, newGitHubAppStatusResponse(app, insts, s.registrantDisplayName(ctx, orgID, userID, app), s.connectCallbackURLSafe(orgID)))
}

// handleGitHubAppInstallURL returns the GitHub deep-link the panel's
// "Install on another GitHub account" button opens. 404 when the org
// has no registered App (nothing to install). Read-only; any org member.
//
// GET /api/orgs/{org_id}/github-app/install-url
func (s *Server) handleGitHubAppInstallURL(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var app *domain.OrgGitHubApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		app, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	// Resolve the install deep-link host through the resolver (settings →
	// github_url secret → github.com) so a GHES / local-mode org whose host lives
	// only in the credential bundle links to the right host instead of github.com.
	ghBase, err := s.ghResolver.BaseURLFor(r.Context(), orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url": ghBase + "/apps/" + app.Slug + "/installations/new",
	})
}
