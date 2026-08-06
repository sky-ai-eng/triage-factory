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
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
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
	// SignedWebhookRateLimit wraps a handler in the per-IP signed-webhook
	// token bucket (no-op in local mode) — a separate, much higher-
	// throughput tier than PreAuthRateLimit for a route that authenticates
	// every request itself via a cryptographic signature before any side
	// effect (e.g. the Slack Events API receiver). Use this instead of
	// PreAuthRateLimit for that route shape: the signature makes
	// PreAuthRateLimit's anti-recon rationale moot, and its 1 req/s budget
	// would throttle a legitimate high-volume sender into looking like a
	// failing endpoint to its own delivery system.
	SignedWebhookRateLimit(h http.Handler) http.Handler

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

	// PublishEvent publishes evt through the durable Ingestor (durable
	// outbox enqueue + bus fan-out) — the ONLY correct way for ee/ ingest to
	// emit an event into the pipeline. Never falls back to a bare bus
	// publish: if the ingestor isn't wired yet (nil), it logs at ERROR and
	// drops the event, matching ingest's own drop-loudly convention, rather
	// than silently skipping durability. Read-through to the live server;
	// nil until app wiring completes (internal/app/subsystems.go). Safe for
	// request-time use and inside OnReady hooks (which fire post-wiring) —
	// the install closure runs before the ingestor is wired, so an
	// extension must NOT call this from its install closure.
	//
	// Pass the request's ctx where there is one: the ingestor stamps its
	// trace context onto the durable row, so an inbound webhook's server
	// span becomes the producer the event's later routing links back to.
	PublishEvent(ctx context.Context, evt domain.Event)
	// Bus returns the in-process event bus, for subscribe-side consumers.
	// Read-through to the live server; nil until app wiring completes. Safe
	// for request-time use and inside OnReady hooks (which fire post-wiring)
	// — same install-closure caveat as PublishEvent.
	Bus() *eventbus.Bus

	// OnReady registers a hook to run once app wiring is complete AND this
	// process holds the background-brain lease — the seam for an
	// extension's long-lived background worker that must run on exactly
	// one pod (connection managers, adapters — e.g. the Slack liveness
	// consumer of run sentinels; TFAC-583 spec §3). The hook fires in its
	// own goroutine, with a context that cancels both at process shutdown
	// AND on lease demotion — a worker that loses the brain gets the same
	// ctx.Done() signal a process shutdown would give it, and is started
	// fresh (a new goroutine, a new ctx) on a later re-acquisition. By
	// hook time every read-through accessor on this API (Bus, PublishEvent,
	// ...) is wired and non-nil. The hook MAY block for as long as this
	// pod holds the brain (run the worker loop directly); respect
	// ctx.Done() for graceful stop — a worker needing drain time handles
	// it inside the hook before returning. Hooks are fired in registration
	// order but run concurrently; there is no join on either shutdown or
	// demotion. Panics at registration time if hook is nil.
	//
	// At TF_ROLE=all / local mode this pod always holds the lease, so
	// OnReady hooks fire exactly once, indistinguishable from today.
	// Ungated, every control pod would run every OnReady worker — the same
	// hazard as running every core brain subsystem on every pod, wearing
	// EE clothes (external writes like Slack posts duplicating ×M). A
	// worker that is genuinely safe to run on every pod (idempotent,
	// stateless, no external side effects) opts out via
	// OnReadyReplicaSafe instead — never by default.
	OnReady(hook func(ctx context.Context))

	// OnReadyReplicaSafe registers a hook that runs once app wiring is
	// complete, on EVERY control/all pod regardless of brain-lease state —
	// the escape hatch for a worker that is genuinely safe to run
	// replicated (idempotent, no external side effects that would
	// duplicate). Fires exactly once per process lifetime (not re-fired on
	// lease transitions, since it doesn't gate on them). As of TFAC-583
	// this has zero callers — every existing OnReady worker is brain-
	// gated by design; a worker opts into this only by explicit
	// declaration. Panics at registration time if hook is nil.
	OnReadyReplicaSafe(hook func(ctx context.Context))

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

// extensionInstaller is one registered extension: a name (diagnostics) and
// the install closure that mounts its routes.
type extensionInstaller struct {
	name    string
	install func(ExtensionAPI)
}

// registeredExtensions is the process-global list of extension installers,
// appended from ee package init()s (blank-imported by package main) before
// the server is constructed. Invoked once, in routes().
var registeredExtensions []extensionInstaller

// RegisterExtension registers an installer to be invoked during routes().
// Every registered extension always mounts; the extension itself gates its
// handlers per-request on entitlements.For(orgID) at its own resolution
// seams. Called from an ee package's init().
func RegisterExtension(name string, install func(ExtensionAPI)) {
	registeredExtensions = append(registeredExtensions, extensionInstaller{name: name, install: install})
}

// installExtensions runs every registered installer, passing each the live
// ExtensionAPI. Called at the end of routes(). Extensions always mount —
// licensing is enforced per-request inside the extension (org resolved at
// each seam), not by skipping the mount at boot. Nothing registered → no-op.
func (s *Server) installExtensions() {
	api := serverExtensionAPI{s}
	for _, ext := range registeredExtensions {
		ext.install(api)
	}
}

// StartExtensionWorkers fires every OnReadyReplicaSafe hook registered
// during installExtensions, each in its own goroutine with ctx. Called
// once by app.Run after wiring completes and before the HTTP listener
// starts — unconditionally, regardless of brain-lease state (that's the
// point of the replica-safe hatch). Idempotent: a second call is a no-op,
// so a caller that double-invokes Run can't double-start workers.
//
// Brain-gated OnReady hooks do NOT fire here — see
// StartBrainExtensionWorkers, called by the brain start/stop unit instead.
func (s *Server) StartExtensionWorkers(ctx context.Context) {
	s.workersStarted.Do(func() {
		for _, hook := range s.replicaSafeHooks {
			go hook(ctx)
		}
	})
}

// StartBrainExtensionWorkers fires every brain-gated OnReady hook, each in
// its own goroutine with ctx. Called by the App's brain start/stop unit
// (internal/app) every time this pod acquires the background-brain
// lease — including a re-acquisition after an earlier demotion — with a
// fresh ctx each time. Unlike StartExtensionWorkers this is NOT
// Once-guarded: the caller (internal/app's brainMu-guarded startBrain) is
// the sole synchronization point, so calling this twice concurrently is a
// caller bug, not a case this method defends against — matching the "no
// join on shutdown" convention the hooks themselves already follow. ctx
// cancellation (on demotion or process shutdown) is what stops the
// started hooks; there's nothing to explicitly "stop" here.
func (s *Server) StartBrainExtensionWorkers(ctx context.Context) {
	for _, hook := range s.readyHooks {
		go hook(ctx)
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
func (a serverExtensionAPI) SignedWebhookRateLimit(h http.Handler) http.Handler {
	return a.s.signedWebhookRateLimit(h)
}
func (a serverExtensionAPI) Tx() db.TxRunner            { return a.s.tx }
func (a serverExtensionAPI) Authz() *authz.Checker      { return a.s.az }
func (a serverExtensionAPI) Stores() db.Stores          { return a.s.allStores }
func (a serverExtensionAPI) DB() *sql.DB                { return a.s.db }
func (a serverExtensionAPI) GotrueAdminBaseURL() string { return a.s.gotrueAdminBaseURL() }
func (a serverExtensionAPI) Bus() *eventbus.Bus         { return a.s.bus }

// PublishEvent delegates to the wired Ingestor. Deliberately does NOT fall
// back to a.s.bus.Publish when unwired — that would silently skip the
// durable outbox, exactly the loss internal/ingest exists to prevent (see
// ingest.go's own drop-loudly convention on an enqueue failure).
func (a serverExtensionAPI) PublishEvent(ctx context.Context, evt domain.Event) {
	if a.s.ingestor == nil {
		serverLog.ErrorContext(ctx, "ExtensionAPI.PublishEvent: ingestor not wired, dropping event", "event_type", evt.EventType)
		return
	}
	a.s.ingestor.Publish(ctx, evt)
}

// OnReady collects the hook onto the server's readyHooks slice. Called
// during routes() install, single-threaded, same no-lock startup-write
// contract as registeredExtensions — StartExtensionWorkers is the only
// reader, and it never runs concurrently with install. Panics at boot on a
// nil hook — same fail-fast-at-registration convention as the other core
// registries (routing.RegisterSource, events.Register) — rather than
// deferring the failure to a nil-func panic inside a goroutine when
// StartExtensionWorkers fires it later.
func (a serverExtensionAPI) OnReady(hook func(ctx context.Context)) {
	if hook == nil {
		panic("ExtensionAPI.OnReady: hook must not be nil")
	}
	a.s.readyHooks = append(a.s.readyHooks, hook)
}

func (a serverExtensionAPI) OnReadyReplicaSafe(hook func(ctx context.Context)) {
	if hook == nil {
		panic("ExtensionAPI.OnReadyReplicaSafe: hook must not be nil")
	}
	a.s.replicaSafeHooks = append(a.s.replicaSafeHooks, hook)
}

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
// access_change_log can't diverge from the grant. This audit contract is
// deliberately NOT best-effort (cf. RecordAuthEvent, which logs-and-continues):
// an audit-write failure rolls the whole tx back, so the membership is NOT
// granted, and the error propagates up through OnLoginResolved BEFORE the session
// is minted — the callback fails CLOSED (a 500, no session), so the user isn't
// logged in rather than landing half-provisioned. They retry once the DB is
// healthy, and the net-new gate makes that retry converge to exactly one grant +
// one audit row. A broken access_change_log write path therefore blocks JIT
// provisioning by design — there is no break-glass that grants while auditing is
// down (restoring it is a code change). The net-new gate keeps JIT —
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
