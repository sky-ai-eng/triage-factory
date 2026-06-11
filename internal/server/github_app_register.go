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
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
	ip = ip.Unmap()
	// Global unicast rejects loopback, unspecified, multicast, and
	// link-local in one predicate; IsPrivate adds RFC 1918 / unique-local.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
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
func (s *Server) buildManifestAndState(ctx context.Context, orgID, userID, ownerType, ownerLogin string) (manifestPostURL, manifestJSON, ghWebOrigin string, err error) {
	var existing *domain.OrgGitHubApp
	var org *domain.Org
	var orgSettings domain.OrgSettings
	if err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var lerr error
		existing, lerr = tx.GitHubApps.GetForOrg(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		org, lerr = tx.Orgs.GetOrg(ctx, orgID)
		if lerr != nil {
			return lerr
		}
		orgSettings, lerr = tx.Orgs.GetSettings(ctx, orgID)
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

	origin, ok := gitHubWebOrigin(ghclient.ResolveBaseURL(orgSettings.GitHubBaseURL))
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
		"redirect_url": publicURL + "/api/orgs/" + orgID + "/github-app/register/callback",
		// Two callbacks: the manifest-conversion redirect (this same URL), and
		// the user-to-server Connect callback — the identity-capture flow
		// reuses this App's client_id and redirects back here after consent,
		// so its callback must be registered on the App at creation.
		"callback_urls": []string{
			publicURL + "/api/orgs/" + orgID + "/github-app/register/callback",
			s.connectCallbackURL(orgID),
		},
		"public": false,
		"default_permissions": map[string]string{
			"issues":        "write",
			"pull_requests": "write",
			"contents":      "read",
			"metadata":      "read",
			"checks":        "read",
			"actions":       "read",
			// members:read is required for GET /orgs/{org}/teams under an
			// App installation token — without it the team-mapping import
			// (settings/team/.../github-groups) and the poller's
			// deletion-reconcile both 403 and silently see zero teams in
			// App-only orgs (no PAT fallback). Read-only: TF lists teams,
			// never edits membership.
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
	// runs via API backfill instead.
	hookURL := publicURL + "/api/webhooks/github/" + orgID
	if !reachable {
		hookURL = "https://example.com/triage-factory-webhook-unconfigured"
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
// GET /api/orgs/{org_id}/github-app/register/launch?owner_type=&owner_login=
func (s *Server) handleGitHubAppRegisterLaunch(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}

	ownerType := r.URL.Query().Get("owner_type")
	ownerLogin := r.URL.Query().Get("owner_login")
	if ownerType == "" || ownerLogin == "" {
		s.renderLaunchError(w, http.StatusBadRequest, orgID,
			"Choose a GitHub account or organization before continuing.")
		return
	}
	if ownerType != "user" && ownerType != "org" {
		s.renderLaunchError(w, http.StatusBadRequest, orgID, "Invalid GitHub owner type.")
		return
	}

	manifestPostURL, manifestJSON, ghWebOrigin, err := s.buildManifestAndState(r.Context(), orgID, userID, ownerType, ownerLogin)
	if err != nil {
		switch {
		case errors.Is(err, errOrgAppExists):
			s.renderLaunchError(w, http.StatusConflict, orgID,
				"This workspace already has a GitHub App registered. Remove it before registering another.")
		case errors.Is(err, errOrgNotFound):
			s.renderLaunchError(w, http.StatusNotFound, orgID, "Workspace not found.")
		case errors.Is(err, errInvalidGitHubBase):
			log.Printf("[github-app] launch: invalid github base url for org %s", orgID)
			s.renderLaunchError(w, http.StatusInternalServerError, orgID,
				"The configured GitHub base URL is invalid. Update it in Workspace Settings.")
		default:
			log.Printf("[github-app] launch: %v", err)
			s.renderLaunchError(w, http.StatusInternalServerError, orgID,
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
		log.Printf("[github-app] render launch page: %v", err)
	}
}

// registerLaunchErrorData feeds the launch-failure page: a human message
// and a link back to the Settings GitHub panel.
type registerLaunchErrorData struct {
	Message     string
	SettingsURL string
}

// registerLaunchErrorTemplate renders a small failure page. The launch
// endpoint is reached via a top-level navigation, so a JSON body would
// dead-end the tab; this states what went wrong and links back to
// Settings. No form on the page, so its CSP needs no form-action.
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
<p><a href="{{.SettingsURL}}">&larr; Back to Settings</a></p>
</div>
</body>
</html>`))

// renderLaunchError writes the failure page with a per-response CSP and
// no-store. It overrides the global CSP the security-headers wrapper set.
func (s *Server) renderLaunchError(w http.ResponseWriter, status int, orgID, msg string) {
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := registerLaunchErrorTemplate.Execute(w, registerLaunchErrorData{
		Message:     msg,
		SettingsURL: settingsRedirectPath(orgID) + "#github-app",
	}); err != nil {
		log.Printf("[github-app] render launch error page: %v", err)
	}
}

// handleGitHubAppRegisterCallback exchanges GitHub's temporary code
// for the App's credentials, writes org_github_apps + vault secrets,
// and redirects the browser to the workspace settings page.
//
// GET /api/orgs/{org_id}/github-app/register/callback?code=...&state=...
func (s *Server) handleGitHubAppRegisterCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
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
		log.Printf("[github-app] invalid state: %v", err)
		http.Error(w, "invalid or expired state token", http.StatusUnauthorized)
		return
	}
	if state.OrgID != orgID {
		http.Error(w, "state org mismatch", http.StatusUnauthorized)
		return
	}

	// Serialize per-org so two concurrent callbacks can't both pass
	// the existence check, both call GitHub's conversion endpoint,
	// and leave an orphan App. The unique constraint is the durable
	// fallback; this mutex prevents the common case.
	regMu, _ := s.githubAppRegMu.LoadOrStore(orgID, &sync.Mutex{})
	regMu.(*sync.Mutex).Lock()
	defer regMu.(*sync.Mutex).Unlock()

	var orgSettings domain.OrgSettings
	var existing *domain.OrgGitHubApp
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		existing, lerr = tx.GitHubApps.GetForOrg(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		orgSettings, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "github-app", err)
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "org already has a GitHub App registered; remove it first",
		})
		return
	}

	ghBase := ghclient.ResolveBaseURL(orgSettings.GitHubBaseURL)
	apiBase := ghclient.APIBase(ghBase)

	conversionURL := apiBase + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	convResp, err := exchangeManifestCode(r.Context(), conversionURL)
	if err != nil {
		log.Printf("[github-app] manifest exchange: %v", err)
		internalError(w, "github-app", fmt.Errorf("GitHub manifest exchange failed"))
		return
	}

	appIDStr := fmt.Sprintf("%d", convResp.ID)
	clientSecretKey := "github_app_" + appIDStr + "_client_secret"
	pemKey := "github_app_" + appIDStr + "_pem"

	secretKeys := []string{clientSecretKey, pemKey}

	// A hookless App (the manifest omitted hook_attributes for a
	// non-public deployment) comes back with no webhook secret. Leave the
	// ref empty and store nothing rather than writing an empty Vault entry.
	hasWebhookSecret := strings.TrimSpace(convResp.WebhookSecret) != ""
	var webhookSecretKey string
	if hasWebhookSecret {
		webhookSecretKey = "github_app_" + appIDStr + "_webhook_secret"
		secretKeys = append(secretKeys, webhookSecretKey)
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.GitHubApps.CreateForOrg(r.Context(), domain.OrgGitHubApp{
			OrgID:              orgID,
			AppID:              appIDStr,
			Slug:               convResp.Slug,
			ClientID:           convResp.ClientID,
			ClientSecretRef:    clientSecretKey,
			PEMRef:             pemKey,
			WebhookSecretRef:   webhookSecretKey,
			OwnerType:          state.OwnerType,
			RegisteredByUserID: userID,
		}); err != nil {
			return err
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
		return nil
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
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "org already has a GitHub App registered; remove it first",
			})
			return
		}
		internalError(w, "github-app", err)
		return
	}

	log.Printf("[github-app] registered app_id=%s slug=%s for org=%s", appIDStr, convResp.Slug, orgID)

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
	// webhook_secret is absent when the manifest omitted hook_attributes
	// (hookless local/NAT'd Apps), so it's not load-bearing. client_id,
	// client_secret, and pem always must be present.
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
