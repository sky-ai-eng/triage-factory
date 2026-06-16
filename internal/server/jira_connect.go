package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// Per-user Jira access — the Jira sibling of the GitHub per-user identity flow
// (github_connect.go). The two differ in ONE structural way:
//
//   - GitHub captures *identity*: it validates the user's token, records their
//     @login, and DISCARDS the token. The org's App/PAT does the acting, so the
//     user level only needs to answer whoami.
//   - Jira captures *access*: the user acts as themselves on board claims, so
//     the per-user credential must be STORED (per-user vault scope, SKY-442)
//     and the identity is derived from it. Retention is the whole point.
//
// DC = paste-a-PAT (this flow). Cloud OAuth (one-click Connect) is a later
// Cloud-tier ticket; until it lands `connect_available` stays false, so the
// surfaces offer only the PAT path.
//
// The credential is stored per-user, host-keyed under "jira_token/<host>"
// (SKY-442 PutUser). The host comes from the org's Jira base URL
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
// SKY-463: the canonicalization itself lives in internal/jira (jira.CanonicalHost)
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
// resolves for the org (per-org override / deployment first-party) — the gate
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
// GET /api/orgs/{org_id}/identity/jira
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
		deployment = string(jira.DeploymentForMarker(jira.AuthMethod(method), jiraHost))

		// Claims-checked GetUser (NOT GetUserSystem): this is a request
		// handler, so the credential read runs on the app pool under the
		// caller's claims, gated to (this org, this user). GetUserSystem is the
		// admin-pool door reserved for system code (the resolver, a later
		// ticket) — a request handler reaching for it would be denied on
		// tf_app. In local mode both collapse to the same keychain read.
		token, lerr := tx.Secrets.GetUser(r.Context(), orgID, userID, jiraTokenKey(jiraHost))
		if lerr != nil {
			return lerr
		}
		connected = token != ""
		if !connected {
			return nil
		}

		// Identity (user_jira_identities) is host-scoped (SKY-397), keyed on
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
	// for the org (per-org override or, in hosted, the deployment first-party).
	// The frontend shows the one-click "Connect" button only then; otherwise it
	// offers just the paste-a-PAT/token path. The authorize/callback flow that
	// the button kicks off is a later ticket — this only advertises that the
	// app credential it needs is in place.
	writeJSON(w, http.StatusOK, jiraIdentityStatusResponse{
		Connected:        connected,
		Account:          account,
		Host:             host,
		ConnectAvailable: s.connectAvailableForOrg(r, orgID),
		Deployment:       deployment,
	})
}

// jiraIdentityPATRequest carries the user-supplied Jira credential for the
// capture-and-STORE access path. Which fields are populated depends on the org's
// deployment: a Data Center org sends a personal access token (PAT, Bearer); a
// Cloud org sends the Atlassian account Email + API Token (Basic). Unlike the
// GitHub sibling, the credential is retained (per-user vault scope) — the user
// acts as themselves on board claims, so it must outlive the request.
type jiraIdentityPATRequest struct {
	PAT   string `json:"pat"`
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
// supply, STORING it (per-user vault scope, SKY-442) as a UserCredential
// envelope. It resolves the org's Jira host + deployment, validates the
// credential against it (GET /myself), persists the envelope under
// "jira_token/<host>", and derives the user's Jira identity from the validated
// /myself response. This is the Jira mirror of handleGitHubIdentityPAT with one
// difference: GitHub discards the token (identity only); Jira keeps it (access).
//
// The credential shape follows the org's deployment, the same dispatch the
// system resolver uses: a Data Center org binds a PAT (Bearer, REST v2); a Cloud
// org binds an Atlassian email + API token (Basic, REST v3). The route is stable
// across both — the branch is internal. Cloud OAuth (one-click Connect) is a
// later ticket that extends the stored envelope with a third method.
//
// Distinct failure shapes, like the GitHub handler: a host TF couldn't reach is
// a 502 (infra), a credential the host rejected is a 422 (your action).
//
// POST /api/orgs/{org_id}/identity/jira/pat
func (s *Server) handleJiraIdentityPAT(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	var req jiraIdentityPATRequest
	if !decodeJSON(w, r, &req, "") {
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
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "Your workspace's Jira URL isn't set up yet. Ask your admin to connect Jira in Workspace Settings first.",
			"field": "jira_pat",
		})
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
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("Couldn't reach %s. This is a connectivity issue between Triage Factory and your Jira server, not the credential — try again.", host),
				"field": field,
			})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "That credential didn't validate against " + host + ". Double-check it and try again.",
			"field": field,
		})
		return
	}
	if jiraUser.StableID() == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "Jira didn't return an account for that credential.",
			"field": field,
		})
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
		return nil
	}); err != nil {
		internalError(w, "jira-identity", err)
		return
	}

	account := jiraUser.DisplayName
	if account == "" {
		account = jiraUser.StableID()
	}
	log.Printf("[jira-identity] bound user=%s account=%s host=%s org=%s source=%s (credential stored)", userID, jiraUser.StableID(), host, orgID, source)

	writeJSON(w, http.StatusOK, jiraIdentityCaptureResponse{Account: account, Host: host})
}
