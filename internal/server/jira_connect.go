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
// keyed under. `connect_available` is false until Cloud OAuth lands.
type jiraIdentityStatusResponse struct {
	Connected        bool   `json:"connected"`
	Account          string `json:"account,omitempty"`
	Host             string `json:"host"`
	ConnectAvailable bool   `json:"connect_available"`
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
		connected bool
		account   string
		host      string
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

	writeJSON(w, http.StatusOK, jiraIdentityStatusResponse{
		Connected:        connected,
		Account:          account,
		Host:             host,
		ConnectAvailable: false, // no Cloud OAuth yet (DC = paste-a-PAT)
	})
}

// jiraIdentityPATRequest carries a user-supplied Jira PAT for the
// capture-and-STORE access path. Unlike the GitHub sibling, the token is
// retained (per-user vault scope) — the user acts as themselves on board
// claims, so the credential must outlive the request.
type jiraIdentityPATRequest struct {
	PAT string `json:"pat"`
}

// jiraIdentityCaptureResponse is the success shape of the PAT-capture path: the
// bound account (display name) and the host it's keyed under, so the caller can
// reflect the just-captured identity without re-reading the status endpoint.
type jiraIdentityCaptureResponse struct {
	Account string `json:"account"`
	Host    string `json:"host"`
}

// handleJiraIdentityPAT binds the caller's Jira access from a personal access
// token they supply, STORING the token (per-user vault scope, SKY-442). It
// resolves the org's Jira host, validates the PAT against it (GET /myself),
// persists the credential under "jira_token/<host>", and derives the user's
// Jira identity from the validated /myself response. This is the Jira mirror of
// handleGitHubIdentityPAT with one difference: GitHub discards the token
// (identity only); Jira keeps it (it's access).
//
// Distinct failure shapes, like the GitHub handler: a host TF couldn't reach is
// a 502 (infra), a token the host rejected is a 422 (your action).
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
	pat := strings.TrimSpace(req.PAT)
	if pat == "" {
		badRequest(w, "A Jira personal access token is required.")
		return
	}

	// Resolve the org's Jira host the same way the status reader does, so the
	// (user, host) credential key always agrees across surfaces.
	var orgSet domain.OrgSettings
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
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

	// Validate against the org's host using the DC config form (Bearer, REST
	// v2) — the same scheme the resolver will use at request time.
	jiraUser, err := auth.ValidateJira(r.Context(), jira.DataCenterPAT(host, pat))
	if err != nil {
		// Keep the two failure shapes distinct, like the GitHub flow: a host we
		// couldn't reach (infra) vs. a token the host rejected (your action).
		if errors.Is(err, auth.ErrJiraHostUnreachable) {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("Couldn't reach %s. This is a connectivity issue between Triage Factory and your Jira server, not the token — try again.", host),
				"field": "jira_pat",
			})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "That token didn't validate against " + host + ". Double-check it and try again.",
			"field": "jira_pat",
		})
		return
	}
	if jiraUser.StableID() == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "Jira didn't return an account for that token.",
			"field": "jira_pat",
		})
		return
	}

	// Store the credential AND derive the identity in one tx — all-or-nothing,
	// so a partial bind (a stored token with no identity, or vice versa) can't
	// land. The retention is the Jira difference: GitHub discards here.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.PutUser(r.Context(), orgID, userID, jiraTokenKey(host), pat, "Jira user access token"); err != nil {
			return fmt.Errorf("store jira credential: %w", err)
		}
		if err := tx.Users.UpsertJiraIdentity(r.Context(), userID, host, jiraUser.StableID(), jiraUser.DisplayName, "pat"); err != nil {
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
	log.Printf("[jira-identity] bound user=%s account=%s host=%s org=%s source=pat (credential stored)", userID, jiraUser.StableID(), host, orgID)

	writeJSON(w, http.StatusOK, jiraIdentityCaptureResponse{Account: account, Host: host})
}
