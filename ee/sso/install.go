package sso

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server"

	// Link the SSO store factories (postgres + sqlite) so the SSO bundle is
	// built into every TxStores / Stores once this package is in the binary.
	_ "github.com/sky-ai-eng/triage-factory/ee/sso/store/lite"
	_ "github.com/sky-ai-eng/triage-factory/ee/sso/store/pg"
)

// init registers the SSO route installer, gated on the `sso` entitlement.
// It runs when package main blank-imports ee/sso; installExtensions() then
// invokes install during routes() iff the deployment is licensed for SSO.
func init() {
	server.RegisterExtension("sso", entitlements.FeatureSSO, install)
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
	api.API("GET /api/sso/connection", ssoc.handleSSOConnectionGet)
	api.APIMutating("POST /api/sso/connection", ssoc.handleSSOConnectionCreate)
	api.APIMutating("PATCH /api/sso/connection", ssoc.handleSSOConnectionUpdate)
	api.APIMutating("PATCH /api/sso/connection/enforcement", ssoc.handleSSOEnforcementUpdate)

	ssobg := &ssoBreakGlassHandler{tx: api.Tx(), az: api.Authz(), conn: ssoc}
	api.API("GET /api/sso/break-glass", ssobg.handleBreakGlassList)
	api.APIMutating("POST /api/sso/break-glass", ssobg.handleBreakGlassAdd)
	api.APIMutating("DELETE /api/sso/break-glass/{user_id}", ssobg.handleBreakGlassRemove)

	ssod := newSSODomainsHandler(api.Tx(), api.Authz())
	api.API("GET /api/sso/domains", ssod.handleDomainList)
	api.APIMutating("POST /api/sso/domains", ssod.handleDomainClaim)
	api.APIMutating("POST /api/sso/domains/{id}/verify", ssod.handleDomainVerify)
	api.APIMutating("DELETE /api/sso/domains/{id}", ssod.handleDomainDelete)

	// Login-path SSO: the verify-before-enforce test-start (session-authed,
	// admin-gated) and the pre-auth identifier-first discovery probe, plus the
	// LoginExtension that handles SAML start + the callback's enforcement/JIT/
	// test forks inside core's shared OAuth login path.
	le := &loginExt{api: api, conn: ssoc}
	api.SetLoginExtension(le)
	api.API("GET /api/sso/connection/test", le.handleTestStart)
	api.Raw("POST /api/sso/discover", api.PreAuthRateLimit(http.HandlerFunc(le.handleDiscover)))
}
