package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Sentinel errors returned by buildManifestAndState so callers can map
// them to the right HTTP status without re-querying.
var (
	errOrgAppExists      = errors.New("org already has a GitHub App registered")
	errOrgNotFound       = errors.New("org not found")
	errInvalidGitHubBase = errors.New("org github base URL is not a valid http(s) origin")
)

// gitHubWebOrigin parses a GitHub web base URL and returns its origin
// (scheme://host[:port]). The base comes from org settings and is thus
// admin-controlled, so it's validated before being concatenated into a
// CSP header / form action: the scheme must be http/https and a host must
// be present. Stripping to the origin also drops any path, query, or
// fragment that could distort CSP parsing. Returns (origin, true) on
// success, ("", false) on a malformed base.
func gitHubWebOrigin(base string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", false
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// nonPublicPrefixes are address ranges that are not reachable over the
// public Internet but that IsGlobalUnicast / IsPrivate don't already
// exclude: carrier-grade NAT, documentation/test ranges, benchmarking,
// and the reserved-for-future block.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT / shared
	netip.MustParsePrefix("192.0.2.0/24"),    // RFC 5737 TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"), // RFC 5737 TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // RFC 5737 TEST-NET-3
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC 2544 benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),     // RFC 1112 reserved (future use)
	netip.MustParsePrefix("2001:db8::/32"),   // RFC 3849 documentation
}

// isPubliclyReachable reports whether rawURL's host could be reached from
// GitHub's servers over the public Internet. A DNS name is assumed to
// resolve publicly; an IP literal must be globally-routable unicast and
// outside the private, carrier-grade-NAT, documentation, benchmark, and
// reserved ranges. GitHub validates hook_attributes.url reachability when
// converting a manifest, so an unreachable host means the manifest must
// omit the hook block.
func isPubliclyReachable(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Not an IP literal — a DNS name we assume resolves publicly.
		return true
	}
	// Same public-unicast predicate the avatar proxy's dial-time SSRF guard
	// uses (isPublicUnicastIP, avatars_handler.go) — single-sourced so the two
	// reachability checks can't drift.
	return isPublicUnicastIP(ip)
}

// buildManifestAndState assembles the GitHub App manifest JSON and the
// manifest POST URL (with an HMAC-signed state token in its query string)
// for the given org + target owner. It also returns the org's validated
// GitHub web origin so callers can scope a per-response CSP to it.
// Org-admin gating is the caller's responsibility.
//
// Returns errOrgAppExists if the org already has an App registered,
// errOrgNotFound if the org row is missing, or errInvalidGitHubBase if
// the org's configured GitHub base URL isn't a valid http(s) origin.
func (s *Server) buildManifestAndState(ctx context.Context, orgID, userID, ownerType, ownerLogin, returnTo string) (manifestPostURL, manifestJSON, ghWebOrigin string, err error) {
	var existing *domain.OrgGitHubApp
	var org *domain.Org
	if err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lerr error
		existing, lerr = tx.GitHubApps.GetForOrg(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		org, lerr = tx.Orgs.GetOrg(ctx, orgID)
		return lerr
	}); err != nil {
		return "", "", "", err
	}
	if existing != nil {
		return "", "", "", errOrgAppExists
	}
	if org == nil {
		return "", "", "", errOrgNotFound
	}

	// Resolve the GitHub web host through the resolver, not the org_settings
	// column alone: BaseURLFor applies the canonical precedence (settings →
	// github_url secret → github.com), so a GHES / local-mode org whose host
	// lives only in the credential bundle still targets the right host. A read
	// failure propagates rather than silently defaulting to github.com.
	ghBase, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve github base for org %s: %w", orgID, err)
	}
	origin, ok := gitHubWebOrigin(ghBase)
	if !ok {
		return "", "", "", errInvalidGitHubBase
	}
	ghWebOrigin = origin
	publicURL := s.deployCfg.publicURL

	appName := "Triage Factory"
	if org.Name != "" {
		appName += " (" + org.Name + ")"
	}
	if utf8.RuneCountInString(appName) > 34 {
		appName = string([]rune(appName)[:34])
	}

	reachable := isPubliclyReachable(publicURL)

	// The manifest "url" is the App's cosmetic homepage shown on its
	// GitHub page — purely informational, unlike redirect_url /
	// callback_urls / hook (which are functional and must point at this
	// deployment). When we're not publicly reachable (local / NAT),
	// publicURL is a localhost address that's meaningless on GitHub's App
	// page, so show the product homepage instead.
	homepageURL := publicURL
	if !reachable {
		homepageURL = "https://www.triagefactory.com"
	}

	manifest := map[string]any{
		"name":         appName,
		"url":          homepageURL,
		"redirect_url": publicURL + "/api/orgs/" + orgID + "/github/app/register/callback",
		// Two callbacks: the manifest-conversion redirect (this same URL), and
		// the user-to-server Connect callback — the identity-capture flow
		// reuses this App's client_id and redirects back here after consent,
		// so its callback must be registered on the App at creation.
		"callback_urls": []string{
			publicURL + "/api/orgs/" + orgID + "/github/app/register/callback",
			s.connectCallbackURL(orgID),
		},
		"public": false,
		"default_permissions": map[string]string{
			// Account permission used only by GitHub Connect's one-time identity
			// capture. The user token is discarded after /user/emails returns.
			// The resource is named "emails" — the manifest is validated against
			// GitHub's permission-resource names, not the "Email addresses"
			// display label, and an unknown key fails the whole registration
			// with "Default permission records resource is not included in the
			// list" rather than dropping the one entry.
			"emails":        "read",
			"issues":        "write",
			"pull_requests": "write",
			// contents:write — delegated agents push branches, and on a blobless
			// clone their lazy blob fetches ride the same App token, so read alone
			// can't mint the push credential a run needs (the gitproxy mints
			// contents:write; GitHub refuses to escalate past what's granted).
			"contents": "write",
			"metadata": "read",
			"checks":   "read",
			"actions":  "read",
			// statuses:read is required for the open-PR CI query to resolve.
			// We read CI off the head commit's statusCheckRollup, whose
			// contexts connection is a CheckRun | StatusContext union —
			// resolving it touches the commit-statuses resource even though we
			// inline only CheckRun fields, so checks:read alone 403s with
			// "Resource not accessible by integration". We don't request the
			// "status" webhook event: CI is read by polling and StatusContext
			// nodes are filtered out, so only the read permission is needed,
			// not status delivery.
			"statuses": "read",
			// members:read carries TWO independent jobs, and a scope-
			// minimization pass that sees only the first will drop the second
			// without noticing.
			//
			// It is required for GET /orgs/{org}/teams under an App
			// installation token — without it the team-mapping import
			// (teams/{team_id}/github-groups) and the poller's
			// deletion-reconcile both 403 and silently see zero teams in
			// App-only orgs (no PAT fallback). Read-only: TF lists teams,
			// never edits membership.
			//
			// And it is the ONLY organization permission this manifest
			// requests, which is the whole of what restricts installing this
			// App on an organization to that organization's owners: GitHub
			// applies the owners-only rule to an App that asks for
			// organization permissions, and to no other. Remove it and a repo
			// admin can install the workspace's App — a guard that lives
			// nowhere in this codebase, because it is enforced entirely by
			// GitHub in response to this one line.
			"members": "read",
		},
		"default_events": []string{
			"pull_request",
			"pull_request_review",
			"pull_request_review_comment",
			"issue_comment",
			"push",
			"check_run",
			"check_suite",
		},
	}
	// GitHub's manifest flow requires a non-blank, publicly-reachable hook
	// URL: it validates the host at manifest-creation even with
	// active:false, and a missing hook_attributes is rejected outright as
	// "Hook url cannot be blank". So we always emit the block. For
	// deployments GitHub can reach we point it at our real receiver and
	// activate it so install/content events actually arrive; for local /
	// NAT'd deployments we substitute an inert public placeholder
	// (example.com is IANA-reserved for exactly this) and keep it inactive
	// — it must never receive deliveries, and installation discovery there
	// runs via API backfill instead. The placeholder is a shared constant
	// rather than a literal because the webhook-health probe recognizes it:
	// an App wearing our own sentinel is "not configured", which is the
	// correct state for a local deployment and not a fault.
	hookURL := publicURL + "/api/webhooks/github/" + orgID
	if !reachable {
		hookURL = githubapp.PlaceholderHookURL
	}
	manifest["hook_attributes"] = map[string]any{
		"url":    hookURL,
		"active": reachable,
	}

	mj, err := json.Marshal(manifest)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal manifest: %w", err)
	}

	st := appRegisterState{
		OrgID:     orgID,
		OwnerType: ownerType,
		ReturnTo:  returnTo,
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(s.deployCfg.hmacKey)
	if err != nil {
		return "", "", "", fmt.Errorf("sign state: %w", err)
	}

	var base string
	switch ownerType {
	case "org":
		base = origin + "/organizations/" + url.PathEscape(ownerLogin) + "/settings/apps/new"
	default:
		base = origin + "/settings/apps/new"
	}
	manifestPostURL = base + "?state=" + url.QueryEscape(signed)

	return manifestPostURL, string(mj), ghWebOrigin, nil
}

// registerLaunchData feeds the bounce-page template. ManifestPostURL is
// the org's GitHub host with the signed state in its query; ManifestJSON
// is the App manifest the hidden field carries.
type registerLaunchData struct {
	ManifestPostURL string
	ManifestJSON    string
	OwnerLogin      string
}

// registerLaunchTemplate renders the script-free bounce page. The visible
// submit button IS the cross-origin POST to GitHub; there's no inline
// script, so the page needs no script-src in its CSP. html/template
// auto-escapes every field in its context (URL attr, attr value, text).
var registerLaunchTemplate = template.Must(template.New("ghapp-launch").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Register GitHub App</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#e6edf3;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{max-width:28rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:#8b949e;line-height:1.5;margin:0 0 1.5rem}
button{font:inherit;font-weight:600;background:#238636;color:#fff;border:0;border-radius:6px;padding:.65rem 1.25rem;cursor:pointer}
button:hover{background:#2ea043}
</style>
</head>
<body>
<div class="card">
<h1>Register your GitHub App</h1>
<p>You're about to create a GitHub App for <strong>{{.OwnerLogin}}</strong>. Continuing takes you to GitHub to confirm and install it.</p>
<form method="post" action="{{.ManifestPostURL}}">
<input type="hidden" name="manifest" value="{{.ManifestJSON}}">
<button type="submit">Continue to GitHub &rarr;</button>
</form>
</div>
</body>
</html>`))

// handleGitHubAppRegisterLaunch serves a minimal bounce page whose only
// job is to POST the GitHub App manifest to the org's GitHub host. It
// carries its OWN Content-Security-Policy — scoped to exactly that host
// via form-action — which overrides the global `form-action 'self'` the
// security-headers wrapper sets (the wrapper sets headers then calls the
// handler, so this Set wins). The cross-origin manifest POST the SPA
// can't make under the global CSP happens here instead. Org-admin only.
// Works in both local and multi mode.
//
// GET /api/orgs/{org_id}/github/app/register/launch?owner_type=&owner_login=
func (s *Server) handleGitHubAppRegisterLaunch(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		// No deploy config means the App-registration surface isn't wired on
		// this deployment: a route that doesn't exist here is a 404.
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	// return_to records the surface registration was launched from so both the
	// callback redirect and the error page's back-link can send the user back
	// there. Parse it BEFORE the owner-field validations so even those early
	// failures return a wizard launcher to /setup rather than dropping them on
	// Settings. Validate against the allowlist, defaulting to "settings" (the
	// historical assumption) for any other or absent value — an unknown value
	// degrades to the safe Settings landing rather than erroring.
	returnTo := r.URL.Query().Get("return_to")
	if returnTo != "setup" && returnTo != "settings" {
		returnTo = "settings"
	}

	ownerType := r.URL.Query().Get("owner_type")
	ownerLogin := r.URL.Query().Get("owner_login")
	if ownerType == "" || ownerLogin == "" {
		s.renderLaunchError(w, http.StatusBadRequest, orgID, returnTo,
			"Choose a GitHub account or organization before continuing.")
		return
	}
	if ownerType != "user" && ownerType != "org" {
		s.renderLaunchError(w, http.StatusBadRequest, orgID, returnTo, "Invalid GitHub owner type.")
		return
	}

	manifestPostURL, manifestJSON, ghWebOrigin, err := s.buildManifestAndState(r.Context(), orgID, userID, ownerType, ownerLogin, returnTo)
	if err != nil {
		switch {
		case errors.Is(err, errOrgAppExists):
			s.renderLaunchError(w, http.StatusConflict, orgID, returnTo,
				"This workspace already has a GitHub App registered. Remove it before registering another.")
		case errors.Is(err, errOrgNotFound):
			s.renderLaunchError(w, http.StatusNotFound, orgID, returnTo, "Workspace not found.")
		case errors.Is(err, errInvalidGitHubBase):
			githubAppLog.Error("launch: invalid github base url", "org", orgID)
			s.renderLaunchError(w, http.StatusInternalServerError, orgID, returnTo,
				"The configured GitHub base URL is invalid. Update it in Workspace Settings.")
		default:
			githubAppLog.Error("launch failed", "error", err)
			s.renderLaunchError(w, http.StatusInternalServerError, orgID, returnTo,
				"Something went wrong preparing the registration. Please try again.")
		}
		return
	}

	// Per-response CSP scoped to this org's GitHub origin. form-action is
	// the only origin the form may submit to; no script-src means no
	// scripts run; base-uri 'none' blocks <base> injection; frame-ancestors
	// 'none' forbids embedding; style-src 'unsafe-inline' covers the inline
	// button styling. Cache-Control: no-store because the page embeds a
	// short-lived signed state token.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; form-action "+ghWebOrigin+"; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := registerLaunchTemplate.Execute(w, registerLaunchData{
		ManifestPostURL: manifestPostURL,
		ManifestJSON:    manifestJSON,
		OwnerLogin:      ownerLogin,
	}); err != nil {
		githubAppLog.Error("render launch page failed", "error", err)
	}
}

// registerLaunchErrorData feeds the launch-failure page: a human message
// and a back-link to where registration was launched from (the wizard's
// /setup or the Settings GitHub panel), with a matching label.
type registerLaunchErrorData struct {
	Message   string
	BackURL   string
	BackLabel string
}

// registerLaunchErrorTemplate renders a small failure page. The launch
// endpoint is reached via a top-level navigation, so a JSON body would
// dead-end the tab; this states what went wrong and links back to wherever
// the user came from. No form on the page, so its CSP needs no form-action.
var registerLaunchErrorTemplate = template.Must(template.New("ghapp-launch-error").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>GitHub App registration</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;background:#0d1117;color:#e6edf3;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{max-width:28rem;padding:2rem;text-align:center}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{color:#8b949e;line-height:1.5;margin:0 0 1rem}
a{color:#58a6ff;font-weight:600;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<h1>Couldn't start registration</h1>
<p>{{.Message}}</p>
<p><a href="{{.BackURL}}">&larr; {{.BackLabel}}</a></p>
</div>
</body>
</html>`))

// renderLaunchError writes the failure page with a per-response CSP and
// no-store. It overrides the global CSP the security-headers wrapper set.
// returnTo mirrors the launch's return_to so the back-link returns the user to
// where they started: "setup" → /setup, anything else (the default) → the
// org's Settings GitHub panel.
func (s *Server) renderLaunchError(w http.ResponseWriter, status int, orgID, returnTo, msg string) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	backURL := settingsRedirectPath(orgID) + "#github-app"
	backLabel := "Back to Settings"
	if returnTo == "setup" {
		backURL = "/setup"
		backLabel = "Back to setup"
	}
	if err := registerLaunchErrorTemplate.Execute(w, registerLaunchErrorData{
		Message:   msg,
		BackURL:   backURL,
		BackLabel: backLabel,
	}); err != nil {
		githubAppLog.Error("render launch error page failed", "error", err)
	}
}

// handleGitHubAppRegisterCallback exchanges GitHub's temporary code
// for the App's credentials, writes org_github_apps + vault secrets,
// and redirects the browser to the workspace settings page.
//
// GET /api/orgs/{org_id}/github/app/register/callback?code=...&state=...
func (s *Server) handleGitHubAppRegisterCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		// No deploy config means the App-registration surface isn't wired on
		// this deployment: a route that doesn't exist here is a 404.
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	code := r.URL.Query().Get("code")
	stateRaw := r.URL.Query().Get("state")
	if code == "" || stateRaw == "" {
		badRequest(w, "missing code or state parameter")
		return
	}

	state, err := parseAppRegisterState(stateRaw, s.deployCfg.hmacKey)
	if err != nil {
		githubAppLog.Warn("invalid state", "error", err)
		httpx.WriteErrors(w, http.StatusUnauthorized, httpx.ErrorItem{
			Reason: httpx.ReasonUnauthenticated, Message: "invalid or expired state token",
		})
		return
	}
	if state.OrgID != orgID {
		httpx.WriteErrors(w, http.StatusUnauthorized, httpx.ErrorItem{
			Reason: httpx.ReasonUnauthenticated, Message: "state org mismatch",
		})
		return
	}

	// Serialize per-org so two concurrent callbacks can't both pass
	// the existence check, both call GitHub's conversion endpoint,
	// and leave an orphan App. The unique constraint is the durable
	// fallback; this closes the common case — across every control pod
	// in multi mode, not just this process (TFAC-579).
	release, err := s.acquireKeyedLock(r.Context(), &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	defer release()

	var existing *domain.OrgGitHubApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		existing, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if existing != nil {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "org already has a GitHub App registered; remove it first"})
		return
	}
	// TODO(TFAC-937): a managed_app org has no App row and passes this gate,
	// registering its own App beside live deployment-App installation rows.
	// Refuse it, naming the disconnect as the way out, once that verb exists.

	// Resolve the conversion host through the resolver (settings → github_url
	// secret → github.com), not the org_settings column alone, so a GHES /
	// local-mode org whose host lives only in the credential bundle exchanges the
	// manifest against the right host.
	ghBase, err := s.ghResolver.BaseURLFor(r.Context(), orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	apiBase := ghbase.APIBase(ghBase)

	conversionURL := apiBase + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	convResp, err := exchangeManifestCode(r.Context(), conversionURL)
	if err != nil {
		githubAppLog.Error("manifest exchange failed", "error", err)
		internalError(w, "github-app", fmt.Errorf("GitHub manifest exchange failed"))
		return
	}

	appIDStr := fmt.Sprintf("%d", convResp.ID)
	// Compose the secret-key names through integrations so the names written here
	// match the names the uninstall sweep later removes (single source of truth).
	appKeys := integrations.GitHubAppKeysFor(appIDStr)
	clientSecretKey := appKeys.ClientSecret
	pemKey := appKeys.PEM

	secretKeys := []string{clientSecretKey, pemKey}

	// The webhook secret is optional on the way back. The manifest ALWAYS
	// emits hook_attributes — GitHub rejects a blank hook url outright — so a
	// non-public deployment registers the inert placeholder above with
	// active:false, and whether GitHub returns a webhook_secret for a hook in
	// that shape is not established. Treat it as absent-or-present either way:
	// leave the ref empty and store nothing rather than writing an empty Vault
	// entry, which the receiver would read as a key every caller can sign with.
	// Absence is no longer silent — the webhook-health probe reports the
	// placeholder as "not configured".
	hasWebhookSecret := strings.TrimSpace(convResp.WebhookSecret) != ""
	var webhookSecretKey string
	if hasWebhookSecret {
		webhookSecretKey = appKeys.WebhookSecret
		secretKeys = append(secretKeys, webhookSecretKey)
	}

	// Best-effort: resolve the bot's numeric user-account id for the numeric-id
	// noreply commit email so App-bot commits link on github.com (TFAC-474).
	// Read against the already-resolved org GitHub base; a failure (404 from
	// bot-account propagation delay, network) leaves it 0 → NULL → the plain
	// noreply form, self-healing on re-register, and never blocks registration.
	botUserID := s.fetchBotUserID(r.Context(), ghBase, convResp.Slug, orgID)

	// Stage the registration when an org PAT is still live (PAT→App switch):
	// write active=false so the PAT stays the live credential — polling never
	// blips — until an atomic cutover flips the bit and deletes the PAT. A
	// fresh setup (no PAT) registers active=true as before, so the App is
	// immediately live. GitHub access is strictly either/or (TFAC-328): the
	// staged bit is how the old credential stays live across the switch window
	// without a separate mode column.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// A failed read here can't be treated as "no PAT": that would write the
		// App active beside a live PAT, breaking the App-XOR-PAT invariant this
		// whole staging rule exists to hold. Fail the request instead — nothing
		// is written yet, so the rollback leaves no App row and no vaulted
		// secrets, and the operator retries once the store recovers.
		creds, lerr := integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load org credentials: %w", lerr)
		}
		staged := creds.GitHubPAT != ""
		if staged {
			githubAppLog.Info("live pat present, staging app inactive until cutover", "org", orgID, "app_id", appIDStr)
		}
		if _, err := tx.GitHubApps.CreateForOrg(r.Context(), domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              appIDStr,
			Slug:               convResp.Slug,
			ClientID:           convResp.ClientID,
			ClientSecretRef:    clientSecretKey,
			PEMRef:             pemKey,
			WebhookSecretRef:   webhookSecretKey,
			OwnerType:          state.OwnerType,
			RegisteredByUserID: userID,
			Active:             !staged,
			BotUserID:          botUserID,
		}); err != nil {
			return err
		}
		// The org is now in the BYO-App credential system — including when the
		// App is STAGED behind a still-live PAT. The class names which system
		// the org is in; the Active bit above names which credential is live.
		// Two orthogonal facts, and this is the window where they differ.
		// Written in this transaction so the class and the registration it
		// describes can never disagree.
		if _, err := tx.Orgs.SetGitHubCredentialClass(r.Context(), orgID, domain.GitHubCredentialClassBYOApp); err != nil {
			return fmt.Errorf("set github credential class: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, clientSecretKey, convResp.ClientSecret, "GitHub App client secret"); err != nil {
			return fmt.Errorf("vault put client_secret: %w", err)
		}
		if err := tx.Secrets.Put(r.Context(), orgID, pemKey, convResp.PEM, "GitHub App private key"); err != nil {
			return fmt.Errorf("vault put pem: %w", err)
		}
		if hasWebhookSecret {
			if err := tx.Secrets.Put(r.Context(), orgID, webhookSecretKey, convResp.WebhookSecret, "GitHub App webhook secret"); err != nil {
				return fmt.Errorf("vault put webhook_secret: %w", err)
			}
		}
		// Registering an App binds the org's most powerful GitHub
		// credential (private key + client secret). Audit it here, where the
		// secrets actually land, so the change-log shows the App arriving and
		// not just the later cutover. A staged registration still records — the
		// key is stored either way, whether or not it's live yet.
		return tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON: accessDetailCredentialNamed(
				domain.CredentialKindGitHubApp, ghBase, convResp.Slug),
		})
	}); err != nil {
		// In local mode SecretStore writes go to the OS keychain
		// outside the SQLite tx. If the tx failed, clean up any
		// keychain entries that landed before the error so we don't
		// leave orphan credentials.
		if runmode.Current() == runmode.ModeLocal {
			for _, k := range secretKeys {
				_, _ = s.secrets.Delete(r.Context(), orgID, k)
			}
		}
		var exists *db.ErrGitHubAppExists
		if errors.As(err, &exists) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "org already has a GitHub App registered; remove it first"})
			return
		}
		internalError(w, "github-app", err)
		return
	}

	githubAppLog.Info("registered app", "app_id", appIDStr, "slug", convResp.Slug, "org", orgID)

	// The org now has a webhook secret where it had none (or a new one where
	// it had another). GitHub starts delivering as soon as the App is
	// installed, so drop whatever the receiver cached — a stale negative from
	// a delivery that arrived mid-registration would reject real deliveries
	// until it expired.
	s.invalidateWebhookSecret(orgID)

	// Land the user back where they launched registration from. The wizard
	// (rt=setup) returns to /setup, which resumes on the now-current "Install
	// the App" step rather than teleporting past it into team config; a
	// Settings launch (the default) returns to the GitHub panel. /setup is a
	// live route in both local and multi mode, so no mode branch is needed —
	// only the Settings path differs per mode (settingsRedirectPath).
	if state.ReturnTo == "setup" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	http.Redirect(w, r, settingsRedirectPath(orgID)+"#github-app", http.StatusFound)
}

// settingsRedirectPath returns the SPA route for the Settings page in
// the active mode. Local mode mounts Settings at /settings; multi mode
// at /orgs/{org_id}/settings. The #github-app fragment tells the
// Settings page to select the Workspace tab and refetch App status.
func settingsRedirectPath(orgID string) string {
	if runmode.Current() == runmode.ModeLocal {
		return "/settings"
	}
	return "/orgs/" + orgID + "/settings"
}

// fetchBotUserID best-effort resolves the numeric GitHub user-account id of an
// App's bot ("<slug>[bot]") via an unauthenticated GET /users/{login} against
// the org's GitHub base, for the numeric-id noreply commit email that links
// App-bot commits on github.com (TFAC-474). Both registration flows (manifest +
// BYOA import) call it just before persisting the App row.
//
// Unauthenticated by design: at registration no installation token exists yet,
// and /users/{login} is public — registration is rare, so the unauthenticated
// rate limit is not a concern. A failure (a 404 while the freshly-created bot
// account propagates, a network blip, a GHES quirk) returns 0 with a warning
// logged here, so the caller stores NULL and the resolver falls back to the
// plain "<slug>[bot]@..." form; it self-heals on the next re-register. Never
// blocks registration.
func (s *Server) fetchBotUserID(ctx context.Context, ghBase, slug, orgID string) int64 {
	botLogin := slug + "[bot]"
	id, err := ghclient.NewClient(ghBase, "").UserID(ctx, botLogin)
	if err != nil {
		githubAppLog.Warn("resolve bot user id failed; storing NULL (plain noreply commit email until re-register)",
			"org", orgID, "bot_login", botLogin, "error", err)
		return 0
	}
	return id
}

// --- manifest code exchange ---

type manifestConversionResponse struct {
	ID            int    `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

var manifestHTTPClient = &http.Client{Timeout: 30 * time.Second}

func exchangeManifestCode(ctx context.Context, conversionURL string) (*manifestConversionResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conversionURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := manifestHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out manifestConversionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	// webhook_secret is not required here. The manifest always carries a
	// hook_attributes block, but a deployment GitHub cannot reach gets the
	// inactive placeholder one, and whether a conversion returns a secret for
	// that is unverified — so this tolerates its absence instead of asserting
	// either behaviour. client_id, client_secret, and pem always must be
	// present.
	if strings.TrimSpace(out.ClientID) == "" ||
		strings.TrimSpace(out.ClientSecret) == "" ||
		strings.TrimSpace(out.PEM) == "" {
		return nil, fmt.Errorf("incomplete response from GitHub (missing client_id, client_secret, or pem)")
	}
	return &out, nil
}

// --- state token (HMAC-signed, ~10min TTL) ---

type appRegisterState struct {
	OrgID string `json:"org_id"`
	// OwnerType ("user"/"org") is carried in the signed state so the callback
	// persists the owner the user picked at launch — the signed token is the
	// source of truth at callback time, not a re-supplied query param, so it
	// can't be tampered with mid-flow.
	OwnerType string `json:"ot"`
	// ReturnTo ("setup"/"settings") records where registration was launched
	// from so the callback redirects the user back there instead of always
	// assuming Settings. The launch handler always populates it (an unknown or
	// absent return_to defaults to "settings"), so a current token carries an
	// explicit value; omitempty only drops a zero value, which an older token
	// minted before this field existed carries. The callback reads any value
	// other than "setup" as the Settings redirect, so an omitted/empty rt stays
	// valid.
	ReturnTo  string `json:"rt,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

func (s appRegisterState) sign(key [32]byte) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseAppRegisterState(raw string, key [32]byte) (*appRegisterState, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode mac: %w", err)
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, errors.New("mac mismatch")
	}
	var s appRegisterState
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if timeNow().Unix() > s.ExpiresAt {
		return nil, errors.New("expired")
	}
	return &s, nil
}
