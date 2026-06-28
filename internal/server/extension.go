package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	// its own authorization). NOTE: TestRoutesCoverage only parses routes() in
	// server.go, so routes mounted here are invisible to it — use Raw ONLY for
	// pre-auth / self-authorizing handlers; a session-gated route must go
	// through API/APIMutating so the session+CSRF wrap (and the coverage guard)
	// apply.
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

	// --- login seam (see login_ext.go) ---

	// SetLoginExtension installs this server's SSO decision hook for the
	// shared OAuth login path. Called by the extension during install.
	SetLoginExtension(LoginExtension)
	// AuthReady reports whether the multi-mode auth stack is wired (false in
	// local mode / an unwired deploy); SSO routes 404 when it isn't.
	AuthReady() bool
	// IssueOAuthStateCookie mints the HMAC state cookie (CSRF + PKCE verifier,
	// plus the optional SSO provider_id and the verify-before-enforce test
	// flag) and returns the verifier + CSRF token. Writes a 500 and ok=false
	// on a crypto/marshal failure.
	IssueOAuthStateCookie(w http.ResponseWriter, r *http.Request, returnTo, providerID string, test bool) (codeVerifier, csrf string, ok bool)
	// GotrueSSO POSTs GoTrue's public /sso and returns the IdP redirect Location.
	GotrueSSO(ctx context.Context, providerID, redirectTo, codeChallenge string) (location string, err error)
	// GotrueExchange completes the PKCE auth-code exchange, returning the tokens.
	GotrueExchange(ctx context.Context, authCode, codeVerifier string) (accessToken, refreshToken string, err error)
	// VerifyAccessToken verifies a GoTrue access token and returns its claims.
	VerifyAccessToken(accessToken string) (*verify.Claims, error)
	// PKCEChallenge derives the S256 code_challenge from a PKCE verifier.
	PKCEChallenge(verifier string) string
	// GrantOrgMembership grants (idempotently, admin pool) an org membership —
	// the JIT-provisioning primitive for an SSO login. A net-new grant is audited
	// in access_change_log atomically (a returning member's no-op grant is not);
	// the caller (ee/sso) needs no awareness of the audit. See TFAC-486.
	GrantOrgMembership(ctx context.Context, userID, orgID uuid.UUID, role string) error
	// EmailDomain extracts the lower-cased domain from an email address.
	EmailDomain(email string) (domain string, ok bool)
	// NormalizeReturnTo applies the open-redirect guard to a return_to value
	// (relative-path-only), so an extension building a redirect URL can't be
	// coaxed into an off-site bounce.
	NormalizeReturnTo(raw string) string
	// RecordAuthEvent writes one auth_events row best-effort (the SOC2
	// authentication audit log of record) — the seam ee/sso records its
	// sso_enforcement_rejected + break_glass_login events through, having no
	// direct store access. The request is threaded so the server fills the
	// source fields from core's canonical parsing (ip_address via clientIP,
	// user_agent) — EE never duplicates that logic; a value already set on e
	// wins, and a nil r leaves them unset. Delegates to the server's
	// recordAuthEvent: on a store failure it logs at ERROR and the auth flow
	// proceeds, never rolled back.
	RecordAuthEvent(ctx context.Context, r *http.Request, e domain.AuthEvent)
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

// errAuthNotWired backs the auth-dependent ExtensionAPI methods when the
// multi-mode auth stack isn't wired (local mode / pre-SetAuthDeps).
// Extensions gate on AuthReady() first; this is a defensive backstop.
var errAuthNotWired = errors.New("auth not wired")

func (a serverExtensionAPI) SetLoginExtension(le LoginExtension) { a.s.loginExt = le }
func (a serverExtensionAPI) AuthReady() bool                     { return a.s.authDeps != nil }

func (a serverExtensionAPI) IssueOAuthStateCookie(w http.ResponseWriter, r *http.Request, returnTo, providerID string, test bool) (string, string, bool) {
	return a.s.issueOAuthStateCookie(w, r, returnTo, providerID, test)
}

func (a serverExtensionAPI) GotrueSSO(ctx context.Context, providerID, redirectTo, codeChallenge string) (string, error) {
	if a.s.authDeps == nil {
		return "", errAuthNotWired
	}
	return a.s.authDeps.gotrueSSO(ctx, providerID, redirectTo, codeChallenge)
}

func (a serverExtensionAPI) GotrueExchange(ctx context.Context, authCode, codeVerifier string) (string, string, error) {
	if a.s.authDeps == nil {
		return "", "", errAuthNotWired
	}
	at, rt, _, err := a.s.authDeps.gotrueExchange(ctx, authCode, codeVerifier)
	return at, rt, err
}

func (a serverExtensionAPI) VerifyAccessToken(accessToken string) (*verify.Claims, error) {
	if a.s.authDeps == nil {
		return nil, errAuthNotWired
	}
	return a.s.authDeps.verifier.Verify(accessToken)
}

func (a serverExtensionAPI) PKCEChallenge(verifier string) string { return pkceChallenge(verifier) }

// GrantOrgMembership grants the SSO JIT org membership and audits the net-new
// grant atomically on the admin pool. The JIT actor has no claims/membership at
// provisioning time, so this can't route through the app-pool AccessChangeLog
// store; it mirrors invite-accept's admin-pool tx instead — grant the membership,
// and if it was NET-NEW (not an ON CONFLICT DO NOTHING no-op on a returning
// member), record the org_member_granted audit row on the SAME tx so the
// access_change_log can't diverge from the grant. The net-new gate keeps JIT —
// which fires on every SSO login — from logging "joined" each time. The actor is
// the user themselves (a self-grant via the org's SSO domain binding), matching
// invite-accept's "joined the org"; team_id is NULL (JIT grants org-only); the
// source in detail_json lets the viewer render "joined via SSO". See TFAC-486.
func (a serverExtensionAPI) GrantOrgMembership(ctx context.Context, userID, orgID uuid.UUID, role string) error {
	tx, err := a.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin jit grant tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	netNew, err := grantOrgMembership(ctx, tx, userID, orgID, role, uuid.NullUUID{}, "")
	if err != nil {
		return err
	}
	if netNew {
		if err := recordAccessChangeTx(ctx, tx, orgID.String(), domain.AccessChange{
			ActorUserID:  userID.String(),
			Action:       domain.AccessActionOrgMemberGranted,
			TargetUserID: userID.String(),
			DetailJSON:   accessDetailSSOJIT(role),
		}); err != nil {
			return fmt.Errorf("audit jit member granted: %w", err)
		}
	}
	return tx.Commit()
}

func (a serverExtensionAPI) EmailDomain(email string) (string, bool) { return emailDomain(email) }
func (a serverExtensionAPI) NormalizeReturnTo(raw string) string     { return normalizeReturnTo(raw) }

func (a serverExtensionAPI) RecordAuthEvent(ctx context.Context, r *http.Request, e domain.AuthEvent) {
	// Enrich with the request's source fields (IP via canonical clientIP
	// parsing, user agent) so EE auth events carry them without ee/ duplicating
	// clientIP — the same filler the core write-sites use via authEventBase.
	fillRequestSource(&e, r)
	a.s.recordAuthEvent(ctx, e)
}
