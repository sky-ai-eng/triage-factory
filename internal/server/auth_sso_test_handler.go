package server

import (
	"html/template"
	"net/http"
	"strings"
)

// This file is the verify-before-enforce SSO test (TFAC-431): the admin-gated
// test-start endpoint and the callback's no-write test branch. It is a test MODE
// of the SAML login flow in auth_handlers.go — the pieces shared with a real
// login (the Test flag on stateClaims, issueOAuthStateCookie's test parameter,
// and the `if state.Test` fork in handleOAuthCallback) live there; everything
// that is test-only lives here.

// handleSAMLConnectionTestStart begins a verify-before-enforce SSO test
// (TFAC-431): an org admin round-trips a REAL sign-in through their own
// connection's IdP to confirm it's wired up correctly BEFORE enabling it. The
// common failures are Entra-side (wrong ACS/Entity ID, cert mismatch, test user
// unassigned, the assertion missing the email claim) and only surface in a live
// round-trip — metadata validation at registration can't catch them.
//
// It is the test-mode sibling of handleSAMLStart, with two deliberate
// differences:
//
//   - Admin-gated, not pre-login. Only an org admin testing THEIR OWN org's
//     connection may start it (adminGate: 404 in local mode / for a non-admin,
//     non-disclosure). The provider isn't a query param — it's resolved from the
//     caller's active-org connection, so an admin can only ever test their own.
//   - It allows a NOT-yet-enabled connection (it deliberately skips the enabled
//     check handleSAMLStart enforces) — testing before enabling is the whole
//     point. A disabled connection can't be reached pre-login, so relaxing the
//     gate here doesn't open a login path.
//
// The browser opens this in a popup so the admin's current TF session is
// untouched: the test path mints no session (see completeSAMLConnectionTest), so
// the sid cookie is never overwritten even though the admin authenticates at the
// IdP again.
//
// GET /api/sso/connection/test
func (s *Server) handleSAMLConnectionTestStart(w http.ResponseWriter, r *http.Request) {
	// authDeps gates the whole multi-mode auth stack; nil means local mode (or an
	// unwired multi deploy) where SSO doesn't exist — 404 ("feature absent"),
	// matching adminGate's local-mode posture and the connection handlers.
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}
	// Reuse the connection handler's admin gate + connection read so the
	// test-start enforces the exact same authorization and org-scoping as the
	// rest of the SSO admin surface (active org, org-admin, app-pool RLS read).
	orgID, userID, ok := s.ssoConn.adminGate(w, r)
	if !ok {
		return
	}
	conn, err := s.ssoConn.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "sso", err)
		return
	}
	if conn == nil {
		// Nothing registered to test. 404 (there is no connection resource).
		http.NotFound(w, r)
		return
	}
	// NOTE: no conn.Enabled check — a not-yet-enabled connection is exactly what
	// this verifies (the verify-before-enforce safety step).

	// The test path renders a result page, never a redirect, so the state's
	// ReturnTo is never consumed — pin it to "/" rather than reading a
	// ?return_to that would silently do nothing (and mislead an admin who set it).
	codeVerifier, csrf, ok := s.issueOAuthStateCookie(w, r, "/", conn.ProviderID, true /* test */)
	if !ok {
		return
	}

	redirectTo := s.deployCfg.publicURL + "/api/auth/callback?state=" + csrf
	location, err := s.authDeps.gotrueSSO(r.Context(), conn.ProviderID, redirectTo, pkceChallenge(codeVerifier))
	if err != nil {
		authLog.Warn("saml test start: gotrue /sso failed", "org", orgID, "error", err)
		http.Error(w, "sso test start failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// samlTestResult is the outcome of a verify-before-enforce SSO test round-trip
// (TFAC-431), rendered into the admin's popup by renderSAMLTestResult. Pass is
// the headline; the rest are the diagnostic detail an admin needs to fix an
// Entra-side misconfig. Every string field is IdP-derived and is rendered
// through html/template's auto-escaping.
type samlTestResult struct {
	Pass          bool   // the round-trip completed and carried a readable email
	Email         string // authenticated email from the assertion ("" → fail)
	EmailVerified bool   // GoTrue's email_verified flag for that email
	Domain        string // the email's domain, for the domain-match line
	DomainMatch   bool   // domain is a VERIFIED sso_domains row for THIS org
	Reason        string // failure explanation (empty on pass)
}

// completeSAMLConnectionTest is the callback's verify-before-enforce test branch
// (TFAC-431), reached only when TF's own signed state carries Test (set by the
// admin-gated test-start). It completes the GoTrue code exchange + JWT verify —
// which together prove the IdP's assertion was accepted by GoTrue's ACS and is
// readable — then SHORT-CIRCUITS before any writes and renders a pass/fail page.
//
// It deliberately does NOT call resolveOrCreatePrincipal, JIT, or mint a
// session, so a test writes NO public.users principal, NO public.user_identities
// link, NO org_membership, and NO session. (GoTrue still creates an auth.users
// row for the SSO login as a side effect of processing any assertion — harmless:
// TF binds nothing to it, so it stays an orphan login row that a later REAL login
// resolves/links from by verified email.)
//
// Failure modes all render a fail page rather than a 4xx, because for a test the
// failure IS the result the admin came to see: a GoTrue ?error= ACS rejection
// (cert mismatch / unsigned / unassigned test user), a failed code exchange, a
// token that won't verify, or an assertion missing the email claim.
func (s *Server) completeSAMLConnectionTest(w http.ResponseWriter, r *http.Request, state *stateClaims) {
	// GoTrue's SAML ACS redirects back with ?error=&error_description= when it
	// rejects the assertion upstream (unsigned, cert mismatch, bad audience /
	// destination). Surface that as the failure rather than a generic "no code",
	// since a rejected assertion is the single most common thing this test exists
	// to catch.
	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		s.renderSAMLTestResult(w, samlTestResult{
			Reason: samlTestErrorReason(errCode, r.URL.Query().Get("error_description")),
		})
		return
	}

	authCode := r.URL.Query().Get("code")
	if authCode == "" {
		s.renderSAMLTestResult(w, samlTestResult{
			Reason: "The identity provider returned no authorization code, so the sign-in did not complete.",
		})
		return
	}

	// Complete the PKCE exchange + JWT verify. A failure of either is itself a
	// meaningful result (the IdP / signing cert is misconfigured), so render a
	// fail page rather than the 4xx a real login returns.
	accessToken, _, _, err := s.authDeps.gotrueExchange(r.Context(), authCode, state.CodeVerifier)
	if err != nil {
		authLog.Warn("saml test: code exchange failed", "error", err)
		s.renderSAMLTestResult(w, samlTestResult{
			Reason: "Could not exchange the sign-in code with the identity provider — the assertion was likely rejected. Check the signing certificate and that the test user is assigned to the app.",
		})
		return
	}
	claims, err := s.authDeps.verifier.Verify(accessToken)
	if err != nil {
		authLog.Warn("saml test: token verify failed", "error", err)
		s.renderSAMLTestResult(w, samlTestResult{
			Reason: "The identity provider completed sign-in, but the returned token failed verification.",
		})
		return
	}

	// SHORT-CIRCUIT here: only READ what the assertion carried — no principal
	// resolve/link, no JIT, no session (the whole point of a test).
	res := samlTestResult{
		Email:         strings.TrimSpace(claims.Email),
		EmailVerified: claims.EmailVerified,
	}
	if res.Email == "" {
		// The most common Entra misconfig: the enterprise app doesn't map the
		// user's email / UPN into the assertion's email claim. The round-trip
		// "succeeded" but carried no identity a real login could key on — a fail.
		res.Reason = "Sign-in completed, but the assertion carried no email claim. In your identity provider, map the user's email (or UPN) to the email claim."
		s.renderSAMLTestResult(w, res)
		return
	}

	// Domain match (informational, never gates pass/fail): would a REAL
	// identifier-first login (TFAC-427) with this email route to SSO here? Yes
	// iff the email's domain is a VERIFIED sso_domains row for THIS org. The
	// SP-initiated / My Apps tile flow works regardless of domain verification,
	// so "no" just tells the admin the email-first login isn't wired up yet.
	// Scoped to the connection's org via the provider binding, so a domain
	// verified to a DIFFERENT org never reads as a match (and isn't disclosed).
	if dom, ok := emailDomain(res.Email); ok {
		res.Domain = dom
		binding, berr := s.allStores.SSOConnections.GetByProviderID(r.Context(), state.ProviderID)
		if berr != nil {
			authLog.Warn("saml test: provider lookup for domain-match failed", "error", berr)
		} else if binding != nil {
			route, derr := s.allStores.SSODomains.GetVerifiedByDomain(r.Context(), dom)
			if derr != nil {
				authLog.Warn("saml test: verified-domain lookup failed", "error", derr)
			} else if route != nil && route.OrgID == binding.OrgID {
				res.DomainMatch = true
			}
		}
	}

	res.Pass = true
	s.renderSAMLTestResult(w, res)
}

// samlTestErrorReason builds the failure message for a GoTrue ACS rejection (the
// ?error / ?error_description it redirects back with). The description is the
// actionable part when present; it's IdP/GoTrue-derived and rendered through
// html/template escaping by renderSAMLTestResult, so it's surfaced as-is.
func samlTestErrorReason(code, desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "The identity provider rejected the sign-in (" + code + ")."
	}
	return "The identity provider rejected the sign-in: " + desc
}

// samlTestResultTmpl renders the verify-before-enforce test outcome as a
// self-contained page in the admin's popup. Parsed once at startup. html/
// template auto-escapes every interpolated field (Email, Domain, Reason) in its
// HTML text context, so an assertion carrying markup can't inject script — and
// renderSAMLTestResult ships a default-src 'none' CSP as the backstop.
var samlTestResultTmpl = template.Must(template.New("sso-test").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SSO test {{if .Pass}}passed{{else}}failed{{end}}</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         background: #f6f7f9; color: #1c2024; padding: 24px; }
  .card { width: 100%; max-width: 420px; background: #fff; border-radius: 16px;
          padding: 28px; box-shadow: 0 12px 40px -16px rgba(0,0,0,.25); }
  .badge { display: inline-flex; align-items: center; gap: 6px; font-size: 13px;
           font-weight: 600; padding: 4px 12px; border-radius: 999px; }
  .pass .badge { background: #e7f6ec; color: #1a7f4b; }
  .fail .badge { background: #fdecec; color: #c0392b; }
  h1 { font-size: 19px; margin: 16px 0 6px; }
  p.lede { margin: 0 0 18px; color: #5b6470; }
  dl { margin: 0; display: grid; grid-template-columns: auto 1fr; gap: 8px 16px; }
  dt { color: #8a929c; font-size: 13px; }
  dd { margin: 0; font-weight: 500; word-break: break-word; }
  .hint { margin: 20px 0 0; font-size: 13px; color: #8a929c; }
</style>
</head>
<body>
<main class="card {{if .Pass}}pass{{else}}fail{{end}}">
  <span class="badge">{{if .Pass}}&#10003; Passed{{else}}&#10007; Failed{{end}}</span>
  <h1>{{if .Pass}}SSO test passed{{else}}SSO test failed{{end}}</h1>
  {{if .Pass}}
  <p class="lede">A sign-in completed end-to-end through your identity provider.</p>
  {{else}}
  <p class="lede">{{.Reason}}</p>
  {{end}}
  {{if .Email}}
  <dl>
    <dt>Authenticated email</dt><dd>{{.Email}}</dd>
    <dt>Email verified</dt><dd>{{if .EmailVerified}}Yes{{else}}No{{end}}</dd>
    <dt>Domain match</dt>
    <dd>{{if .DomainMatch}}Yes &mdash; {{.Domain}} is verified for your organization{{else}}No{{if .Domain}} &mdash; {{.Domain}} is not a verified domain for your organization{{end}}{{end}}</dd>
  </dl>
  {{end}}
  <p class="hint">You can close this window and return to Settings.</p>
</main>
</body>
</html>`))

// renderSAMLTestResult writes the test outcome as a self-contained HTML page in
// the admin's popup, status 200 for both pass and fail (the HTTP request
// succeeded; pass/fail is content). The page loads no scripts and no external
// resources, so a tight CSP locks it down as defense-in-depth behind the
// template's auto-escaping of the IdP-derived fields.
//
// Header notes (mirrors github_app_register.go's self-contained pages):
//   - The per-response CSP OVERRIDES the global one withSecurityHeaders set, so
//     it must re-state frame-ancestors 'none' (clickjacking) itself — otherwise
//     the override would silently drop it. (X-Frame-Options: DENY from the global
//     middleware still applies; this is the standards-track twin.)
//   - Cache-Control: no-store because the page embeds IdP-derived PII (the
//     authenticated email) that must not be cached by the browser or any proxy.
//
// The global middleware still supplies X-Content-Type-Options / Referrer-Policy /
// Permissions-Policy (this handler runs inside that wrap), so they aren't re-set.
func (s *Server) renderSAMLTestResult(w http.ResponseWriter, res samlTestResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := samlTestResultTmpl.Execute(w, res); err != nil {
		// Status + headers are already committed, so we can't switch to a 500 —
		// just log; the popup may render partially.
		authLog.Error("saml test: render result failed", "error", err)
	}
}
