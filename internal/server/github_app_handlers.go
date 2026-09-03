package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// githubAppStatusResponse is the read-only shape the Workspace Settings
// "GitHub access" panel polls to render its state machine. `app` is null
// when the org has no registered App of its own — a PAT workspace, which the
// panel offers registration, or one riding the deployment's App, which
// `using_deployment_default` marks so the panel never offers it a second
// credential it cannot hold.
type githubAppStatusResponse struct {
	App                    *githubAppInfo          `json:"app"`
	Installations          []githubAppInstallation `json:"installations"`
	UsingDeploymentDefault bool                    `json:"using_deployment_default"`
	// DeploymentAppAvailable is whether this deployment offers a deployment
	// App for a workspace to bind — a fact about the deployment, not the org,
	// carried here because the panel's empty state is where it matters: a
	// workspace with no credential is offered Connect beside registering,
	// importing and a token, and only when there is something to connect to.
	// Always false in local mode, where the managed class does not exist.
	DeploymentAppAvailable bool `json:"deployment_app_available"`
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
	// one. The panel renders a suspended installation in its own state, since
	// one that merely looked connected would explain nothing about the 403s
	// every run under it earns.
	SuspendedAt string `json:"suspended_at"`
	SuspendedBy string `json:"suspended_by"`
	// RepositorySelection is "all" or "selected" — whether the grant is every
	// repository on the account or an enumerated set — and null when the
	// mirror has not learned it yet. Three values on purpose: it decides
	// whether scope drift is even possible on this installation, and "not
	// known yet" is neither answer.
	RepositorySelection *string `json:"repository_selection"`
	// SettingsURL is the installation's settings page on GitHub — where the
	// grant is chosen, and the only place it can be changed. TF links out to
	// it and never edits the grant itself; GitHub enforces who may.
	SettingsURL string `json:"settings_url"`
}

// installationSettingsURL is GitHub's settings page for an installation,
// which lives under the organization for an org account and under the user's
// own settings for a personal one. The host is the installation's own, so a
// self-host aggregating orgs across two GitHubs links each to the right one.
func installationSettingsURL(inst domain.OrgGitHubAppInstallation) string {
	base := strings.TrimRight(inst.GitHubHost, "/")
	if base == "" {
		return ""
	}
	if strings.EqualFold(inst.AccountType, "Organization") {
		return base + "/organizations/" + url.PathEscape(inst.AccountLogin) + "/settings/installations/" + url.PathEscape(inst.InstallationID)
	}
	return base + "/settings/installations/" + url.PathEscape(inst.InstallationID)
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
// row, which is the same absence the PAT arm has. A managed workspace's panel
// is its installations: the accounts it bound, each with its suspension, the
// width of its grant, and the GitHub page where that grant is edited.
//
// deployment_app_available is not set here — it is the server's to answer, not
// the org's, and githubAppStatus stamps it.
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
		// null, not "", for a width nobody has reported: "" would decode as a
		// third selection value a client has to special-case, and null is the
		// same absence the column stores.
		if sel := inst.RepositorySelection; sel != "" {
			dto.RepositorySelection = &sel
		}
		dto.SettingsURL = installationSettingsURL(inst)
		resp.Installations = append(resp.Installations, dto)
	}
	return resp
}

// deploymentAppAvailable is whether a workspace here could bind the
// deployment App: the same three conditions the Connect route exists under.
// A deployment App read from the environment in local mode is inert, so the
// mode is part of the answer rather than the configuration alone.
func (s *Server) deploymentAppAvailable() bool {
	return s.deployCfg != nil && runmode.Current() == runmode.ModeMulti && s.deploymentApp.Configured()
}

// githubAppStatus is the status payload as every handler serves it: the
// mapped registration and installations, plus the three facts the server
// resolves around them — the registrant's name, the Connect callback URL, and
// whether the deployment offers an App to bind. health is passed through
// because only some callers have probed (an import has not), and an unprobed
// App is rendered as unknown rather than fine.
func (s *Server) githubAppStatus(ctx context.Context, orgID, userID string, class domain.GitHubCredentialClass, app *domain.OrgGitHubApp, insts []domain.OrgGitHubAppInstallation, health *githubAppWebhookHealth) githubAppStatusResponse {
	resp := newGitHubAppStatusResponse(class, app, insts, s.registrantDisplayName(ctx, orgID, userID, app), s.connectCallbackURLSafe(orgID), health)
	resp.DeploymentAppAvailable = s.deploymentAppAvailable()
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
// installation state. Read-only; any org member (or local-mode user). It
// reads the mirror and nothing else: opening the panel must never ask GitHub
// anything or kick a refresh — the refresh POST beside it is the deliberate
// gesture for that.
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

	writeJSON(w, http.StatusOK, s.githubAppStatus(ctx, orgID, userID, class, app, insts, s.webhookHealthDTO(ctx, orgID, app)))
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

	// A reconcile only makes sense for an org whose GitHub access is an App at
	// all; there is nothing of the org's to reconcile on the PAT tier. Gate on
	// the class first — a PAT org's missing registration is a 404 by decision,
	// not by the accident of a nil row — then require whatever the class
	// promises. 404 with the same shape handleGitHubAppInstallURL uses. The App
	// mirror is read through the System (claims-free) door here — the admin gate
	// already authorized orgID, and the reconcile below is itself a System
	// operation.
	//
	// Both App classes are admitted, and they reconcile through different store
	// methods rather than one method that branches, because they are different
	// operations. A workspace with its own App key reconciles by DISCOVERY: that
	// key lists its own installations and nobody else's, so the listing is
	// authoritative about whose they are. A workspace on the deployment App
	// reconciles by REFRESH: one key serves many workspaces, the listing is every
	// tenant's, and which of them belong here is a fact only the bind asserts —
	// so that path updates rows the org has already bound and creates none.
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class on installations refresh", "org", orgID)
		}
		internalError(w, "github-app", err)
		return
	}
	if !class.AppTier() {
		notFound(w, "github app")
		return
	}

	// The registration row is the BYO class's promise and the managed class's
	// impossibility: a workspace riding the shared App holds none and can hold
	// none, so requiring one would 404 exactly the orgs this handler was just
	// opened to.
	var app *domain.OrgGitHubApp
	if class == domain.GitHubCredentialClassBYOApp {
		app, err = s.githubApps.GetForOrgSystem(ctx, orgID)
		if err != nil {
			internalError(w, "github-app", err)
			return
		}
		if app == nil {
			notFound(w, "github app")
			return
		}
	}

	// The reconcile: mint an App JWT, GET /app/installations, apply the answer —
	// the same call the poller cycle makes for a BYO org. A failure here is
	// GitHub or the App credential, not the request, so it's a 502 (and logged:
	// the silent failure path is what made the original picker dead-end
	// untraceable). The managed arm is handed the deployment App the server read
	// once at boot; an unconfigured one fails here rather than resolving to
	// nothing, which is the same 502 an unusable BYO PEM produces.
	var rerr error
	if class == domain.GitHubCredentialClassManagedApp {
		rerr = s.githubApps.RefreshManagedInstallations(ctx, orgID, s.deploymentApp)
	} else {
		rerr = s.githubApps.BackfillInstallationsFromAPI(ctx, orgID)
	}
	if rerr != nil {
		githubAppLog.Error("refresh installations failed", "org", orgID, "class", class, "error", rerr)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "failed to refresh GitHub App installations" + localDetail(rerr)})
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
	writeJSON(w, http.StatusOK, s.githubAppStatus(ctx, orgID, userID, class, app, insts, s.webhookHealthDTO(ctx, orgID, app)))
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
	// github_url secret → the deployment default) so a GHES / local-mode org whose host lives
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
