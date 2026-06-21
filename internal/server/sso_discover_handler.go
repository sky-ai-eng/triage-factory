package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Identifier-first SSO discovery (TFAC-427, epic TFAC-422). In a multi-org
// build an anonymous visitor's org is unknown, so the login page can't show an
// org-specific "Sign in with SSO" button up front. Instead it is
// identifier-first: the visitor types their work email, TF looks up the exact
// email domain, and either routes them to that org's SAML connection or falls
// back to GitHub (the universal bootstrap-floor login).
//
// This file is the lookup half — POST /api/sso/discover. It is deliberately a
// pre-login, no-side-effect read: the caller has no session yet (so it can't go
// through s.api/apiMutating, which require one), and it mutates nothing. The
// SQLite domain store is a stub, so the endpoint is multi-mode only and answers
// {sso:false} in local mode.
//
// # Privacy (the TFAC-422 trust model)
//
// app.triagefactory.com runs the identical image as a self-host, and its orgs
// are mutually-distrusting paying customers. So the response must never reveal
// org identity or which org owns a domain — it carries only "SSO available +
// where to start" or "not". No org names, no connection ids, no enumeration
// signal beyond "this domain has SSO here" (which the My Apps bookmark tile
// already exposes for any domain its owner configured). The start_url's opaque
// provider_id is the single value handed back, and it names no org.

// ssoDiscoverRequest is the discovery endpoint's request body. Only Email is
// required; ReturnTo (the path the visitor was headed to before being bounced
// to login) rides along so the start_url can carry it through the SAML
// roundtrip — the login page knows it, the server stitches it into the URL.
type ssoDiscoverRequest struct {
	Email    string `json:"email"`
	ReturnTo string `json:"return_to"`
}

// ssoDiscoverResponse is the discovery reply — minimal by privacy design (see
// the file doc). A match is {sso:true, start_url}; anything else is {sso:false}
// (omitting start_url) and the login page continues with GitHub.
type ssoDiscoverResponse struct {
	SSO      bool   `json:"sso"`
	StartURL string `json:"start_url,omitempty"`
}

// handleSSODiscover resolves a typed work email to an SSO start URL, or signals
// "use GitHub". An anonymous visitor POSTs {email}; TF extracts the exact
// domain and looks for a VERIFIED sso_domains row routing to an enabled
// connection.
//
// Exact-match, locked (TFAC-422): alice@eng.corp.com routes only if
// eng.corp.com is verified; a corp.com claim does NOT cover it. The store query
// is `lower(domain) = lower($1)` — no suffix, no wildcard, no longest-match,
// ever.
//
// Every non-match — unknown domain, pending-only claim, disabled connection,
// unparseable email, local mode — collapses to {sso:false}, because GitHub is
// always the working fallback and the login page must never be left with a dead
// SSO redirect or a 400 it has to special-case.
//
// POST /api/sso/discover  body: { "email": "user@corp.com", "return_to": "/path" }
func (s *Server) handleSSODiscover(w http.ResponseWriter, r *http.Request) {
	var body ssoDiscoverRequest
	if !decodeJSON(w, r, &body, "") {
		return
	}

	// Multi-mode only: local mode (N=1) registers no IdP, so SSO is never
	// available. Answer {sso:false} (→ GitHub) rather than 404 — this is the
	// public, pre-login "which login do I use?" probe, not the admin surface
	// that 404s for non-disclosure, and the SQLite domain store is a stub that
	// would error. The login page is multi-mode-only anyway, so a local-mode
	// caller is a defensive case the UI never reaches.
	if runmode.Current() == runmode.ModeLocal {
		writeJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	domainName, ok := emailDomain(body.Email)
	if !ok {
		// Not a parseable email → no domain to route on. {sso:false} keeps the
		// page on GitHub rather than forcing it to special-case a 400 for what
		// is usually just a half-typed address.
		writeJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	// Admin pool (BYPASSRLS): the caller is pre-login with no membership, so the
	// verified domain itself is the lookup authorization — RLS can't express a
	// pre-login cross-org read. Verified rows only; pending claims are inert.
	route, err := s.allStores.SSODomains.GetVerifiedByDomain(r.Context(), domainName)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	if route == nil || !route.Enabled {
		// No verified row, or it routes to a disabled connection. A disabled
		// connection must not begin a flow — handleSAMLStart would 404 the
		// start_url — so collapse it into {sso:false}: the user gets the working
		// GitHub login instead of a dead SSO redirect.
		writeJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	// Match: hand back the SAML start URL the login page redirects to — the same
	// SP-initiated endpoint (handleSAMLStart) the My Apps bookmark tile targets.
	// The URL carries the opaque provider_id and nothing else (no org id, name,
	// or connection id), so it never reveals who owns the domain. return_to is
	// run through the same open-redirect guard the start endpoint applies, so a
	// hostile value can't smuggle an off-site redirect into the URL TF emits.
	writeJSON(w, http.StatusOK, ssoDiscoverResponse{
		SSO:      true,
		StartURL: ssoStartURL(route.ProviderID, normalizeReturnTo(body.ReturnTo)),
	})
}

// emailDomain extracts the lower-cased email domain for the exact-match
// discovery lookup. It requires exactly one '@' with a non-empty local part and
// a non-empty domain, trims surrounding whitespace and any trailing FQDN-root
// dot (so "Alice@Corp.com." and "alice@corp.com" both resolve to "corp.com",
// mirroring normalizeSSODomain's stored form), and returns the FULL domain
// after the '@' — eng.corp.com, never the registrable corp.com — because the
// match is exact. Returns ("", false) for anything that isn't a plain address.
func emailDomain(raw string) (string, bool) {
	e := strings.TrimSpace(raw)
	at := strings.IndexByte(e, '@')
	if at <= 0 {
		// No '@', or it's the first character (empty local part).
		return "", false
	}
	rest := e[at+1:]
	if strings.IndexByte(rest, '@') >= 0 {
		// A second '@' — not a plain address; don't guess at the domain.
		return "", false
	}
	domain := strings.ToLower(strings.TrimRight(rest, "."))
	if domain == "" {
		return "", false
	}
	// A real domain carries no interior whitespace; reject it for a clean
	// "valid bare domain or false" contract (the store would simply not match
	// such a value, but rejecting here keeps the call site's intent clear).
	if strings.ContainsAny(domain, " \t\r\n") {
		return "", false
	}
	return domain, true
}

// ssoStartURL builds the SP-initiated SAML start URL the discovery response
// hands back: the same /api/auth/oauth/saml endpoint handleSAMLStart serves and
// the My Apps tile points at, carrying the connection's provider_id and the
// (already-normalized) return_to. url.Values.Encode sorts keys, yielding
// provider_id before return_to and percent-encoding both.
func ssoStartURL(providerID, returnTo string) string {
	q := url.Values{}
	q.Set("provider_id", providerID)
	q.Set("return_to", returnTo)
	return "/api/auth/oauth/saml?" + q.Encode()
}
