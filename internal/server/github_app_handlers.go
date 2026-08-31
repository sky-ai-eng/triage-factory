package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// githubAppStatusResponse is the read-only shape the Workspace Settings
// "GitHub access" panel polls to render its state machine. `app` is null
// when the org has no registered App (the panel offers registration);
// `using_deployment_default` is true only when a deployment-level App covers
// the org — always false in this build, whose credential classes are all the
// org's own.
type githubAppStatusResponse struct {
	App                    *githubAppInfo          `json:"app"`
	Installations          []githubAppInstallation `json:"installations"`
	UsingDeploymentDefault bool                    `json:"using_deployment_default"`
	// ConnectCallbackURL is the absolute redirect_uri the App owner must register
	// on the App for per-user "Connect GitHub" OAuth to work. It's the
	// same URL buildManifestAndState bakes into a manifest-created App's
	// callback_urls — harmless there (already registered at creation), load-bearing
	// for a bring-your-own-App import whose owner must register it by hand. Empty
	// when no deployment identity is configured (deployCfg nil — e.g. a unit-test
	// server); the field is only actioned by an admin enabling OAuth.
	ConnectCallbackURL string `json:"connect_callback_url"`
	// WebhookHealth is whether GitHub is actually delivering this App's
	// webhooks to this deployment — null when there is no App, no deployment
	// identity to compare a hook URL against, or no probe answer yet. Its
	// absence is "not known", never "fine": a registered App that receives
	// nothing is the case this exists to make visible. See
	// github_webhook_health.go.
	WebhookHealth *githubAppWebhookHealth `json:"webhook_health"`
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
	// SuspendedAt is RFC3339 when the account owner has suspended this
	// installation, "" when it is live — the installation still holds its
	// grant, but GitHub refuses every token minted from it. SuspendedBy is the
	// login that suspended it, "" when unsuspended or when the source named no
	// one. Carried so the panel can render the state; nothing in the UI reads
	// them yet, and no other behaviour turns on them.
	SuspendedAt string `json:"suspended_at"`
	SuspendedBy string `json:"suspended_by"`
}

// newGitHubAppStatusResponse assembles the read-only status payload from the
// org's credential class, a loaded App registration, its installations, and the
// registrant's display name. Shared by the member-readable status GET
// (handleGitHubAppStatus) and the admin-only refresh POST
// (handleGitHubAppInstallationsRefresh) so the two can never drift in shape.
// registeredByName may be empty when the registrant is unknown or the lookup
// was skipped. connectCallbackURL is the org's Connect OAuth redirect_uri (or
// "" when no deployment identity is configured), carried so the import/connect
// form can show the exact URL the App owner must register. health is the last
// known App-webhook probe answer, or nil when none is known — a nil is rendered
// as an absent block rather than as a healthy one.
//
// class decides using_deployment_default, which is the one field on this payload
// that a nil app cannot answer for. A nil app means "no registration of your
// own", which is true of a PAT org and equally true of an org riding the
// deployment's own App — and those two want opposite answers here. The two
// own-credential classes report false; the managed class is the arm that
// reports true, and it says so from the class rather than from the absence of a
// row, which is the same absence the PAT arm has.
//
// This one boolean is the whole of the managed rendering for now: the panel
// still shows a managed workspace no App block, because it has no App of its
// own to show. What that workspace's panel should show instead — the bound
// installations and their accounts — is its own ticket.
func newGitHubAppStatusResponse(class domain.GitHubCredentialClass, app *domain.OrgGitHubApp, insts []domain.OrgGitHubAppInstallation, registeredByName, connectCallbackURL string, health *githubAppWebhookHealth) githubAppStatusResponse {
	resp := githubAppStatusResponse{
		Installations:      make([]githubAppInstallation, 0, len(insts)),
		ConnectCallbackURL: connectCallbackURL,
		WebhookHealth:      health,
	}
	switch class {
	case domain.GitHubCredentialClassPAT, domain.GitHubCredentialClassBYOApp:
		// The org owns whatever credential it has — no deployment-level App
		// stands behind it.
		resp.UsingDeploymentDefault = false
	case domain.GitHubCredentialClassManagedApp:
		// The org's credential IS the deployment's App. There is no row to
		// render and there never will be one, so app stays nil below and this
		// field is what tells the panel that nil means "riding the shared App"
		// rather than "nothing configured".
		resp.UsingDeploymentDefault = true
	default:
		// Handlers refuse an unknown class before reaching here; this arm is the
		// backstop. Render no App and claim nothing about a deployment default: a
		// panel that says "no App configured" for an org whose credential system
		// this build can't name would invite an admin to register a second one.
		githubAppLog.Error("unknown github credential class in status payload; rendering app:null", "class", class)
		// Nothing is claimed about an App this build can't name, webhook health
		// included — a health block beside app:null would describe a
		// registration the payload just declined to render.
		resp.WebhookHealth = nil
		return resp
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
		dto := githubAppInstallation{
			InstallationID: inst.InstallationID,
			AccountType:    inst.AccountType,
			AccountLogin:   inst.AccountLogin,
			InstalledAt:    inst.InstalledAt.UTC().Format(time.RFC3339),
		}
		// "" rather than the zero instant formatted, so an unsuspended
		// installation reads as unsuspended instead of as one suspended in
		// year one.
		if inst.Suspended() {
			dto.SuspendedAt = inst.SuspendedAt.UTC().Format(time.RFC3339)
			dto.SuspendedBy = inst.SuspendedBy
		}
		resp.Installations = append(resp.Installations, dto)
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
		githubAppLog.Warn("display name lookup failed", "user", app.RegisteredByUserID, "error", err)
		return ""
	}
	return name
}

// handleGitHubAppStatus returns the org's GitHub App registration +
// installation state. Read-only; any org member (or local-mode user).
//
// GET /api/orgs/{org_id}/github/app
func (s *Server) handleGitHubAppStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// The class rides along in the same transaction as the registration it
	// describes, so the payload can never pair one org's class with another
	// read's view of its App.
	var class domain.GitHubCredentialClass
	var app *domain.OrgGitHubApp
	var insts []domain.OrgGitHubAppInstallation
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		set, lerr := tx.Orgs.GetSettings(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		class = set.GitHubCredentialClass
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
	if !class.Known() {
		githubAppLog.Error("unknown github credential class on app status read", "org", orgID, "class", class)
		internalError(w, "github-app", ErrUnknownGitHubCredentialClass)
		return
	}

	writeJSON(w, http.StatusOK, newGitHubAppStatusResponse(class, app, insts, s.registrantDisplayName(ctx, orgID, userID, app), s.connectCallbackURLSafe(orgID), s.webhookHealthDTO(ctx, orgID, app)))
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
// POST /api/orgs/{org_id}/github/app/installations/refresh
func (s *Server) handleGitHubAppInstallationsRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// A reconcile only makes sense for an org that brought its own App; there is
	// nothing of the org's to reconcile in any other credential system. Gate on
	// the class first — a PAT org's missing registration is a 404 by decision,
	// not by the accident of a nil row — then require the row the class
	// promises. 404 with the same shape handleGitHubAppInstallURL uses. The App
	// mirror is read through the System (claims-free) door here — the admin gate
	// already authorized orgID, and the backfill below is itself a System
	// operation.
	//
	// The managed class is refused here DELIBERATELY, and not because there is
	// nothing to reconcile — there is, and the button would be useful. Refresh
	// means reconcile, and the reconcile below stamps this org's id on every
	// installation GET /app/installations reports. Under one App key serving many
	// workspaces that listing is every tenant's installations, so an unscoped
	// reconcile would claim them all for whoever pressed the button — and because
	// the removal diff runs against this org's own set, nothing would ever undo
	// it. The refusal lifts when the reconcile is scoped to what a workspace
	// actually bound, not before.
	// TODO(TFAC-924): open this to the managed class once the reconcile refreshes
	// bound installations only and never discovers.
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class on installations refresh", "org", orgID)
		}
		internalError(w, "github-app", err)
		return
	}
	if class != domain.GitHubCredentialClassBYOApp {
		notFound(w, "github app")
		return
	}

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
		githubAppLog.Error("refresh installations failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "failed to refresh GitHub App installations" + localDetail(err)})
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
	writeJSON(w, http.StatusOK, newGitHubAppStatusResponse(class, app, insts, s.registrantDisplayName(ctx, orgID, userID, app), s.connectCallbackURLSafe(orgID), s.webhookHealthDTO(ctx, orgID, app)))
}

// handleGitHubAppInstallURL returns the GitHub deep-link the panel's
// "Install on another GitHub account" button opens. 404 when the org
// has no registered App (nothing to install). Read-only; any org member.
//
// GET /api/orgs/{org_id}/github/app/install-url
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
