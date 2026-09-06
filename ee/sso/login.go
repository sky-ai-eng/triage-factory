package sso

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	ssostore "github.com/sky-ai-eng/triage-factory/ee/sso/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// loginExt implements server.LoginExtension — the SSO forks of the shared
// OAuth login path (SP-initiated SAML start, the verify-before-enforce test
// round-trip, enforcement of a non-SSO login on an enforced domain, and JIT
// provisioning of an SSO login), reaching core's OAuth primitives through
// the ExtensionAPI rather than *Server.
type loginExt struct {
	api  server.ExtensionAPI
	conn *ssoConnectionHandler // adminGate + currentConnection for test-start
}

func (le *loginExt) stores() *ssostore.Bundle { return ssostore.FromStores(le.api.Stores()) }

// IdPForProviders answers core's read-side seam from the connection store:
// the admin-pool lookup, since the caller is a principal reading their own
// login identities and no org membership scopes that.
func (le *loginExt) IdPForProviders(ctx context.Context, providerIDs []string) (map[string]string, error) {
	return le.stores().Connections.IdPsByProviderIDs(ctx, providerIDs)
}

// StartSSO handles GET /api/auth/oauth/saml — the SP-initiated SAML start.
// Given a TF-registered provider_id it POSTs GoTrue's /sso and forwards the
// 303 to the IdP. A non-"saml" provider is declined (core reports it).
func (le *loginExt) StartSSO(w http.ResponseWriter, r *http.Request, provider, returnTo string) bool {
	if provider != "saml" {
		return false
	}
	if !le.api.AuthReady() {
		http.NotFound(w, r)
		return true
	}
	providerID := strings.TrimSpace(r.URL.Query().Get("provider_id"))
	if providerID == "" {
		http.Error(w, "missing provider_id", http.StatusBadRequest)
		return true
	}

	// Validate against TF's binding before initiating. Admin pool: the actor is
	// pre-login with no membership (provider_id is the lookup key). Unknown/
	// disabled → 404 (non-disclosure).
	binding, err := le.stores().Connections.GetByProviderID(r.Context(), providerID)
	if err != nil {
		ssoLog.Error("saml start: provider lookup failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	if binding == nil || !binding.Enabled {
		http.NotFound(w, r)
		return true
	}
	if !entitlements.For(binding.OrgID).Has(entitlements.FeatureSSO) {
		http.NotFound(w, r)
		return true
	}

	codeVerifier, csrf, ok := le.api.IssueOAuthStateCookie(w, r, returnTo, providerID, false)
	if !ok {
		return true
	}
	redirectTo := le.api.PublicURL() + "/api/auth/callback?state=" + csrf
	location, err := le.api.GotrueSSO(r.Context(), providerID, redirectTo, le.api.PKCEChallenge(codeVerifier))
	if err != nil {
		ssoLog.Warn("saml start: gotrue /sso failed", "error", err)
		http.Error(w, "sso start failed", http.StatusBadGateway)
		return true
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
	return true
}

// OnLoginResolved folds the two post-principal SSO forks: enforcement of a
// non-SSO login on an enforced domain, and JIT provisioning of an SSO login.
func (le *loginExt) OnLoginResolved(w http.ResponseWriter, r *http.Request, in server.LoginResolved) (uuid.NullUUID, bool, error) {
	// SSO enforcement: a non-SSO (GitHub) login whose VERIFIED email is on an
	// org's enforced verified domain is rejected — unless the principal is a
	// designated break-glass account. Only the non-SSO path is gated.
	if !in.IsSSO && in.EmailVerified {
		if dom, okDom := le.api.EmailDomain(in.Email); okDom {
			route, rerr := le.stores().Domains.GetVerifiedByDomain(r.Context(), dom)
			if rerr != nil {
				httpx.InternalError(w, "sso", rerr)
				return uuid.NullUUID{}, true, nil
			}
			if route != nil && route.Enabled && route.Enforced && entitlements.For(route.OrgID).Has(entitlements.FeatureSSO) {
				isBG, berr := le.stores().BreakGlass.IsBreakGlass(r.Context(), route.OrgID, in.UserID.String())
				if berr != nil {
					httpx.InternalError(w, "sso", berr)
					return uuid.NullUUID{}, true, nil
				}
				if !isBG {
					ssoLog.Info("sso enforcement: rejected non-sso login on enforced domain",
						"user", in.UserID, "org", route.OrgID, "domain", dom)
					// Durable auth record (best-effort): an enforced domain bounced
					// a non-SSO login. Recorded before the redirect; r is threaded
					// so the seam captures the source IP + user agent.
					le.api.RecordAuthEvent(r.Context(), r, ssoAuthEvent(
						domain.AuthEventSSOEnforcementRejected, in, route.OrgID, dom))
					q := url.Values{"error": {"sso_required"}}
					if in.State.ReturnTo != "" && in.State.ReturnTo != "/" {
						q.Set("return_to", in.State.ReturnTo)
					}
					http.Redirect(w, r, "/login?"+q.Encode(), http.StatusFound)
					return uuid.NullUUID{}, true, nil
				}
				// isBG == true: the break-glass account bypassed enforcement that
				// would OTHERWISE have applied (route enabled + enforced, on-domain
				// verified email). The highest-scrutiny SOC2 event — today this path
				// had no log line at all. Record it, then fall through to a normal
				// login.
				//
				// TIMING: this fires at the enforcement-bypass DECISION (here, before
				// the session is minted in handleOAuthCallback) rather than strictly
				// after the full login. That is deliberate — moving it post-session
				// would require threading break-glass state + its detail back through
				// the LoginExtension seam into core, leaking SSO semantics into the
				// core handler (the seam exists to keep core SSO-free). It captures
				// the authentication + bypass, which IS complete at this point, and a
				// break_glass_login is always PAIRED with a login_success (recorded
				// after CreateSystem succeeds): so a break_glass_login with no
				// matching login_success precisely flags "break-glass auth bypassed
				// SSO but the session never completed" — a detectable signal, not a
				// silent false positive.
				ssoLog.Info("sso enforcement: break-glass login bypassed enforced domain",
					"user", in.UserID, "org", route.OrgID, "domain", dom)
				le.api.RecordAuthEvent(r.Context(), r, ssoAuthEvent(
					domain.AuthEventBreakGlassLogin, in, route.OrgID, dom))
			}
		}
	}

	// JIT provisioning: the provider_id → org binding IS the cross-org isolation
	// boundary. Resolve + grant on the admin pool (the JIT actor has no
	// membership in the target org yet).
	if in.IsSSO {
		binding, lerr := le.stores().Connections.GetByProviderID(r.Context(), in.State.ProviderID)
		if lerr != nil {
			ssoLog.Error("jit: provider lookup failed", "user", in.UserID, "error", lerr)
			return uuid.NullUUID{}, false, lerr
		}
		if binding != nil && binding.Enabled {
			orgUUID, perr := uuid.Parse(binding.OrgID)
			if perr != nil {
				ssoLog.Error("jit: bad org id in binding", "org", binding.OrgID, "error", perr)
				return uuid.NullUUID{}, false, perr
			}
			if !entitlements.For(binding.OrgID).Has(entitlements.FeatureSSO) {
				return uuid.NullUUID{}, false, nil
			}
			// teamID NULL → "org member, zero teams". Idempotent (ON CONFLICT DO
			// NOTHING): a returning member's row already exists, so this grants but
			// never re-roles.
			if gerr := le.api.GrantOrgMembership(r.Context(), in.UserID, orgUUID, binding.DefaultRole); gerr != nil {
				ssoLog.Error("jit: grant membership failed", "user", in.UserID, "org", orgUUID, "error", gerr)
				return uuid.NullUUID{}, false, gerr
			}
			ssoLog.Info("jit provisioned sso login", "user", in.UserID, "org", orgUUID, "role", binding.DefaultRole)
			return uuid.NullUUID{UUID: orgUUID, Valid: true}, false, nil
		}
	}
	return uuid.NullUUID{}, false, nil
}

// ssoAuthEvent builds the auth_events row for an SSO enforcement decision —
// sso_enforcement_rejected (a non-SSO login bounced) or break_glass_login (a
// break-glass account bypassed enforcement). Both carry the principal, the org
// whose enforced domain matched (the top-level OrgID column), and {"domain":…}
// detail — the org is NOT duplicated into the detail. The request-source fields
// (ip_address / user_agent) are NOT set here — the RecordAuthEvent seam fills
// them from the threaded *http.Request via core's canonical clientIP parsing, so
// ee/sso doesn't duplicate that logic. ee/sso has no direct store access, so it
// records through the ExtensionAPI seam (server.recordAuthEvent), which owns the
// best-effort / log-on-failure contract.
func ssoAuthEvent(eventType string, in server.LoginResolved, orgID, dom string) domain.AuthEvent {
	return domain.AuthEvent{
		EventType:  eventType,
		UserID:     in.UserID.String(),
		OrgID:      orgID,
		DetailJSON: ssoAuthDetail(dom),
	}
}

// ssoAuthDetail marshals the {"domain":…} detail payload (org_id is the row's own
// column, not duplicated here). On the near-impossible marshal error it logs and
// returns "" — the event is still recorded, just without detail.
func ssoAuthDetail(dom string) string {
	b, err := json.Marshal(map[string]string{"domain": dom})
	if err != nil {
		ssoLog.Error("sso auth event detail marshal failed", "error", err)
		return ""
	}
	return string(b)
}

// handleDiscover is POST /api/sso/discover — identifier-first SSO discovery.
// Pre-auth, no side effect: resolves a typed work email to a SAML start URL
// or signals "use GitHub". Every non-match collapses to {sso:false}.
func (le *loginExt) handleDiscover(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, ssoDiscoverMaxBody)

	var body ssoDiscoverRequest
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}

	// Multi-mode only: local registers no IdP. Answer {sso:false} (→ GitHub).
	if runmode.Current() == runmode.ModeLocal {
		httpx.WriteJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	domainName, ok := le.api.EmailDomain(body.Email)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	// Admin pool (BYPASSRLS): pre-login, the verified domain is the authorization.
	route, err := le.stores().Domains.GetVerifiedByDomain(r.Context(), domainName)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if route == nil || !route.Enabled {
		httpx.WriteJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}
	if !entitlements.For(route.OrgID).Has(entitlements.FeatureSSO) {
		httpx.WriteJSON(w, http.StatusOK, ssoDiscoverResponse{SSO: false})
		return
	}

	httpx.WriteJSON(w, http.StatusOK, ssoDiscoverResponse{
		SSO:      true,
		StartURL: ssoStartURL(route.ProviderID, le.api.NormalizeReturnTo(body.ReturnTo)),
	})
}

const ssoDiscoverMaxBody = 4 << 10

type ssoDiscoverRequest struct {
	Email    string `json:"email"`
	ReturnTo string `json:"return_to"`
}

type ssoDiscoverResponse struct {
	SSO      bool   `json:"sso"`
	StartURL string `json:"start_url,omitempty"`
}

// ssoStartURL builds the SP-initiated SAML start URL the discovery response
// hands back (the same /api/auth/oauth/saml endpoint StartSSO serves).
func ssoStartURL(providerID, returnTo string) string {
	q := url.Values{}
	q.Set("provider_id", providerID)
	q.Set("return_to", returnTo)
	return "/api/auth/oauth/saml?" + q.Encode()
}

// handleTestStart is GET /api/sso/connection/test — the admin-gated
// verify-before-enforce test start. Round-trips a REAL sign-in through the
// org's own connection (allowing a not-yet-enabled connection) so the admin
// can confirm the IdP is wired up before enabling. The browser opens it in a
// popup; OnTestCallback renders the result and mints no session.
func (le *loginExt) handleTestStart(w http.ResponseWriter, r *http.Request) {
	if !le.api.AuthReady() {
		http.NotFound(w, r)
		return
	}
	orgID, userID, ok := le.conn.adminGate(w, r)
	if !ok {
		return
	}
	conn, err := le.conn.currentConnection(r.Context(), orgID, userID)
	if err != nil {
		httpx.InternalError(w, "sso", err)
		return
	}
	if conn == nil {
		http.NotFound(w, r)
		return
	}
	// No conn.Enabled check — a not-yet-enabled connection is exactly what this
	// verifies. The test path renders a result page, never a redirect, so pin
	// ReturnTo to "/".
	codeVerifier, csrf, ok := le.api.IssueOAuthStateCookie(w, r, "/", conn.ProviderID, true /* test */)
	if !ok {
		return
	}
	redirectTo := le.api.PublicURL() + "/api/auth/callback?state=" + csrf
	location, err := le.api.GotrueSSO(r.Context(), conn.ProviderID, redirectTo, le.api.PKCEChallenge(codeVerifier))
	if err != nil {
		ssoLog.Warn("saml test start: gotrue /sso failed", "org", orgID, "error", err)
		http.Error(w, "sso test start failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// OnTestCallback is the callback's verify-before-enforce test branch, reached
// only when TF's signed state carries Test. It completes the code exchange +
// JWT verify (proving the assertion is valid + readable), SHORT-CIRCUITS
// before any writes, and renders a pass/fail page.
func (le *loginExt) OnTestCallback(w http.ResponseWriter, r *http.Request, st server.LoginState) bool {
	if !st.Test {
		return false
	}
	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		le.renderSAMLTestResult(w, samlTestResult{
			Reason: samlTestErrorReason(errCode, r.URL.Query().Get("error_description")),
		})
		return true
	}

	authCode := r.URL.Query().Get("code")
	if authCode == "" {
		le.renderSAMLTestResult(w, samlTestResult{
			Reason: "The identity provider returned no authorization code, so the sign-in did not complete.",
		})
		return true
	}

	accessToken, _, err := le.api.GotrueExchange(r.Context(), authCode, st.CodeVerifier)
	if err != nil {
		ssoLog.Warn("saml test: code exchange failed", "error", err)
		le.renderSAMLTestResult(w, samlTestResult{
			Reason: "Could not exchange the sign-in code with the identity provider — the assertion was likely rejected. Check the signing certificate and that the test user is assigned to the app.",
		})
		return true
	}
	claims, err := le.api.VerifyAccessToken(accessToken)
	if err != nil {
		ssoLog.Warn("saml test: token verify failed", "error", err)
		le.renderSAMLTestResult(w, samlTestResult{
			Reason: "The identity provider completed sign-in, but the returned token failed verification.",
		})
		return true
	}

	res := samlTestResult{
		Email:         strings.TrimSpace(claims.Email),
		EmailVerified: claims.EmailVerified,
	}
	if res.Email == "" {
		res.Reason = "Sign-in completed, but the assertion carried no email claim. In your identity provider, map the user's email (or UPN) to the email claim."
		le.renderSAMLTestResult(w, res)
		return true
	}

	// Domain match (informational, never gates pass/fail), scoped to the
	// connection's org via the provider binding.
	if dom, ok := le.api.EmailDomain(res.Email); ok {
		res.Domain = dom
		binding, berr := le.stores().Connections.GetByProviderID(r.Context(), st.ProviderID)
		if berr != nil {
			ssoLog.Warn("saml test: provider lookup for domain-match failed", "error", berr)
		} else if binding != nil {
			route, derr := le.stores().Domains.GetVerifiedByDomain(r.Context(), dom)
			if derr != nil {
				ssoLog.Warn("saml test: verified-domain lookup failed", "error", derr)
			} else if route != nil && route.OrgID == binding.OrgID {
				res.DomainMatch = true
			}
		}
	}

	// Durably record the pass — best-effort.
	if err := le.stores().Connections.MarkTestedByProviderID(r.Context(), st.ProviderID); err != nil {
		ssoLog.Warn("saml test: mark tested failed", "provider_id", st.ProviderID, "error", err)
	}

	res.Pass = true
	le.renderSAMLTestResult(w, res)
	return true
}

type samlTestResult struct {
	Pass          bool
	Email         string
	EmailVerified bool
	Domain        string
	DomainMatch   bool
	Reason        string
}

func samlTestErrorReason(code, desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "The identity provider rejected the sign-in (" + code + ")."
	}
	return "The identity provider rejected the sign-in: " + desc
}

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

func (le *loginExt) renderSAMLTestResult(w http.ResponseWriter, res samlTestResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := samlTestResultTmpl.Execute(w, res); err != nil {
		ssoLog.Error("saml test: render result failed", "error", err)
	}
}
