package server

import (
	"database/sql"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
)

// ExtensionAPI is the stable surface a registered server extension (the
// Enterprise Edition, ee/) uses to mount routes and reach the server
// capabilities it needs — without importing the server's unexported
// internals, and without core importing the extension. Core hands one to
// each installer during routes().
//
// The route registrars mirror the server's own api / apiMutating wrap
// discipline (withSession, and for mutating routes the CSRF same-origin
// check), so an extension-mounted route is indistinguishable from a core
// route — same auth, same CSRF, same identity context. The accessors are
// read-through to the live server, so values populated after routes()
// (deployCfg, authCfg) resolve lazily at request time.
type ExtensionAPI interface {
	// API mounts a read (GET/websocket) /api route through withSession.
	API(pattern string, h http.HandlerFunc)
	// APIMutating mounts a state-changing /api route through the CSRF
	// same-origin check + withSession.
	APIMutating(pattern string, h http.HandlerFunc)
	// Raw mounts a handler with no session/CSRF wrap — for pre-auth routes
	// that run before any session exists (their handler is responsible for
	// its own authorization).
	Raw(pattern string, h http.Handler)
	// PreAuthRateLimit wraps a handler in the per-IP pre-auth token bucket
	// (no-op in local mode), for anonymous-reachable routes.
	PreAuthRateLimit(h http.Handler) http.Handler

	// Tx is the transaction runner (claims-set tx; RLS).
	Tx() db.TxRunner
	// Authz is the org/team authorization layer (Require* gates, role probes).
	Authz() *authz.Checker
	// Stores is the non-tx aggregate, for admin-pool reads. Extensions read
	// their own store bundle off it via their typed accessor.
	Stores() db.Stores
	// DB is the primary handle (admin pool in multi mode).
	DB() *sql.DB
	// PublicURL is the deployment's external base URL ("" until deployCfg lands).
	PublicURL() string
	// GotrueAdminBaseURL is the in-network GoTrue base URL for admin calls.
	GotrueAdminBaseURL() string
}

// extensionInstaller is one registered extension: a name (diagnostics), a
// gating feature, and the install closure that mounts its routes.
type extensionInstaller struct {
	name    string
	feature entitlements.Feature
	install func(ExtensionAPI)
}

// registeredExtensions is the process-global list of extension installers,
// appended from ee package init()s (blank-imported by package main) before
// the server is constructed. Invoked once, in routes().
var registeredExtensions []extensionInstaller

// RegisterExtension registers an installer to be invoked during routes()
// iff its feature is licensed (entitlements.Active().Has(feature)). A zero
// feature means "always install". Called from an ee package's init().
func RegisterExtension(name string, feature entitlements.Feature, install func(ExtensionAPI)) {
	registeredExtensions = append(registeredExtensions, extensionInstaller{name: name, feature: feature, install: install})
}

// installExtensions runs every registered installer whose feature is
// licensed, passing each the live ExtensionAPI. Called at the end of
// routes(). Entitlements have already been resolved at startup
// (ee.Install runs before app.New), so the gate reflects the active
// license. Nothing registered / nothing licensed → no-op.
func (s *Server) installExtensions() {
	api := serverExtensionAPI{s}
	for _, ext := range registeredExtensions {
		if ext.feature != "" && !entitlements.Active().Has(ext.feature) {
			continue
		}
		ext.install(api)
	}
}

// serverExtensionAPI adapts *Server to ExtensionAPI. A thin value wrapper
// so the methods read through to the live server.
type serverExtensionAPI struct{ s *Server }

func (a serverExtensionAPI) API(pattern string, h http.HandlerFunc) { a.s.api(pattern, h) }
func (a serverExtensionAPI) APIMutating(pattern string, h http.HandlerFunc) {
	a.s.apiMutating(pattern, h)
}
func (a serverExtensionAPI) Raw(pattern string, h http.Handler) { a.s.mux.Handle(pattern, h) }
func (a serverExtensionAPI) PreAuthRateLimit(h http.Handler) http.Handler {
	return a.s.preAuthRateLimit(h)
}
func (a serverExtensionAPI) Tx() db.TxRunner            { return a.s.tx }
func (a serverExtensionAPI) Authz() *authz.Checker      { return a.s.az }
func (a serverExtensionAPI) Stores() db.Stores          { return a.s.allStores }
func (a serverExtensionAPI) DB() *sql.DB                { return a.s.db }
func (a serverExtensionAPI) GotrueAdminBaseURL() string { return a.s.gotrueAdminBaseURL() }

func (a serverExtensionAPI) PublicURL() string {
	if a.s.deployCfg == nil {
		return ""
	}
	return a.s.deployCfg.publicURL
}
