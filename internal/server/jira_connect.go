package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Per-user Jira access — the Jira sibling of the GitHub per-user identity flow
// (github_connect.go). The two differ in ONE structural way:
//
//   - GitHub captures *identity*: it validates the user's token, records their
//     @login, and DISCARDS the token. The org's App/PAT does the acting, so the
//     user level only needs to answer whoami.
//   - Jira captures *access*: the user acts as themselves on board claims, so
//     the per-user credential must be STORED (per-user vault scope)
//     and the identity is derived from it. Retention is the whole point.
//
// DC = paste-a-PAT (this flow). Cloud OAuth (one-click Connect, see
// handleJiraConnectStart / handleJiraConnectCallback below) is the Cloud
// sibling; `connect_available` gates which of the two a given org's
// surfaces offer (false when no Atlassian OAuth app resolves for the org).
//
// The credential is stored per-user, host-keyed under "jira_token/<host>"
// (PutUser). The host comes from the org's Jira base URL
// (org_settings.jira_base_url) — single Jira host per org for v1, but the key
// is host-scoped for forward-compat with multi-host Cloud. The status reader
// and the PAT writer both resolve the host through resolveJiraHost, so the key
// always agrees across the two surfaces.

// resolveJiraHost canonicalizes the org's configured Jira base URL into the
// value the per-user credential is keyed under ("jira_token/<host>") — the
// SAME value the status reader and the PAT writer compose, so a stored
// credential reads back under the key it was written with. ok=false on an
// empty (Jira not configured) or malformed base URL, in which case there's no
// host to key a lookup off. Unlike resolveGitHubHost there's no github.com-style
// default — Jira has no canonical host, so an empty config is genuinely "not
// configured."
//
// The canonicalization itself lives in internal/jira (jira.CanonicalHost)
// so the bind flow here and the write-actor resolver (jira.Resolver.ForUser)
// compose the per-user key identically — a single source of truth.
func resolveJiraHost(orgBase string) (string, bool) {
	return jira.CanonicalHost(orgBase)
}

// jiraTokenKey is the per-user secret key the Jira access token is custodied
// under — host-scoped so a user can hold credentials on more than one Jira host
// (forward-compat; v1 is single-host). Delegates to jira.UserTokenKey so the
// reader, the writer, and the resolver stay in lockstep.
func jiraTokenKey(host string) string {
	return jira.UserTokenKey(host)
}

// jiraIdentityStatusResponse is the read-only shape the onboarding gate /
// settings section polls. `connected` is the single load-bearing bit — unlike
// GitHub (where it reflects an identity row), here it reflects a STORED
// credential, because Jira's user level holds access, not just identity.
// `account` is the bound display name (the human-recognizable label, the Jira
// analog of GitHub's @login); `host` is the org's Jira host the credential is
// keyed under. `connect_available` reflects whether an Atlassian OAuth app
// resolves for the org (per-org override / the deployment app) — the gate
// for offering the one-click Cloud "Connect" button.
type jiraIdentityStatusResponse struct {
	Connected        bool   `json:"connected"`
	Account          string `json:"account,omitempty"`
	Host             string `json:"host"`
	ConnectAvailable bool   `json:"connect_available"`
	// Deployment is the org's Jira backend ("cloud" / "data_center"), resolved
	// from the org's stored auth-method marker (falling back to host shape for a
	// pre-Cloud org). The bind surfaces key off it to render the right paste
	// fields — a Cloud org pastes an email + API token, a Data Center org a
	// single PAT. Empty when no Jira host is configured.
	Deployment string `json:"deployment,omitempty"`
}

// handleJiraIdentityStatus reports whether the caller has a stored Jira access
// credential for the active org's Jira host — the gate's "is this user set up
// for Jira" check. Read-only; any org member. An absent credential is
// connected=false (the durable supported state). Mirrors
// handleGitHubIdentityStatus, but `connected` keys on credential presence
// rather than an identity row.
//
// GET /api/orgs/{org_id}/jira/identity
func (s *Server) handleJiraIdentityStatus(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	var (
		connected  bool
		account    string
		host       string
		deployment string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		orgSet, lerr := tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		jiraHost, okHost := resolveJiraHost(orgSet.JiraBaseURL)
		if !okHost {
			// No (or malformed) Jira host configured — surface the raw value
			// for display but report not-connected (no host to key a lookup
			// off). Mirrors the GitHub status reader's bad-host branch.
			host = strings.TrimRight(strings.TrimSpace(orgSet.JiraBaseURL), "/")
			return nil
		}
		host = jiraHost

		// Resolve the org's deployment the same way the system resolver and the
		// capture handler do — from the stored auth-method marker, falling back
		// to host shape for a pre-Cloud org — so the bind surfaces render the
		// paste fields that match the scheme this handler will validate. The
		// marker is a claims-scoped org-secret read (the same door
		// integrations.Load uses on the request path).
		method, lerr := tx.Secrets.Get(r.Context(), orgID, integrations.KeyJiraAuthMethod)
		if lerr != nil {
			return lerr
		}
		deploymentEnum := jira.DeploymentForMarker(jira.AuthMethod(method), jiraHost)
		deployment = string(deploymentEnum)

		// Claims-checked GetUser (NOT GetUserSystem): this is a request
		// handler, so the credential read runs on the app pool under the
		// caller's claims, gated to (this org, this user). GetUserSystem is the
		// admin-pool door reserved for system code (the resolver, a later
		// ticket) — a request handler reaching for it would be denied on
		// tf_app. In local mode both collapse to the same keychain read.
		raw, lerr := tx.Secrets.GetUser(r.Context(), orgID, userID, jiraTokenKey(jiraHost))
		if lerr != nil {
			return lerr
		}
		if raw == "" {
			return nil // no stored credential → not connected
		}
		// "Connected" must mean the same thing as "ForUser would succeed": a
		// stored credential that's well-formed FOR THIS DEPLOYMENT. A corrupt
		// envelope, or a stale credential whose scheme no longer matches the org
		// (e.g. a DC PAT left over after a Cloud cutover), reports not-connected
		// so the paste UI re-renders and the user can re-bind — rather than
		// reporting connected and then failing every write at request time.
		cred, perr := jira.ParseUserCredential(raw)
		if perr != nil {
			jiraIdentityLog.Warn("stored credential unparsable, reporting not-connected", "user", userID, "org", orgID, "host", jiraHost, "error", perr)
			return nil
		}
		connected = cred.UsableFor(deploymentEnum)
		if !connected {
			return nil
		}

		// Identity (user_jira_identities) is host-scoped, keyed on
		// the same canonical host the credential is stored under — read it
		// for the host we just resolved. Prefer the display name for the
		// human-facing label, falling back to the stable account id.
		accountID, displayName, lerr := tx.Users.GetJiraIdentity(r.Context(), userID, jiraHost)
		if lerr != nil {
			return lerr
		}
		if displayName != "" {
			account = displayName
		} else {
			account = accountID
		}
		return nil
	}); err != nil {
		internalError(w, "jira-identity", err)
		return
	}

	// connect_available is true exactly when an Atlassian OAuth app resolves
	// for the org (per-org override or, in multi, the deployment app).
	// The frontend shows the one-click "Connect" button only then; otherwise it
	// offers just the paste-a-PAT/token path. The button drives the
	// authorize/callback flow below (handleJiraConnectStart /
	// handleJiraConnectCallback) — this endpoint just advertises that the
	// app credential it needs is in place.
	writeJSON(w, http.StatusOK, jiraIdentityStatusResponse{
		Connected:        connected,
		Account:          account,
		Host:             host,
		ConnectAvailable: s.connectAvailableForOrg(r, orgID),
		Deployment:       deployment,
	})
}

// The user-supplied Jira credential for the capture-and-STORE access path, one
// struct per deployment. Which shape applies is the ORG's deployment, not the
// caller's choice — a user cannot bind a Cloud API token against a Data Center
// site — so unlike the org-credential bind there is no discriminator in the
// body. The struct is still picked before the decode rather than after, so the
// other deployment's fields are rejected by name instead of being accepted and
// silently ignored, which is what a single both-shapes struct did.
//
// Unlike the GitHub sibling, the credential is retained (per-user vault scope):
// the user acts as themselves on board claims, so it must outlive the request.
type jiraIdentityDataCenterPAT struct {
	PAT string `json:"pat"`
}

type jiraIdentityCloudToken struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

// jiraIdentityCaptureResponse is the success shape of the PAT-capture path: the
// bound account (display name) and the host it's keyed under, so the caller can
// reflect the just-captured identity without re-reading the status endpoint.
type jiraIdentityCaptureResponse struct {
	Account string `json:"account"`
	Host    string `json:"host"`
}

// handleJiraIdentityPAT binds the caller's Jira access from a credential they
// supply, STORING it (per-user vault scope) as a UserCredential
// envelope. It resolves the org's Jira host + deployment, validates the
// credential against it (GET /myself), persists the envelope under
// "jira_token/<host>", and derives the user's Jira identity from the validated
// /myself response. This is the Jira mirror of handleGitHubIdentityPAT with one
// difference: GitHub discards the token (identity only); Jira keeps it (access).
//
// The credential shape follows the org's deployment, the same dispatch the
// system resolver uses: a Data Center org binds a PAT (Bearer, REST v2); a Cloud
// org binds an Atlassian email + API token (Basic, REST v3). The route is stable
// across both — the branch is internal. Cloud OAuth (one-click Connect, see
// handleJiraConnectCallback) is the third method, extending the same stored
// envelope shape.
//
// Distinct failure shapes, like the GitHub handler: a host TF couldn't reach is
// a 502 (infra), a credential the host rejected is a 422 (your action).
//
// POST /api/orgs/{org_id}/jira/identity/pat
func (s *Server) handleJiraIdentityPAT(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	// The body is read but not yet decoded: which struct it has to satisfy is
	// the ORG's deployment, which the read below resolves.
	body, ok := httpx.BodyBytes(w, r)
	if !ok {
		return
	}

	// Resolve the org's Jira host + deployment the same way the status reader
	// does, so the (user, host) credential key always agrees across surfaces and
	// the scheme we validate matches the one the resolver will rebuild. The
	// deployment comes from the org's stored auth-method marker (falling back to
	// host shape for a pre-Cloud org) — a claims-scoped org-secret read, the same
	// door integrations.Load uses on the request path.
	var (
		orgSet     domain.OrgSettings
		authMethod string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		authMethod, lerr = tx.Secrets.Get(r.Context(), orgID, integrations.KeyJiraAuthMethod)
		return lerr
	}); err != nil {
		internalError(w, "jira-identity", err)
		return
	}
	host, okHost := resolveJiraHost(orgSet.JiraBaseURL)
	if !okHost {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonNotConfigured, Message: "Your workspace's Jira URL isn't set up yet. Ask your admin to connect Jira in Workspace Settings first.", Field: "jira_pat"})
		return
	}
	cloud := jira.DeploymentForMarker(jira.AuthMethod(authMethod), host) == jira.DeploymentCloud

	// Build the validation Config + the credential envelope from the right
	// fields. `field` names the form input an error points at, so the surface can
	// highlight the offending field per deployment.
	var (
		cfg      jira.Config
		envelope string
		source   string
		field    string
	)
	if cloud {
		// Strict decode against the Cloud struct: a `pat` in the body is a 400
		// naming the field, not a value quietly dropped on the floor because
		// this org happens to be Cloud.
		var req jiraIdentityCloudToken
		if !httpx.DecodeJSONStrictBytes(w, body, &req) {
			return
		}
		email := strings.TrimSpace(req.Email)
		token := strings.TrimSpace(req.Token)
		field = "jira_api_token"
		if email == "" || token == "" {
			badRequest(w, "Your Atlassian account email and an API token are both required.")
			return
		}
		cfg = jira.CloudAPIToken(host, email, token)
		env, err := jira.MarshalUserCredential(jira.UserCredential{
			Method: jira.AuthMethodCloudAPIToken,
			Email:  email,
			Token:  token,
		})
		if err != nil {
			internalError(w, "jira-identity", err)
			return
		}
		envelope = env
		source = string(jira.AuthMethodCloudAPIToken)
	} else {
		// The Data Center mirror: an `email`/`token` pair here is rejected by
		// name rather than ignored.
		var req jiraIdentityDataCenterPAT
		if !httpx.DecodeJSONStrictBytes(w, body, &req) {
			return
		}
		pat := strings.TrimSpace(req.PAT)
		field = "jira_pat"
		if pat == "" {
			badRequest(w, "A Jira personal access token is required.")
			return
		}
		cfg = jira.DataCenterPAT(host, pat)
		env, err := jira.MarshalUserCredential(jira.UserCredential{
			Method: jira.AuthMethodDCPAT,
			Token:  pat,
		})
		if err != nil {
			internalError(w, "jira-identity", err)
			return
		}
		envelope = env
		// Identity source marker stays "pat" for Data Center (unchanged).
		source = "pat"
	}

	// Validate against the org's host using the scheme its deployment dictates —
	// the same scheme + version the resolver will use at request time.
	jiraUser, err := auth.ValidateJira(r.Context(), cfg)
	if err != nil {
		// Keep the two failure shapes distinct, like the GitHub flow: a host we
		// couldn't reach (infra) vs. a credential the host rejected (your action).
		if errors.Is(err, auth.ErrJiraHostUnreachable) {
			httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: fmt.Sprintf("Couldn't reach %s. This is a connectivity issue between Triage Factory and your Jira server, not the credential — try again.", host), Field: field})
			return
		}
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "That credential didn't validate against " + host + ". Double-check it and try again.", Field: field})
		return
	}
	if jiraUser.StableID() == "" {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "Jira didn't return an account for that credential.", Field: field})
		return
	}

	// Store the credential AND derive the identity in one tx — all-or-nothing,
	// so a partial bind (a stored token with no identity, or vice versa) can't
	// land. The retention is the Jira difference: GitHub discards here.
	//
	// host is the resolveJiraHost(org_settings.jira_base_url) canonical form, and
	// it is LOAD-BEARING beyond this write: assignee-centric routing's reverse
	// lookup (internal/routing assigneeTeams → UserIDsForJiraAccountSystem) keys
	// off the same org_settings.jira_base_url. If a future capture path keys the
	// identity under a different host (e.g. the integrations creds.JiraURL the
	// poller falls back to when jira_base_url is empty), the row would never be
	// found by the router and assignee routing would silently drop tasks — keep
	// this host derivation and that lookup's in agreement.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.PutUser(r.Context(), orgID, userID, jiraTokenKey(host), envelope, "Jira user access token"); err != nil {
			return fmt.Errorf("store jira credential: %w", err)
		}
		if err := tx.Users.UpsertJiraIdentity(r.Context(), userID, host, jiraUser.StableID(), jiraUser.DisplayName, source); err != nil {
			return fmt.Errorf("persist jira identity: %w", err)
		}
		// TFAC-471: audit the per-user Jira credential bind/rotate in the same
		// tx; actor is the user binding their own access, host carries the org's
		// Jira host. No target_user_id — that column is for membership/role
		// actions; the per-user credential's subject is the actor.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindJiraUser, host),
		}); err != nil {
			return fmt.Errorf("audit credential set: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "jira-identity", err)
		return
	}

	// If this user previously bound via OAuth, an access token may be cached
	// against the now-overwritten refresh token; drop it so the next read
	// reflects the credential just pasted rather than the superseded OAuth one.
	s.jiraTokenCache.Invalidate(orgID, userID, host)

	account := jiraUser.DisplayName
	if account == "" {
		account = jiraUser.StableID()
	}
	jiraIdentityLog.Info("bound user, credential stored", "user", userID, "account", jiraUser.StableID(), "host", host, "org", orgID, "source", source)

	writeJSON(w, http.StatusOK, jiraIdentityCaptureResponse{Account: account, Host: host})
}

// ---- Cloud OAuth 3LO "Connect" (per-user) ----
//
// The one-click counterpart of the paste path above, mirroring the GitHub
// Connect flow (github_connect.go). It binds the signed-in user's Atlassian
// Cloud account via OAuth consent and stores a ROTATING refresh token as the
// durable per-user credential; short-lived access tokens are minted from it on
// demand (internal/jiraoauth). Unlike GitHub Connect — which captures identity
// and discards the token — Jira Connect captures access, so the refresh token is
// retained (Jira's user level must act as the user).
//
// Available only when an Atlassian OAuth app resolves for the org (per-org
// override or, in multi, the deployment app) — the same connect_available
// gate the status endpoint surfaces. Cloud only: a Data Center org has no
// OAuth, and the start handler bounces it back to the paste path.

const jiraConnectStateCookieName = "tf_jira_connect_state"

// jiraConnectStatePath scopes the state cookie to the Jira connect callback for
// a specific org — the Jira sibling of connectStatePath.
func jiraConnectStatePath(orgID string) string {
	return "/api/orgs/" + orgID + "/jira/connect/"
}

// handleJiraConnectStart kicks off the Atlassian OAuth 3LO dance: redirect the
// browser to auth.atlassian.com/authorize with the org's resolved app client_id
// and an HMAC-signed CSRF state. Any org member binds their own access.
//
// GET /api/orgs/{org_id}/jira/connect/start?return_to=/some/path
func (s *Server) handleJiraConnectStart(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	// The org's Jira host + deployment marker — also needed at the callback to
	// pin the cloud_id, but resolved here too so we fail fast (and so a non-Cloud
	// / misconfigured org never reaches Atlassian).
	var (
		orgSet     domain.OrgSettings
		authMethod string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		authMethod, lerr = tx.Secrets.Get(r.Context(), orgID, integrations.KeyJiraAuthMethod)
		return lerr
	}); err != nil {
		internalError(w, "jira-connect", err)
		return
	}
	host, okHost := resolveJiraHost(orgSet.JiraBaseURL)
	if !okHost {
		jiraConnectLog.Warn("invalid jira base url", "org", orgID)
		s.redirectJiraConnect(w, r, orgID, returnTo, "bad_host")
		return
	}
	// OAuth 3LO is Cloud-only — Data Center authenticates with a PAT, not an
	// Atlassian app. An app can resolve for a DC org (the resolver doesn't gate
	// on deployment), so without this check connect_available could be true and
	// a DC user would be bounced through Atlassian only to fail at the cloud_id
	// match. Resolve the deployment the same way the status reader does (stored
	// marker, falling back to host shape) and bounce non-Cloud orgs back to the
	// paste path BEFORE any external round-trip.
	if jira.DeploymentForMarker(jira.AuthMethod(authMethod), host) != jira.DeploymentCloud {
		jiraConnectLog.Warn("connect attempted on non-cloud org, redirecting to paste path", "org", orgID)
		s.redirectJiraConnect(w, r, orgID, returnTo, "not_cloud")
		return
	}

	// Resolve the org's Atlassian OAuth app (per-org override → deployment).
	// A system read (the caller already authorized orgID); ErrNoAtlassianOAuthApp
	// is the expected "no app configured" state, not a backend failure.
	app, _, err := s.jiraOAuthApps.Resolve(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, jira.ErrNoAtlassianOAuthApp) {
			s.redirectJiraConnect(w, r, orgID, returnTo, "no_app")
			return
		}
		internalError(w, "jira-connect", err)
		return
	}

	csrfRaw := make([]byte, 16)
	if _, err := rand.Read(csrfRaw); err != nil {
		internalError(w, "jira-connect", err)
		return
	}
	st := connectState{
		OrgID:     orgID,
		UserID:    userID,
		CSRF:      hex.EncodeToString(csrfRaw),
		ReturnTo:  returnTo,
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := st.sign(s.deployCfg.hmacKey)
	if err != nil {
		internalError(w, "jira-connect", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     jiraConnectStateCookieName,
		Value:    signed,
		Path:     jiraConnectStatePath(orgID),
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	// Only the CSRF nonce travels through Atlassian; the org/user/return_to stay
	// in the signed cookie, off Atlassian's logs and the URL bar.
	target := jiraoauth.AuthorizeURL(app, s.jiraConnectCallbackURL(orgID), st.CSRF)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleJiraConnectCallback completes the dance: validate state/CSRF, exchange
// the code for {access, refresh} tokens, resolve the cloud_id by matching the
// org's configured site against accessible-resources, derive the bound account
// from /myself, and store the rotating refresh token as the per-user credential
// envelope ({method:cloud_oauth, cloud_id, refresh_token}). The access token is
// never stored. On any failure it redirects back to the gate page with a
// distinguishing error code.
//
// GET /api/orgs/{org_id}/jira/connect/callback?code=...&state=...
func (s *Server) handleJiraConnectCallback(w http.ResponseWriter, r *http.Request) {
	if s.deployCfg == nil {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}

	// State cookie carries the signed flow context; clear it once read so a
	// stale cookie can't be replayed.
	cookie, cookieErr := r.Cookie(jiraConnectStateCookieName)
	s.clearJiraConnectCookie(w, r, orgID)
	if cookieErr != nil {
		s.redirectJiraConnect(w, r, orgID, "", "state")
		return
	}
	cs, err := parseConnectState(cookie.Value, s.deployCfg.hmacKey)
	if err != nil {
		jiraConnectLog.Warn("state cookie parse failed", "error", err)
		s.redirectJiraConnect(w, r, orgID, "", "state")
		return
	}
	if cs.CSRF == "" || r.URL.Query().Get("state") != cs.CSRF || cs.OrgID != orgID || cs.UserID != userID {
		s.redirectJiraConnect(w, r, orgID, cs.ReturnTo, "state")
		return
	}

	returnTo := normalizeReturnTo(cs.ReturnTo)
	if returnTo == "/" {
		returnTo = "/orgs/" + orgID
	}

	// User denied consent (or Atlassian reported another OAuth error).
	if oErr := r.URL.Query().Get("error"); oErr != "" {
		jiraConnectLog.Warn("atlassian oauth error", "atlassian_error", oErr, "org", orgID)
		s.redirectJiraConnect(w, r, orgID, returnTo, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectJiraConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	// Read the org's host + resolve its OAuth app (client_id + client_secret).
	var orgSet domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "jira-connect", err)
		return
	}
	host, okHost := resolveJiraHost(orgSet.JiraBaseURL)
	if !okHost {
		s.redirectJiraConnect(w, r, orgID, returnTo, "bad_host")
		return
	}
	app, _, err := s.jiraOAuthApps.Resolve(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, jira.ErrNoAtlassianOAuthApp) {
			s.redirectJiraConnect(w, r, orgID, returnTo, "no_app")
			return
		}
		internalError(w, "jira-connect", err)
		return
	}

	// Exchange the code → {access, refresh}. The access token is used only to
	// resolve the cloud_id + whoami; the refresh token is what we store.
	tok, err := s.jiraOAuthMinter.ExchangeCode(r.Context(), app, code, s.jiraConnectCallbackURL(orgID))
	if err != nil {
		jiraConnectLog.Warn("code exchange failed", "org", orgID, "error", err)
		s.redirectJiraConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	// Resolve the cloud_id: list the sites this token can reach and match the
	// org's configured host. Single-site v1 — extra sites are ignored, and a
	// configured site that's not in the list is a clear, actionable error.
	resources, err := s.jiraOAuthMinter.AccessibleResources(r.Context(), tok.AccessToken)
	if err != nil {
		jiraConnectLog.Warn("accessible-resources failed", "org", orgID, "error", err)
		s.redirectJiraConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}
	cloudID, okCloud := matchCloudResource(resources, host)
	if !okCloud {
		jiraConnectLog.Warn("no accessible site matches host", "host", host, "org", orgID, "sites", len(resources))
		s.redirectJiraConnect(w, r, orgID, returnTo, "site_mismatch")
		return
	}

	// Derive the bound account from /myself against the gateway, using the
	// freshly-minted access token — the same client shape the resolver builds.
	jiraUser, err := auth.ValidateJira(r.Context(), jira.CloudOAuth(cloudID, tok.AccessToken))
	if err != nil {
		jiraConnectLog.Warn("whoami failed", "org", orgID, "error", err)
		s.redirectJiraConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}
	if jiraUser.StableID() == "" {
		s.redirectJiraConnect(w, r, orgID, returnTo, "connect_failed")
		return
	}

	envelope, err := jira.MarshalUserCredential(jira.UserCredential{
		Method:       jira.AuthMethodCloudOAuth,
		CloudID:      cloudID,
		RefreshToken: tok.RefreshToken,
	})
	if err != nil {
		internalError(w, "jira-connect", err)
		return
	}

	// Store the rotating refresh token AND derive the identity in one tx —
	// all-or-nothing, mirroring the paste handler. The credential is keyed under
	// the same host the assignee-routing reverse lookup uses (see the note in
	// handleJiraIdentityPAT).
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.PutUser(r.Context(), orgID, userID, jiraTokenKey(host), envelope, "Jira user access token"); err != nil {
			return fmt.Errorf("store jira oauth credential: %w", err)
		}
		if err := tx.Users.UpsertJiraIdentity(r.Context(), userID, host, jiraUser.StableID(), jiraUser.DisplayName, "connect_oauth"); err != nil {
			return fmt.Errorf("persist jira identity: %w", err)
		}
		// TFAC-471: audit the per-user Jira credential bind/rotate in the same
		// tx, mirroring the paste path (handleJiraIdentityPAT). actor = the user
		// binding their own access; host carries the org's Jira host.
		if err := tx.AccessChangeLog.Record(r.Context(), orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredential(domain.CredentialKindJiraUser, host),
		}); err != nil {
			return fmt.Errorf("audit credential set: %w", err)
		}
		return nil
	}); err != nil {
		internalError(w, "jira-connect", err)
		return
	}

	// Drop any access token cached against the SUPERSEDED refresh token — a
	// re-Connect mints a new credential, and the next per-user write must refresh
	// off the just-stored refresh token rather than serve a stale cached token.
	s.jiraTokenCache.Invalidate(orgID, userID, host)

	jiraConnectLog.Info("bound user, refresh token stored",
		"user", userID, "account", jiraUser.StableID(), "host", host, "cloud_id", cloudID, "org", orgID, "source", "connect_oauth")
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// matchCloudResource finds the cloud_id of the accessible site whose URL
// matches the org's configured Jira host. Comparison is case-insensitive and
// trailing-slash-insensitive; host is already canonical (resolveJiraHost). v1
// is single-site, so the first exact match wins and extra sites are ignored.
func matchCloudResource(resources []jiraoauth.Resource, host string) (string, bool) {
	want := strings.ToLower(strings.TrimRight(host, "/"))
	for _, res := range resources {
		if strings.ToLower(strings.TrimRight(res.URL, "/")) == want {
			return res.ID, true
		}
	}
	return "", false
}

// redirectJiraConnect bounces a failed/aborted Jira Connect flow back to the FE
// gate page with an error code (and the original return_to). The Jira sibling
// of redirectConnect; errCode "" means a bare redirect with no banner.
func (s *Server) redirectJiraConnect(w http.ResponseWriter, r *http.Request, orgID, returnTo, errCode string) {
	q := url.Values{}
	if errCode != "" {
		q.Set("error", errCode)
	}
	if rt := normalizeReturnTo(returnTo); rt != "" && rt != "/" {
		q.Set("return_to", rt)
	}
	dest := "/orgs/" + orgID + "/connect-jira"
	if enc := q.Encode(); enc != "" {
		dest += "?" + enc
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// clearJiraConnectCookie expires the Jira connect state cookie. Its
// Secure/SameSite/Path must match the SetCookie in the start handler.
func (s *Server) clearJiraConnectCookie(w http.ResponseWriter, r *http.Request, orgID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     jiraConnectStateCookieName,
		Value:    "",
		Path:     jiraConnectStatePath(orgID),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}
