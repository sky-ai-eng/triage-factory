package sso

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/server"

	// Link the SSO store factories (postgres + sqlite) so the SSO bundle is
	// built into every TxStores / Stores once this package is in the binary.
	_ "github.com/sky-ai-eng/triage-factory/ee/sso/store/lite"
	_ "github.com/sky-ai-eng/triage-factory/ee/sso/store/pg"
)

// init registers the SSO route installer. It runs when package main
// blank-imports ee/sso; installExtensions() invokes install during routes()
// unconditionally — install's own handlers gate per-request on the `sso`
// entitlement at their own org-resolution seams (see login.go, connection.go,
// domains.go).
func init() {
	server.RegisterExtension("sso", install)
}

// install mounts the org-admin SSO surface (connection, break-glass,
// domains) on the core server through the extension API, with the same
// withSession/CSRF discipline and authorization as core's own routes. The
// login-path SSO routes (discover, verify-test, SAML start, callback
// enforcement/JIT) are wired separately through the login hooks, not here.
func install(api server.ExtensionAPI) {
	ssoc := &ssoConnectionHandler{
		tx:        api.Tx(),
		az:        api.Authz(),
		client:    ssoHTTPClient,
		gotrueURL: api.GotrueAdminBaseURL,
		publicURL: api.PublicURL,
	}
	api.Op(opConnectionGet, ssoc.handleSSOConnectionGet)
	api.Op(opConnectionCreate, ssoc.handleSSOConnectionCreate)
	api.Op(opConnectionUpdate, ssoc.handleSSOConnectionUpdate)
	api.Op(opEnforcementUpdate, ssoc.handleSSOEnforcementUpdate)

	ssobg := &ssoBreakGlassHandler{tx: api.Tx(), az: api.Authz(), conn: ssoc}
	api.Op(opBreakGlassList, ssobg.handleBreakGlassList)
	api.Op(opBreakGlassAdd, ssobg.handleBreakGlassAdd)
	api.Op(opBreakGlassRemove, ssobg.handleBreakGlassRemove)

	ssod := newSSODomainsHandler(api.Tx(), api.Authz())
	api.Op(opDomainList, ssod.handleDomainList)
	api.Op(opDomainClaim, ssod.handleDomainClaim)
	api.Op(opDomainVerify, ssod.handleDomainVerify)
	api.Op(opDomainDelete, ssod.handleDomainDelete)

	// Login-path SSO: the verify-before-enforce test-start (session-authed,
	// admin-gated) and the pre-auth identifier-first discovery probe, plus the
	// LoginExtension that handles SAML start + the callback's enforcement/JIT/
	// test forks inside core's shared OAuth login path.
	le := &loginExt{api: api, conn: ssoc}
	api.SetLoginExtension(le)
	api.API("GET /api/sso/connection/test", le.handleTestStart)
	api.RawOp(opDiscover, api.PreAuthRateLimit(http.HandlerFunc(le.handleDiscover)))
}
