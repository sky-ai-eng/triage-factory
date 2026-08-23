package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Server is the main HTTP server for Triage Factory.
type Server struct {
	db               *sql.DB
	prompts          db.PromptStore
	swipes           db.SwipeStore
	agents           db.AgentStore           // resolves the org's agent for claim stamps
	teamAgents       db.TeamAgentStore       // re-checks team_agents.enabled on task-delegate / factory-delegate
	users            db.UsersStore           // display_name + Jira binding on the user row; host-scoped GitHub identity via user_github_identities
	blueprints       db.BlueprintStore       // used by event-handler + project test fixtures
	tasks            db.TaskStore            // task lifecycle, claim, queue + factory snapshot reads
	conversations    db.ConversationStore    // conversation lifecycle + transcript
	repos            db.RepositoryStore      // repositories CRUD for repos/settings handlers
	events           db.EventStore           // events audit log Record/Latest for stock carry-over + factory drag-to-delegate
	taskMemory       db.TaskMemoryStore      // conversation_memory writes (human verdict capture on review/PR submit, task-disposition cleanup)
	secrets          db.SecretStore          // canonical credential read/write path — local-mode keychain, multi-mode vault
	teams            db.TeamsStore           // resolves the request org's default team for handlers that synthesize team-scoped rows (tasks, prompts)
	orgs             db.OrgsStore            // per-org settings (GitHub/Jira base URLs, poll intervals, clone protocol) post-internal/config deletion
	jiraRules        db.JiraStatusRulesStore // per-team Jira status rules (replaces the deleted config.Jira.Projects view)
	githubApps       db.GitHubAppsStore      // per-org GitHub App registrations (manifest flow)
	reachableRepos   db.ReachableReposStore  // the reachable-repo mirror the picker lists from and the team-repos write gate validates against
	githubDeliveries db.GitHubDeliveryStore  // applied webhook deliveries, so a redelivery is dropped before the mirror write and the bus publish
	authEvents       db.AuthEventStore       // TFAC-76: SOC2 authentication audit log of record — written best-effort via recordAuthEvent at the auth write-sites
	// tx runs handler-cleanup write batches under the request user's
	// claims even when the cleanup needs to outlive the request
	// context. Each cleanup wraps in `s.tx.WithTx(cleanupCtx, orgID,
	// userID, fn)` so multi-mode RLS sees the user's identity. Local
	// mode SQLite ignores userID.
	tx db.TxRunner
	// az is the org/team authorization layer — ResolveTeamID, the
	// Require* gates, and the membership/role probes. It bundles db +
	// tx so a handler holds one dependency for these cross-cutting
	// checks instead of re-deriving them against raw fields. See the
	// authz package.
	az *authz.Checker
	// allStores is the full bundle, retained so post-commit bootstrap
	// helpers (db.BootstrapNewOrg / db.BootstrapNewTeam) — which take a
	// db.Stores and must run outside WithTx on the admin pool — can be
	// invoked from handlers without re-threading every individual store.
	allStores db.Stores
	mux       *http.ServeMux
	static    fs.FS
	ws        *websocket.Hub
	// wsBackplane is the optional multi-mode cross-pod kick publisher
	// (TFAC-584), wired via SetWSBackplane. Nil in local mode and before
	// wiring; every call site nil-checks it, so a session revoke on a
	// single-pod deployment behaves exactly as before this existed — only
	// the local s.ws.CloseUserConnections closes anything.
	wsBackplane WSKicker
	spawner     *delegate.Spawner
	// reconciler backs the Tier-2 conversation-scoped artifact refresh endpoint
	// (TFAC-464) — the same Reconciler the background Tier-1 Manager runs.
	// Wired via SetReconciler after construction; nil until then, so the
	// endpoint 503s rather than panicking if a request races startup.
	reconciler *reconcile.Reconciler
	// bedrockRole resolves the Bedrock role-mode setup + connect probe's live
	// AWS calls (TFAC-616). Wired via SetBedrockRoleResolver after
	// construction; nil in local mode and until then, so the role-setup
	// endpoint reports "role mode requires the control service" rather than
	// panicking.
	bedrockRole bedrockRoleResolver
	// placement backs the GET /api/fleet/placement explainer (TFAC-587): the
	// computed capacity-weighted rendezvous candidate order for a key. Wired
	// via SetPlacementResolver after construction; nil until then so the
	// endpoint 503s rather than panicking if a request races startup.
	placement placementResolver
	// modelProber spends one minimal request establishing whether an org's
	// credentials can invoke a given model. Wired via SetModelProber after
	// construction; nil in local mode, where nothing probes and the test
	// routes say the deployment cannot answer.
	modelProber modelProber
	// fleetQueue backs the GET /api/fleet/queue view: per-org conversation-queue shares
	// (active/queued + cap). Satisfied by the ConversationQueue store; a narrow
	// interface so the handler test can inject canned shares.
	fleetQueue fleetQueueReader
	// ghResolver picks the right GitHub credential (org App installation
	// token → PAT) per request, given the org + target account. The per-repo
	// handler operations migrated off the old process-global PAT client —
	// review diff/submit, pending-PR submit, branches, dashboard — resolve
	// through it, and there is no longer a process-global PAT client. (A few
	// handlers still build a request-scoped
	// PAT client directly where they intentionally need the PAT identity — the
	// repo picker's PAT fallback and GitHub-teams discovery.) Built in New from
	// the stores, so it's never nil — handlers don't need a guard.
	ghResolver ghclient.Resolver
	// ghTokenCache is the per-installation App-token cache backing ghResolver.
	// Retained on the Server (not just handed to the resolver) so the paths
	// that learn a token has died — installation.deleted, installation.suspend,
	// an App cutover or teardown — can Invalidate it, the same reason
	// jiraTokenCache is held here. Built in New, never nil.
	ghTokenCache ghclient.TokenCache
	// jiraResolver routes Jira writes by provenance: ForSystem for
	// the org/bot service cred, ForUser for the acting user's own stored
	// credential. User-initiated board claim / undo / requeue resolve via
	// ForUser so the write is attributed to the user, not the service account.
	// Built in New from the stores, so it's never nil — handlers don't guard.
	jiraResolver jira.Resolver
	// jiraApps owns the org_jira_apps table — per-org Atlassian OAuth app
	// registrations (the BYO-app override / local-supplied app). The settings
	// handlers read/write it; the resolver reads it (system door) to resolve
	// the app the per-user Connect flow runs against.
	jiraApps db.JiraAppsStore
	// jiraOAuthApps resolves the Atlassian OAuth app for an org (per-org
	// override → deployment first-party in hosted; local-supplied BYO else
	// not-configured). Backs the Jira app settings card's status + the
	// connect_available signal that the per-user Jira status endpoint returns.
	// Built in New, so it's never nil — handlers don't guard.
	jiraOAuthApps jira.OAuthAppResolver
	// jiraOAuthMinter performs the stateless Atlassian OAuth HTTP exchanges for
	// the per-user Connect flow (authorize-code exchange, accessible-resources).
	// The per-request refresh + rotation write-back lives in the token cache
	// wired into jiraResolver; this minter is the connect handlers' direct seam.
	// Built in New, never nil.
	jiraOAuthMinter *jiraoauth.Minter
	// jiraTokenCache is the per-user Cloud OAuth access-token cache backing
	// jiraResolver's OAuth branch. Retained on the Server (not just handed to
	// the resolver) so the connect callback + a paste-over-OAuth re-bind can
	// Invalidate the stale cached token — mirrors how ghTokenCache is reachable
	// via onInstallationTokensInvalid. Built in New, never nil.
	jiraTokenCache *jiraoauth.TokenCache
	// Change callbacks accept the orgID of the tenant whose integration
	// creds just rotated, so the closure can re-resolve via SecretStore.
	// Local mode always passes runmode.LocalDefaultOrgID; multi-mode
	// handlers thread the request's orgID through so the callback
	// can't fire one org's poller restart with another org's PAT.
	onGitHubChanged func(orgID string) // GitHub creds/access changed — evict the reachable-repo cache + re-due the org's GitHub poll (profiling is driven by the system:poll "profiler" subscriber, not here)
	onJiraChanged   func(orgID string) // Jira config changed — restart Jira poller only
	// onSourcesChanged fires after an org admin pauses or resumes one event
	// source. It carries the kind because both reactions are per-source: the
	// router's disabled-source gate is invalidated for this org (in process
	// when this pod runs the brain, over the tf_ctl relay otherwise), and a
	// resumed polled source is re-dued so it starts again on the next wake
	// rather than after a full interval. Nil until SetOnSourcesChanged runs.
	onSourcesChanged func(orgID, kind string)
	scorerTrigger    func(orgID string) // invoked after non-poll task creation (e.g. carry-over) to kick the per-org scorer immediately
	// profilerTrigger kicks the per-org repo-profiling manager. force=true
	// bypasses the 3-day TTL — the explicit "Re-profile" button and a
	// repo-set change both want an immediate re-profile rather than waiting
	// out a poll interval. Nil until SetProfilerTrigger runs.
	profilerTrigger func(orgID string, force bool)
	// reachTrigger kicks the per-org reachable-repo refresh manager.
	// force=true bypasses its TTL — the picker's refresh control and every
	// credential change. Nil until SetReachTrigger runs.
	reachTrigger func(orgID string, force bool)
	// dashboardBackfill seeds a bound user's trailing-window PR history into the
	// entity store so the personal dashboard isn't blank for history that
	// predates tracking (TFAC-396). Multi-mode only — wired to the poller's
	// BackfillUserDashboard; nil in local mode, where per-cycle Phase 1b owns
	// history. kickDashboardBackfill runs it fire-and-forget, marker-guarded.
	dashboardBackfill func(ctx context.Context, orgID, userID, login, host string) error

	// bus is the in-process event bus. The GitHub webhook receiver
	// publishes verified deliveries here; nil until SetEventBus runs
	// (the receiver no-ops the publish if so). Content-event processing
	// is a downstream subscriber's concern.
	bus *eventbus.Bus
	// ingestor is the durable emit seam (events audit row + queue row,
	// then bus fan-out). nil until SetIngestor runs (wired in
	// internal/app/subsystems.go immediately after ingest.New). Backs
	// ExtensionAPI.PublishEvent — ee/ ingest must publish through here,
	// never through the raw bus, or events lose the durable outbox.
	ingestor *ingest.Ingestor

	// onInstallationTokensInvalid fires when an installation's already-minted
	// tokens stop being usable — a verified installation.deleted or
	// installation.suspend webhook, and the App-credential change paths — so
	// the credential resolver's per-installation token cache can drop the
	// now-dead entry. New wires it to ghTokenCache.Invalidate, so it is set on
	// every Server; SetInstallationTokensInvalidHook replaces it (tests observe
	// the firings that way), and callers still nil-guard because that setter
	// accepts a nil.
	onInstallationTokensInvalid func(orgID, installationID string)

	// deployCfg holds deployment-identity config (publicURL, HMAC key,
	// secureCookies) populated in both local and multi mode.
	deployCfg *deployConfig

	// authDeps groups the multi-mode-only auth stack (JWKS verifier +
	// session store + gotrue HTTP client). Nil in local mode; checked
	// by middleware before any session lookup so local-mode boots
	// without dragging GoTrue into the dependency graph.
	authDeps  *authDeps
	authCfg   *authConfig
	authProxy http.Handler // /auth/v1/* → gotrue:9999/*

	// loginExt is the SSO decision seam in the shared OAuth login path
	// (see login_ext.go). nil until an extension calls SetLoginExtension
	// during installExtensions; loginExtension() returns a no-op default
	// when unset (SSO off). The Enterprise Edition registers the real one.
	loginExt LoginExtension

	// refreshGroup dedupes concurrent JWT refresh attempts per session.
	// singleflight.Group is the standard "share-the-call-result-across-
	// concurrent-callers" primitive: at most one gotrue refresh runs
	// per session ID at a time, and all waiters receive the same
	// result. The key is cleared once the in-flight call returns, so
	// there's no per-session state accumulating over process lifetime
	// (vs the prior sync.Map[uuid]*Mutex which leaked one entry per
	// session forever).
	refreshGroup singleflight.Group

	// inlineScriptHashes is the base64-encoded SHA-256 of each inline
	// <script> block in the served index.html. Populated by SetStatic;
	// the CSP middleware (withSecurityHeaders) injects them into
	// script-src as `'sha256-<hash>'` directives.
	inlineScriptHashes []string

	// githubAppRegMu serializes per-org GitHub App registration so
	// two concurrent callbacks can't both pass the existence check,
	// both call GitHub's conversion endpoint, and leave an orphan
	// App. TFAC-579: this map is now only the LOCAL-mode half of
	// acquireKeyedLock — sufficient at N=1 (there's no second process to
	// race), but not across control pods in multi mode, where
	// acquireKeyedLock instead takes a Postgres session-scoped advisory
	// lock. Both call sites go through acquireKeyedLock; nothing reaches
	// into this map directly anymore.
	githubAppRegMu sync.Map // map[orgID]*sync.Mutex

	// webhookSecretMu guards the three fields below — the short-TTL per-org
	// cache of the secret the pre-auth GitHub webhook receiver verifies
	// deliveries against. Resolving it costs a settings read, a registration
	// read, and a vault read, all of them spent before the signature is
	// checked, so an unauthenticated flood would otherwise pay them per
	// request. Entries (positives and negatives alike) expire after
	// webhookSecretTTL and are dropped explicitly when the App lifecycle
	// rotates or tears down the secret. See github_webhook_secret.go.
	webhookSecretMu    sync.Mutex
	webhookSecretCache map[string]webhookSecretEntry // key: orgID
	webhookSecretSweep time.Time                     // last expiry sweep, for the once-per-TTL gate
	// webhookSecretGen counts invalidations. Resolutions run outside the
	// lock, so it is what stops one that started before a rotation from
	// writing the pre-rotation secret back afterwards.
	webhookSecretGen uint64

	// webhookHealthMu guards the per-org cache of the App-webhook probe — is
	// GitHub actually delivering this org's webhooks here, and is the receiver
	// accepting them. Unlike the secret cache above, this one is not a
	// throughput measure: it is what keeps two GitHub round trips off the
	// Settings status read, and what preserves the last known answer when a
	// probe fails. Keys are org ids from membership-checked reads, so the key
	// set is the deployment's own orgs rather than anything a caller chooses.
	// See github_webhook_health.go.
	webhookHealthMu sync.Mutex
	webhookHealth   map[string]*webhookHealthEntry // key: orgID
	// hookProbe substitutes the GitHub half of that probe. Nil in production
	// (the real App-JWT reads run); set by tests, which have no GitHub to ask
	// and need to drive the failure path deterministically.
	hookProbe func(ctx context.Context, orgID string, app *domain.OrgGitHubApp, expectedURL string) (githubapp.WebhookHealth, error)

	// preAuthLimiter is the per-IP token-bucket cap on the pre-auth
	// allowlist routes (TFAC-433). Those mounts run before any session
	// exists, so they lack the implicit "needs a valid session" bound the
	// s.api/apiMutating routes get for free; this blunts a single-source
	// flood (e.g. SSO-discovery domain enumeration). Constructed in New so
	// it's never nil; the wrapper no-ops in local mode. See ratelimit.go.
	preAuthLimiter *ipRateLimiter
	// signedWebhookLimiter is the per-IP token-bucket cap for pre-auth
	// mounts that authenticate themselves via a cryptographic signature
	// before any side effect (the Slack Events API receiver) — a separate,
	// much higher-throughput tier than preAuthLimiter's human-login budget,
	// since a legitimate sender at this tier already proves its
	// authenticity and a shared low budget would 429 it into looking like a
	// failing endpoint. Constructed in New so it's never nil; the wrapper
	// no-ops in local mode. See ratelimit.go.
	signedWebhookLimiter *ipRateLimiter

	// readyHooks are brain-gated OnReady hooks registered by extensions
	// during installExtensions (routes()). Collected single-threaded at
	// startup — same no-lock contract as registeredExtensions — then fired
	// by StartBrainExtensionWorkers every time this pod's brain starts
	// (TFAC-583; at TF_ROLE=all / local, that's once, at boot — same as
	// before this ticket).
	readyHooks []func(context.Context)
	// replicaSafeHooks are OnReadyReplicaSafe hooks — the escape hatch for
	// a worker that's safe to run on every pod regardless of brain-lease
	// state. Fired once, unconditionally, by StartExtensionWorkers.
	replicaSafeHooks []func(context.Context)
	// workersStarted guards StartExtensionWorkers so a double call (e.g. a
	// test double-invoking Run) can't fire replicaSafeHooks twice.
	workersStarted sync.Once

	// version is main.Version (the release tag, or "dev" for an unreleased
	// build), threaded through for GET /readyz (TFAC-573) so an operator
	// can confirm a deploy landed without shelling into the host. Set via
	// SetVersion; the zero value "" renders as an empty string rather than
	// panicking for any Server built without it (bare test constructions).
	version string
	// migrationsOK records that db.Migrate completed successfully at boot,
	// for GET /readyz's "migrations" hard check. Always set true in
	// production — db.Migrate runs to completion in internal/app/stores.go
	// before server.New / ListenAndServeContext, so there is no live-
	// serving path where migrations failed. Kept as an explicit flag
	// (default false) rather than hard-coding "ok" in the handler so the
	// check has real meaning if a future boot path (e.g. async migrations)
	// stops gating this as tightly, and so a Server built without the
	// SetMigrationsOK call (bare test constructions) fails the check
	// loudly instead of lying "ok". See SetMigrationsOK.
	migrationsOK bool
	// pollerHealth is the poller Manager's Health() method, wired via
	// SetPollerManager, backing /readyz's poller-alive hard check + per-org
	// poll-staleness soft signal. Narrower than handing over the whole
	// *poller.Manager — the handler only needs the read. Nil until wired
	// (bare test constructions); the handler reports both poller checks
	// failed rather than nil-dereffing.
	pollerHealth func(ctx context.Context) poller.HealthSnapshot
	// leaseStatus reports this pod's current view of the background-brain
	// lease, wired via SetLeaseStatus — role=control in multi mode only
	// (TFAC-583). nil at role=all / local (the single process always
	// self-holds, so /readyz's poller hard-check stays byte-identical to
	// the pre-TFAC-583 contract and omits the `lease` field entirely) and
	// nil on an executor (no /readyz there — it serves a separate
	// localhost healthz).
	leaseStatus LeaseStatusFunc
}

// agentEnabledForTeam returns the resolved agent and whether the
// team_agents.enabled flag is true for it *on the given team*. This is
// the acceptance rule "delegating re-checks
// team_agents.enabled at delegate time," now correctly scoped to the
// acting team rather than always the org default — so a
// multi-team user delegating for team B is gated on B's bot setting.
//
// Three outcomes the caller maps:
//   - (a, true, nil)  — proceed with the delegate.
//   - (a, false, nil) — bot disabled for this team; refuse with 409.
//   - (_, _, err)     — store error; refuse with 500.
//
// Nil agent (no bootstrap) returns err so the caller surfaces a
// distinguishable 500 message rather than a misleading "disabled" 409.
// An empty teamID (teamless org — a bootstrap bug) resolves to
// disabled, never to a guessed team.
func (s *Server) agentEnabledForTeam(ctx context.Context, orgID, userID, teamID string) (*domain.Agent, bool, error) {
	if s.agents == nil {
		return nil, false, fmt.Errorf("agent store not configured")
	}
	var (
		a       *domain.Agent
		enabled bool
		// teamMissing distinguishes "no agent bootstrapped"
		// (fatal err) from "team_agents row missing → treat as
		// disabled" inside the closure where we can't return the
		// three-tuple directly.
		teamMissing bool
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		a, e = tx.Agents.GetForOrg(ctx, orgID)
		if e != nil {
			return fmt.Errorf("agent lookup: %w", e)
		}
		if a == nil {
			return fmt.Errorf("no agent bootstrapped — set up the bot first")
		}
		if s.teamAgents == nil {
			// Pre-D-Claims wiring (tests). Treat as enabled to preserve
			// the pre-flag behavior for any test path that hasn't wired
			// teamAgents yet.
			enabled = true
			return nil
		}
		if teamID == "" {
			// No team supplied (teamless org). Production installs always
			// have a team (multi-mode org provisioning; local-mode
			// baseline migration), so this is a bootstrap bug —
			// surface as disabled rather than minting a wrong-team row.
			teamMissing = true
			return nil
		}
		ta, e := tx.TeamAgents.GetForTeam(ctx, orgID, teamID, a.ID)
		if e != nil {
			return fmt.Errorf("team_agents lookup: %w", e)
		}
		if ta == nil {
			// team_agents row missing — treat as disabled. Provisioned
			// installs always have the row (BootstrapLocalOrg /
			// team-create bootstrap); a missing row at runtime means
			// either the tenant was never provisioned or something went
			// sideways.
			teamMissing = true
			return nil
		}
		enabled = ta.Enabled
		return nil
	}); err != nil {
		return a, false, err
	}
	if teamMissing {
		return a, false, nil
	}
	return a, enabled, nil
}

// New creates a new server with the given database + the full
// per-resource store bundle, and registers all routes. The Server retains
// individual store fields rather than a single db.Stores struct so existing
// handler code keeps working — the constructor just unpacks the bundle once
// instead of forcing every caller to enumerate 20+ stores positionally.
//
// raw *sql.DB stays available for handlers that haven't been
// ported to a store yet.
func New(database *sql.DB, stores db.Stores) *Server {
	s := &Server{
		db:               database,
		prompts:          stores.Prompts,
		swipes:           stores.Swipes,
		agents:           stores.Agents,
		teamAgents:       stores.TeamAgents,
		users:            stores.Users,
		blueprints:       stores.Blueprints,
		tasks:            stores.Tasks,
		conversations:    stores.Conversations,
		repos:            stores.Repos,
		events:           stores.Events,
		taskMemory:       stores.TaskMemory,
		secrets:          stores.Secrets,
		teams:            stores.Teams,
		orgs:             stores.Orgs,
		jiraRules:        stores.JiraStatusRules,
		githubApps:       stores.GitHubApps,
		reachableRepos:   stores.ReachableRepos,
		githubDeliveries: stores.GitHubDeliveries,
		jiraApps:         stores.JiraApps,
		authEvents:       stores.AuthEvents,
		tx:               stores.Tx,
		az:               authz.New(database, stores.Tx),
		fleetQueue:       stores.ConversationQueue,
		allStores:        stores,
		mux:              http.NewServeMux(),
		ws:               websocket.NewHub(),
	}
	// Per-IP rate limiters for the pre-auth allowlist and the signed-webhook
	// tier. Built here (not injected) so a Server is always usable without
	// external wiring; both wrappers that consult them no-op in local mode.
	s.preAuthLimiter = newIPRateLimiter(preAuthRatePerSec, preAuthBurst, rateLimitBucketTTL)
	s.signedWebhookLimiter = newIPRateLimiter(signedWebhookRatePerSec, signedWebhookBurst, rateLimitBucketTTL)
	// GitHub credential resolver + its installation-token cache, built from
	// the same stores. Constructed here (not injected) so a Server is always
	// usable without external wiring — tests that call New directly get a
	// working resolver too. A verified installation.deleted or
	// installation.suspend webhook drops the dead token from the cache via
	// onInstallationTokensInvalid.
	s.ghTokenCache = ghclient.NewMemoryTokenCache()
	s.ghResolver = ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, s.ghTokenCache)
	// Atlassian OAuth app resolver (TFAC-337): per-org override → deployment
	// first-party (hosted) / local-supplied (local). The first-party app is
	// read from the deployment env, and only in hosted mode — local has no
	// first-party default, so FirstPartyOAuthAppFromEnv returns the zero app
	// there and the resolver relies on the org's BYO row.
	s.jiraOAuthApps = jira.NewOAuthAppResolver(stores.JiraApps, stores.Secrets, jira.FirstPartyOAuthAppFromEnv())
	// Cloud OAuth minter + per-user access-token cache. The cache reads the
	// stored refresh token, mints an access token, and writes the rotated
	// refresh token back (PutUserSystem) — the structural difference from the
	// static PAT/API-token creds. It's the OAuth source the resolver delegates
	// its AuthMethodCloudOAuth branch to.
	s.jiraOAuthMinter = jiraoauth.NewMinter()
	s.jiraTokenCache = jiraoauth.NewTokenCache(s.jiraOAuthMinter, s.jiraOAuthApps, stores.Secrets)
	// Jira write-actor resolver: ForSystem (org/bot cred) +
	// ForUser (acting user's cred, incl. the Cloud OAuth mint path).
	// Constructed here like ghResolver so a Server is always usable without
	// external wiring.
	s.jiraResolver = jira.NewResolverWithOAuth(stores.Secrets, stores.Orgs, s.jiraTokenCache)
	s.onInstallationTokensInvalid = func(orgID, installationID string) {
		s.ghTokenCache.Invalidate(orgID, installationID)
	}
	s.routes()
	return s
}

// Handler returns the server's fully-wrapped HTTP handler — the same one
// ListenAndServeContext binds — so callers can mount the app under a parent
// mux, wrap it with additional middleware, or drive it over httptest without
// opening a socket. The mux is populated by routes() during New, so the
// handler is ready as soon as New returns.
func (s *Server) Handler() http.Handler { return s.tracedHandler() }

// ListenAndServe starts the HTTP server with no shutdown signal wired.
// Kept for callers (and tests) that don't need graceful shutdown; it
// delegates to ListenAndServeContext with a never-cancelled context.
func (s *Server) ListenAndServe(addr string) error {
	return s.ListenAndServeContext(context.Background(), addr)
}

// ListenAndServeContext starts the HTTP server on the given address and
// shuts it down gracefully when ctx is cancelled (SIGINT/SIGTERM at the
// process boundary). The mux is wrapped in withSecurityHeaders so every
// response carries the standard set (HSTS conditionally, CSP,
// X-Frame-Options, etc.), and in otelhttp outside that — see
// tracedHandler. Returns nil on a clean ctx-driven shutdown; a
// non-ErrServerClosed listen error otherwise.
func (s *Server) ListenAndServeContext(ctx context.Context, addr string) error {
	httpSrv := &http.Server{Addr: addr, Handler: s.tracedHandler()}

	// The watcher shuts the server down on ctx cancel, but must also exit if
	// the server stops on its own first (e.g. a bind failure) — otherwise a
	// caller passing context.Background(), like ListenAndServe, would leak it
	// forever on a Done channel that never closes. serverExited signals that
	// second path; shutdownDone lets us join the watcher before returning so
	// none outlives this call.
	serverExited := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		case <-serverExited:
			// ListenAndServe already returned; nothing to shut down.
		}
	}()

	err := httpSrv.ListenAndServe()
	close(serverExited)
	<-shutdownDone

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// api mounts a read-only /api/* route through withSession so identity
// context (sentinel claims+orgID in local mode, JWT claims + active org
// in multi mode) is seeded before the handler runs. Use for routes that
// do not mutate server state — GETs and the websocket handshake.
//
// Pair with apiMutating for state-changing routes, which additionally
// adds the same-origin (CSRF) defense the cookie session needs.
func (s *Server) api(pattern string, h http.HandlerFunc) {
	s.mux.Handle(pattern, s.withSession(h))
}

// apiMutating mounts a state-changing /api/* route through both the
// CSRF same-origin check (outer) and withSession (inner). Use for
// POST/PUT/PATCH/DELETE — anything that changes server-side state when
// authenticated by the sid cookie.
//
// The wrap order is CSRF → session → handler: reject obviously-cross-
// origin browser POSTs before doing the more expensive session lookup,
// and ensure handlers always see seeded identity context regardless of
// CSRF outcome (CSRF rejections short-circuit before the handler runs
// anyway, so the inner session middleware isn't reached for those).
func (s *Server) apiMutating(pattern string, h http.HandlerFunc) {
	s.mux.Handle(pattern, s.withCSRFOriginCheck(s.withSession(h)))
}

func (s *Server) routes() {
	// Pre-auth allowlist — these /api/* (and /auth/v1/*, /) routes
	// intentionally DO NOT go through s.api / s.apiMutating because
	// they must run before any session exists, or have no identity
	// dependency at all. Any addition here must be deliberate; the
	// routes_coverage_test guards against accidental wrap-stripping.
	//
	//   GET  /api/auth/oauth/{provider} — initiates the OAuth dance
	//        before a session is created.
	//   GET  /api/auth/callback         — completes OAuth and creates
	//        the session; can't gate on the session it's about to mint.
	//   POST /api/auth/logout           — reads sid cookie directly so
	//        logout still works on a stale/invalid session. CSRF only.
	//   GET  /api/config                — AuthGate reads deployment_mode
	//        at boot to pick the login flow; must answer before any
	//        session exists. The handler returns only deployment_mode;
	//        per-user identity lives on /api/me.
	//   GET  /api/health                — platform liveness probe (Fly
	//        checks, compose healthcheck, k8s liveness). Pre-auth so
	//        the probe doesn't need a session; deliberately doesn't
	//        consult the DB (see handleHealth).
	//   GET  /readyz                    — readiness probe (TFAC-573): DB +
	//        migrations + poller liveness (hard checks, 503 on failure)
	//        plus poll-staleness + active-claim count (soft signals). Bare
	//        path (not /api/readyz) by convention — the universal k8s/ALB
	//        probe target — so it's outside /api/* and the preAuthAllowlist
	//        below / routes_coverage_test entirely; noted here only for
	//        consistency with the other pre-auth routes. See
	//        readyz_handler.go.
	//   POST /api/webhooks/github/{org_id} — GitHub App webhook receiver;
	//        GitHub has no session, and the handler verifies the HMAC
	//        signature against the org's stored webhook secret itself.
	//   GET  /api/invites/preview          — invite-token preview; the
	//        recipient hasn't authenticated yet, so this can't gate on a
	//        session. Runs on the admin pool; the token is the bearer secret.
	//   POST /api/sso/discover             — identifier-first login lookup;
	//        the visitor is anonymous (pre-login), so it can't gate on a
	//        session. No side effect, admin-pool read, privacy-safe reply
	//        (registered with the SSO routes below; see TFAC-427).
	//   /auth/v1/                        — GoTrue reverse proxy; auth
	//        happens upstream, not in our middleware.
	//   /api/                            — JSON 404 for any unmatched
	//        /api/* path (TFAC-409 item 5); no identity dependency.
	//   /                                — SPA fallback; static-file
	//        serving with no identity dependency.
	//
	// IP rate limiting (TFAC-433): the interactive pre-auth mounts —
	// oauth start, logout, invite preview, and SSO discovery (registered
	// below) — are wrapped in s.preAuthRateLimit so an anonymous caller
	// can't flood them (e.g. discovery domain enumeration). The cap is
	// declared here at the allowlist, not sprinkled across handlers; it
	// no-ops in local mode. Deliberately NOT wrapped: the OAuth callback
	// (already bounded per-flow by its HMAC-signed PKCE state cookie +
	// single-use IdP code, so an IP cap adds ~no protection — and a 429
	// mid-OAuth on a shared NAT would break a top-level navigation for no
	// gain), /api/health (platform liveness probes hit it often),
	// /api/config (the AuthGate boot read), the /auth/v1/ GoTrue proxy
	// (rate-limited upstream), and the SPA fallback (every static asset).
	//
	// The GitHub webhook receiver takes the separate signed-webhook tier
	// rather than this one, alongside the Slack receiver: it authenticates
	// each delivery by HMAC, so the human-login budget would throttle a
	// legitimate sender, but "GitHub-paced" describes only the legitimate
	// traffic — an anonymous caller can hit it as fast as it likes, and each
	// attempt costs reads before the signature is checked.
	//
	// Wrap ORDER matters where a cheap rejection gate also applies: the
	// limiter goes INSIDE such a gate so a request the gate rejects costs
	// no tokens. Logout is the only case — withCSRFOriginCheck (outer) runs
	// first, so a cross-origin POST 403s before the limiter is consulted.
	// Otherwise a malicious page could CSRF-spam logout from a victim's
	// browser and drain that IP's shared pre-auth budget (starving its
	// login/discovery) with requests that never pass CSRF anyway. The other
	// wrapped routes have no such gate, so the limiter is outermost there.
	s.mux.Handle("GET /api/auth/oauth/{provider}", s.preAuthRateLimit(http.HandlerFunc(s.handleOAuthStart)))
	s.mux.HandleFunc("GET /api/auth/callback", s.handleOAuthCallback)
	s.mux.Handle("POST /api/auth/logout", s.withCSRFOriginCheck(s.preAuthRateLimit(http.HandlerFunc(s.handleLogout))))
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	// Liveness probe — pre-auth so platform healthchecks (Fly checks,
	// compose healthcheck, k8s liveness) can hit it without a session.
	// Plain 200 OK with a tiny JSON body. Don't expand this into a
	// readiness probe (which would couple to DB + integrations) — the
	// platforms use auto-restart on liveness failure and we don't want
	// a flapping integration to recycle the whole process.
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	// Readiness probe (TFAC-573) — bare path (not under /api/*), matching
	// the universal k8s/ALB probe convention; Go 1.22's ServeMux matches
	// this exact literal ahead of the "/" SPA catch-all regardless of
	// registration order. Pre-auth for the same reason as /api/health
	// above. See readyz_handler.go.
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	// /auth/v1/* reverse-proxy to gotrue, wired lazily inside
	// SetAuthDeps. The closure here re-reads s.authProxy each
	// request so local-mode (where it stays nil) returns 404
	// rather than panicking, and multi-mode picks up the proxy
	// once SetAuthDeps completes.
	s.mux.Handle("/auth/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authProxy == nil {
			http.NotFound(w, r)
			return
		}
		s.authProxy.ServeHTTP(w, r)
	}))

	// Integration credentials (GitHub PAT, Jira PAT). Distinct from the
	// session-auth routes above — these are per-user-stored credentials
	// for talking to third-party services on the user's behalf, not the
	// user's own login. Lived under /api/auth/* historically; renamed in
	// a cleanup so /api/auth/* unambiguously means
	// "session authentication." Wrapped via s.api/apiMutating since you
	// need to be logged in to manage your integration credentials.
	s.api("GET /api/integrations/status", s.handleIntegrationsStatus)
	// Placement explainer (TFAC-587): the computed rendezvous candidate order
	// for a key. Org-admin gated inside the handler on ?org= (the fleet
	// operator identity is TFAC-589). Read-only.
	s.api("GET /api/fleet/placement", s.handleFleetPlacement)
	// Fleet queue view: per-org active/queued shares + the org's concurrency
	// cap. Org-admin gated inside the handler on ?org= (the fleet-wide
	// operator view is a later ticket, same interim as the placement explainer
	// above). Read-only.
	s.api("GET /api/fleet/queue", s.handleFleetQueue)
	// Logout-everywhere: must be authenticated to use it (you can only
	// nuke your own sessions).
	s.apiMutating("POST /api/auth/logout/all", s.handleLogoutAll)
	// /api/me is the session-protected identity endpoint. In local mode
	// the shim in withSession injects sentinel claims so the handler
	// sees a non-nil claims value; in multi mode the handler 401s when
	// no claims are seeded.
	s.api("GET /api/me", s.handleMe)
	// Linked login identities for the signed-in principal — the read-only
	// "Login methods" view. Personal read keyed on the session principal, no
	// org scoping. Both modes (local returns a single synthetic row).
	s.api("GET /api/me/identities", s.handleMeIdentities)
	// Switch the session's active org.
	s.apiMutating("POST /api/me/active-org", s.handleActiveOrgUpdate)
	// The caller's own settings row. Viewer-relative, so the subject comes
	// from the session and there is no id to address.
	s.api("GET /api/me/settings", s.handleMeSettingsGet)
	s.apiMutating("PATCH /api/me/settings", s.handleMeSettingsPatch)
	// Create the workspace — the "Start your Factory" onboarding CTA in
	// both modes. Multi mints a net-new org (403 when org creation is
	// disabled on the instance); local provisions its single synthetic
	// tenant, idempotently.
	s.apiMutating("POST /api/orgs", s.handleOrgCreate)
	// The canonical single read for an org: the same {id, name, role} row
	// /api/me carries per membership. Member-visible; a non-member 404s.
	s.api("GET /api/orgs/{org_id}", s.handleOrgGet)
	// multi-team selectors. GET /api/teams is the data source
	// for the per-page read filter + write-time picker (count-gated to
	// ≥2 teams in the frontend); it carries the last-acting-team the write
	// picker seeds from, maintained server-side on each write (no
	// explicit-set endpoint — it's a recency signal, not a user preference).
	// POST /api/teams is the org-admin "add team" affordance (hosted-only;
	// 404 in local).
	th := &teamsHandler{
		tx:        s.tx,
		az:        s.az,
		allStores: s.allStores,
		spawner:   func() *delegate.Spawner { return s.spawner },
	}
	s.apiMutating("POST /api/teams/list", th.handleTeamsList)
	s.apiMutating("POST /api/teams", th.handleTeamCreate)
	// The canonical single read, and the only reader of a team's description.
	// Any org member may read any team in their org; a cross-org id 404s under
	// RLS. {team_id} takes the literal "default" in local mode (authz.ResolveTeamID).
	s.api("GET /api/teams/{team_id}", th.handleTeamGet)
	// PATCH /api/teams/{team_id} renames a team / edits its description
	// (hosted-only; 404 in local). Gated team-admin-or-org-admin; a plain
	// member 403s, a cross-org team_id 404s (VerifyTeamInOrg).
	s.apiMutating("PATCH /api/teams/{team_id}", th.handleTeamUpdate)
	// Team archive/restore lifecycle (TFAC-448), org-admin only, multi-mode.
	// Archive soft-deletes + force-stops the team's in-flight delegations
	// and blocks further writes; restore flips it back (dead conversations
	// stay dead). The preview + archived-list back the confirm modal and the
	// org-admin restore surface.
	// The team's settings row and its Jira project rules, under the team
	// resource so the path segment the caller asserts is the one the
	// authorization check is about. The rules are their own replace-set write
	// rather than a key inside the settings body — a child collection, like the
	// tracked repos and the github-group mappings, not a field.
	s.api("GET /api/teams/{team_id}/settings", s.handleTeamSettingsGet)
	s.apiMutating("PATCH /api/teams/{team_id}/settings", s.handleTeamSettingsPatch)
	s.apiMutating("PUT /api/teams/{team_id}/jira-projects", s.handleTeamJiraProjectsPut)
	// The other two child collections, siblings of jira-projects: the team's
	// tracked GitHub repos and its GitHub-team mappings, both replace-set PUTs.
	// github-repos, not repos — it reads as the sibling it is and does not
	// collide with the top-level /api/repos registry resource.
	s.api("GET /api/teams/{team_id}/github-repos", s.handleTeamReposGet)
	s.apiMutating("PUT /api/teams/{team_id}/github-repos", s.handleTeamReposPut)
	s.api("GET /api/teams/{team_id}/github-groups", s.handleTeamGitHubGroupsGet)
	s.apiMutating("PUT /api/teams/{team_id}/github-groups", s.handleTeamGitHubGroupsPut)
	s.apiMutating("POST /api/teams/archived/list", th.handleTeamArchivedList)
	s.api("GET /api/teams/{team_id}/archive/preview", th.handleTeamArchivePreview)
	s.apiMutating("POST /api/teams/{team_id}/archive", th.handleTeamArchive)
	s.apiMutating("POST /api/teams/{team_id}/restore", th.handleTeamRestore)

	// Team roster: list members + add + change role + remove/leave. The list
	// read is the one roster on the surface — the management roster, the
	// assignee picker and the predicate editor all read it — and answers in
	// both modes, where {team_id} takes the "default" alias at N=1. The
	// MUTATORS are multi-mode only (each 404s in local, which has nobody to
	// enrol). List is any org member; POST/PATCH/DELETE gate
	// team-admin-or-org-admin (DELETE also allows a self-leave).
	// VerifyTeamInOrg 404s a cross-org team_id; the last-admin guard is a DB
	// trigger surfaced as a 409.
	tmh := &teamMembersHandler{tx: s.tx, az: s.az}
	s.apiMutating("POST /api/teams/{team_id}/members/list", tmh.handleTeamRosterList)
	s.apiMutating("POST /api/teams/{team_id}/members", tmh.handleTeamMemberAdd)
	s.apiMutating("PATCH /api/teams/{team_id}/members/{user_id}", tmh.handleTeamMemberRoleChange)
	s.apiMutating("DELETE /api/teams/{team_id}/members/{user_id}", tmh.handleTeamMemberRemove)

	// Org People roster: list members + change role + remove.
	// Multi-mode only (each handler 404s in local). GET is any-member;
	// PATCH/DELETE gate org-admin (DELETE also allows a self-leave). The
	// last-owner guard is a DB trigger surfaced as a 409.
	// revokeUserOrgSessions is read lazily because authDeps lands after
	// routes() runs (SetAuthDeps); the closure nil-checks it for the boot
	// race and parses the string ids the handler carries into the UUIDs the
	// store expects. See TFAC-487.
	omh := &orgMembersHandler{tx: s.tx, az: s.az, ws: s.ws,
		revokeUserOrgSessions: func(ctx context.Context, userID, orgID string) (int64, error) {
			if s.authDeps == nil {
				return 0, errors.New("auth not wired")
			}
			uid, err := uuid.Parse(userID)
			if err != nil {
				return 0, fmt.Errorf("parse user id: %w", err)
			}
			oid, err := uuid.Parse(orgID)
			if err != nil {
				return 0, fmt.Errorf("parse org id: %w", err)
			}
			return s.authDeps.sessions.RevokeForUserInOrgSystem(ctx, uid, oid)
		},
		publishKick: func(ctx context.Context, userID, sid, orgID string, code int, reason string) {
			if s.wsBackplane != nil {
				s.wsBackplane.PublishKick(ctx, userID, sid, orgID, code, reason)
			}
		},
	}
	s.apiMutating("POST /api/orgs/{org_id}/members/list", omh.handleOrgMembersList)
	s.apiMutating("PATCH /api/orgs/{org_id}/members/{user_id}", omh.handleOrgMemberRoleChange)
	s.apiMutating("DELETE /api/orgs/{org_id}/members/{user_id}", omh.handleOrgMemberRemove)
	// Ownership transfer: owner-only (gated on tf.user_owns_org + the
	// guard_org_owner_transfer trigger). Promotes the target, repoints the
	// owner sentinel, demotes the former owner — all in one tx as the owner.
	s.apiMutating("POST /api/orgs/{org_id}/transfer-ownership", omh.handleOrgOwnershipTransfer)

	// Usage (spend layer) — the core Usage page's read API, each route under
	// the scope it reports on: usage is a fact about a caller, a team, or an
	// org, the same way a roster is. /api/me/usage is viewer-relative and takes
	// its org from the session; the team and org routes name their scope in the
	// path, and the org routes authorize against THAT org rather than whichever
	// one the session points at.
	// Scope is role-gated: /me is any org member, /teams/{id} is team-admin OR
	// org-admin, /orgs/{id} is org-admin. The team/org reads use the admin-pool
	// ListSpendSystem (the role gate is the authorization for crossing RLS).
	uh := &usageHandler{tx: s.tx, az: s.az, conversationQueue: s.allStores.ConversationQueue}
	s.api("GET /api/me/usage", uh.handleUsageMe)
	s.api("GET /api/teams/{team_id}/usage", uh.handleUsageTeam)
	s.api("GET /api/orgs/{org_id}/usage", uh.handleUsageOrg)
	// The team's flow node — events/tasks/runs cuts over a window, the
	// spend node's sibling with the member-level gate (the team page's
	// figures are every member's; spend stays admin-gated on /usage).
	s.api("GET /api/teams/{team_id}/activity", s.handleTeamActivity)
	// Org-scoped operations subset (TFAC-589): an org admin's own queue waits +
	// run durations + failure rates. Org-admin gated, SaaS-safe (no cross-tenant
	// machine truth) — the org-facing complement to the operator-only fleet console.
	s.api("GET /api/orgs/{org_id}/usage/ops", uh.handleUsageOrgOps)
	// Activity feed (EE, FeatureGovernance): the team/org Actions (external-action
	// audit log) + Objects (artifact history) lenses — same scope gates as the
	// spend reads above, plus the entitlement (unlicensed → 404).
	// Two resources, two routes each. The single ?view=-multiplexed /activity
	// route they replaced answered with two different row shapes and two
	// different filter vocabularies from one address.
	s.apiMutating("POST /api/teams/{team_id}/usage/artifacts/list", uh.handleUsageTeamArtifacts)
	s.apiMutating("POST /api/teams/{team_id}/usage/actions/list", uh.handleUsageTeamActions)
	s.apiMutating("POST /api/orgs/{org_id}/usage/artifacts/list", uh.handleUsageOrgArtifacts)
	s.apiMutating("POST /api/orgs/{org_id}/usage/actions/list", uh.handleUsageOrgActions)
	// Per-team daily spend cap (TFAC-482) — org-admin-set, EE/governance-gated
	// (the handlers 404 when unlicensed). The GET lists every active team + its cap
	// for the editor (so an idle team can be pre-capped); the PUT writes one cap and
	// is mutating, so it runs through the CSRF + session wrap. Both write/read the
	// admin pool: an org admin may cap a team they don't belong to.
	s.apiMutating("POST /api/orgs/{org_id}/usage/team-caps/list", uh.handleUsageTeamCaps)
	s.apiMutating("PUT /api/teams/{team_id}/usage/cap", uh.handleUsageTeamCap)
	// EE governance audit surface (TFAC-484): the access & credential change-log
	// viewer. Org-admin-gated AND FeatureGovernance-gated (404 unlicensed) inside
	// the handler — the data is core, only the cross-team lens is Enterprise.
	s.apiMutating("POST /api/orgs/{org_id}/usage/access-log/list", uh.handleUsageAccessLog)

	// Avatar proxy (TFAC-480): serves a user's OAuth-captured avatar first-party
	// so it renders under the app's tight `img-src 'self'` CSP instead of being
	// blocked as a cross-origin image. Any authenticated org member; the target
	// user_id is resolved under the caller's claims, so RLS scopes it to the
	// caller + co-org-members (a cross-org id 404s). One process-lifetime fetcher
	// owns the upstream fetch + cache. Consumed by Usage's by-user roster and the
	// topbar UserMenu.
	avh := &avatarsHandler{tx: s.tx, fetcher: newAvatarFetcher()}
	s.api("GET /api/avatars/{user_id}", avh.handleAvatar)

	// Org invites (multi-mode only — each handler 404s in local).
	// The admin-facing create/list/revoke gate on org-admin and write
	// through the app pool; preview + accept are the redeem surfaces and run
	// on the admin pool (the redeem actor holds a token but no membership).
	// publicURL + setActiveOrg are read lazily because deployCfg/authDeps
	// land after routes() runs. GET /api/invites/preview is intentionally
	// pre-auth (the recipient hasn't authenticated yet) — see the
	// preAuthAllowlist note below + routes_coverage_test.
	ih := &invitesHandler{
		tx:      s.tx,
		az:      s.az,
		admin:   s.db,
		invites: s.allStores.Invites,
		publicURL: func() string {
			if s.deployCfg == nil {
				return ""
			}
			return s.deployCfg.publicURL
		},
		setActiveOrg: func(ctx context.Context, sessID, orgID uuid.UUID) error {
			if s.authDeps == nil {
				return errors.New("auth not wired")
			}
			return s.authDeps.sessions.UpdateActiveOrgSystem(ctx, sessID, orgID)
		},
	}
	s.apiMutating("POST /api/invites/list", ih.handleInviteList)
	s.apiMutating("POST /api/invites", ih.handleInviteCreate)
	// The single read carries no token and no accept URL — those live only in
	// the create response. Registered after /preview, which is a literal and
	// so wins the {id} segment on GET.
	s.api("GET /api/invites/{id}", ih.handleInviteGet)
	s.apiMutating("POST /api/invites/{id}/revoke", ih.handleInviteRevoke)
	s.apiMutating("POST /api/invites/accept", ih.handleInviteAccept)
	// Pre-auth: the invitee previews the token before signing in. The handler
	// runs on the admin pool and discloses only org name + role to the token
	// holder. Listed in preAuthAllowlist; IP-rate-limited (TFAC-433).
	s.mux.Handle("GET /api/invites/preview", s.preAuthRateLimit(http.HandlerFunc(ih.handleInvitePreview)))

	// Entitlements probe — the deployment's licensed EE feature set. CORE (it
	// reports the entitlements checker's state and is NOT itself license-gated),
	// authenticated-session-only: the answer is deployment-level (process-global
	// TF_LICENSE), so no org/role scoping. The frontend's useEntitlements hook
	// reads this once to decide which EE surfaces to render; [] in a community /
	// unlicensed build. See entitlements_handler.go.
	s.api("GET /api/entitlements", s.handleEntitlements)

	// SSO (connection / domains / break-glass admin surface, the verify-
	// before-enforce test-start, and the identifier-first discovery probe) is
	// an Enterprise Edition feature: ee/sso mounts these routes through the
	// route-extension seam (installExtensions, gated on the `sso` entitlement)
	// and registers the LoginExtension that drives SAML start + the callback's
	// enforcement/JIT/test forks. Core holds no SSO symbols.

	// The tasks list read. It replaces the former GET /api/queue and
	// GET /api/tasks outright rather than aliasing them: the two spellings
	// answered the same nominal filter with different hidden ones
	// (?status=queued returned claimed and future-snoozed rows the queue
	// view hides), and one address per read is what keeps that from
	// happening again. apiMutating is deliberate for a body-carrying read —
	// see handleTaskList.
	// Find-or-create the task at a station. The Factory's drop gesture is this
	// plus POST /api/tasks/{id}/delegate: the task is the row, delegating is
	// what you then do to it, so the in-between state — task resolved, run not
	// started — is addressable rather than a partial success buried in one
	// call's response.
	s.apiMutating("POST /api/tasks", s.handleTaskCreate)
	s.apiMutating("POST /api/tasks/list", s.handleTaskList)
	s.api("GET /api/tasks/{id}", s.handleTaskGet)
	// The task's field-write path. It replaces the former /swipe
	// multiplexer, /snooze and /advance: the lifecycle axis and the wake
	// time are columns, and a route per column value is how one gesture
	// grew six arms with different required fields, different authz and
	// different response shapes. The verbs below survive because each
	// carries an effect a field write can't express — an external Jira
	// write, a spawned run, artifact teardown, an audit reversal.
	s.apiMutating("PATCH /api/tasks/{id}", s.handleTaskPatch)
	s.apiMutating("POST /api/tasks/{id}/claim", s.handleTaskClaim)
	s.apiMutating("POST /api/tasks/{id}/delegate", s.handleTaskDelegate)
	s.apiMutating("POST /api/tasks/{id}/undo", s.handleUndo)
	s.apiMutating("POST /api/tasks/{id}/requeue", s.handleRequeue)

	ag := &agentHandler{tx: s.tx, ws: s.ws, spawner: func() *delegate.Spawner { return s.spawner }, reconciler: func() *reconcile.Reconciler { return s.reconciler }}
	s.api("GET /api/agent/conversations/{conversationID}", ag.handleAgentStatus)
	s.api("GET /api/agent/conversations/{conversationID}/messages", ag.handleMessages)
	// Conversation-scoped artifact read (A·6, TFAC-465): the conversation's artifacts across
	// every kind, team-scoped via the conversation. Backs the conversation detail surface (TFAC-470).
	s.api("GET /api/agent/conversations/{conversationID}/artifacts", ag.handleAgentArtifacts)
	// Conversation-scoped external-action read — the audit log filtered to this conversation.
	// Its sibling above answers "what objects does this conversation own"; this answers
	// "what did it do", including the writes that produce no object at all (a
	// review-thread reply, a refused merge, a denied push).
	s.apiMutating("POST /api/agent/conversations/{conversationID}/actions/list", ag.handleAgentActions)
	// The one conversation-level stop. It replaces the former /cancel and
	// /interrupt outright rather than aliasing them: two addresses is how they
	// drifted into two meanings of `open` in the first place.
	s.apiMutating("POST /api/agent/conversations/{conversationID}/stop", ag.handleAgentStop)
	s.apiMutating("POST /api/agent/conversations/{conversationID}/message", ag.handleMessage)
	// The pending set behind the demoted `permission_request` frame: the frame
	// carries only the tool_call_id and every surface reads the prompt from
	// here, so a refresh / second tab / cold load reconstructs it.
	s.api("GET /api/agent/conversations/{conversationID}/permissions", ag.handleAgentPermissions)
	s.apiMutating("POST /api/agent/conversations/{conversationID}/permissions/{toolCallID}", ag.handleAgentPermission)
	// Tier-2 conversation-scoped artifact reconcile (TFAC-464): the conversation view polls this
	// while open to refresh that conversation's non-terminal artifacts against GitHub.
	s.apiMutating("POST /api/agent/conversations/{conversationID}/artifacts/refresh", ag.handleArtifactRefresh)
	s.apiMutating("POST /api/agent/conversations/list", ag.handleConversations)

	// Websocket: wrapped via s.api so the handshake sees claims in
	// r.Context() (sentinel in local mode, real values in multi).
	// handleWS pulls (userID, orgID) out and threads them into the
	// hub's HandleWS so the per-connection scoping in pkg/websocket
	// can filter Broadcast fanout without importing internal/server.
	// Treated as GET-equivalent — no CSRF wrap.
	s.api("GET /api/ws", s.handleWS)

	dh := &dashboardHandler{tx: s.tx, ghResolver: s.ghResolver, backfill: s.kickDashboardBackfill}
	s.api("GET /api/dashboard/stats", dh.handleDashboardStats)
	s.apiMutating("POST /api/dashboard/prs/list", dh.handleDashboardPRs)
	s.api("GET /api/dashboard/prs/{owner}/{repo}/{number}/status", dh.handleDashboardPRStatus)
	s.apiMutating("POST /api/dashboard/prs/{owner}/{repo}/{number}/draft", dh.handleDashboardPRDraft)

	// Prompt imports. Two routes rather than one with a source field: they
	// take different bodies (a paste vs. nothing) and only one of them can
	// exist in multi mode, where the process has no per-tenant disk to scan.
	pi := &promptImportHandler{db: s.db, prompts: s.prompts, tx: s.tx, az: s.az}
	s.apiMutating("POST /api/prompts/from-disk", pi.handlePromptImportFromDisk)
	s.apiMutating("POST /api/prompts/upload", pi.handlePromptUpload)
	// Proxy list: the rows come from GitHub, which reports no total for this
	// read, so total_count is null and page_token wraps the upstream cursor.
	s.apiMutating("POST /api/github/repos/list", s.handleGitHubRepos)
	s.apiMutating("POST /api/github/repos/refresh", s.handleGitHubReposRefresh)
	se := &settingsHandler{
		tx: s.tx, az: s.az,
		bedrockRole: func() bedrockRoleResolver { return s.bedrockRole },
		kickJira:    s.kickJiraChanged,
	}
	s.apiMutating("POST /api/github/preflight-ssh", se.handleGitHubPreflightSSH)
	// URL-only host reachability (the wizard's URL sub-step) — no auth sent,
	// distinct from the creds stage (auth.ValidateGitHub / /api/jira/connect).
	s.apiMutating("POST /api/github/reachability", handleGitHubReachability)
	s.apiMutating("POST /api/repos/list", s.handleRepositories)
	// Addressed by the registry row id, which is what every repo payload
	// carries. by-name is the slug-accepting read the API rules give a
	// resource with a unique name, and the only one: the writes take the id
	// they were served, so a rename cannot repoint a save.
	s.api("GET /api/repos/by-name/{owner}/{repo}", s.handleRepoGetByName)
	s.api("GET /api/repos/{id}", s.handleRepoGet)
	s.apiMutating("PATCH /api/repos/{id}", s.handleRepoUpdate)
	s.apiMutating("POST /api/repos/{id}/branches/list", s.handleRepoBranches)
	s.apiMutating("POST /api/jira/reachability", handleJiraReachability)
	// Bedrock role-mode setup (TFAC-616): returns the control service's caller
	// ARN + the TF-generated External ID so the UI can render a filled
	// trust-policy snippet. POST (not GET) because first entry generates +
	// persists the External ID — a state change that needs CSRF protection,
	// per the mutating-verb convention (route_auth_test enforces GET ≠
	// apiMutating).
	s.apiMutating("POST /api/bedrock/role-setup", se.handleBedrockRoleSetup)
	// The Jira project picker's candidates. A proxy list — the rows come from
	// Jira live, which is what a catalog of dozens behind one org credential
	// is worth — so total_count is null and page_token wraps the upstream
	// offset. Its GitHub sibling reads a mirror instead; see the handler for
	// why the two differ.
	s.apiMutating("POST /api/jira/projects/list", s.handleJiraProjectsList)
	s.api("GET /api/jira/statuses", se.handleJiraStatuses)
	// **Declared exception** to the list-envelope rule. The stock deck is a
	// composite the discovery UI deals from — a readiness status plus two
	// partitions of the same set — not a row list a client walks. It reads the
	// team's whole active Jira set, bounded by the team's tracked projects
	// rather than by a window; the deck is meant to be dealt whole. What it
	// does hold to is one shape in every state: the status field never changes
	// which keys are present.
	s.api("GET /api/jira/stock", s.handleJiraStockGet)
	// One route per carry-over action. The arms share only an eligibility
	// gate: queue mints a task off a synthesized event with no Jira write,
	// claim makes two external Jira writes before it touches a row, and done
	// transitions the ticket and closes the entity without minting anything.
	s.apiMutating("POST /api/jira/stock/queue", s.handleJiraStockQueue)
	s.apiMutating("POST /api/jira/stock/claim", s.handleJiraStockClaim)
	s.apiMutating("POST /api/jira/stock/done", s.handleJiraStockDone)

	// Artifact-id-addressed endpoints back both the GitHub-native PR preview and
	// the review preview (kind-dispatched inside the handlers). The review path
	// (TFAC-463) replaced the local pending_reviews path: GET serves the artifact
	// + live pending-review comments, PATCH stages body/event, the comment routes
	// edit/delete inline comments on the live pending review, approve submits it,
	// and dismiss resolves a single artifact (per-item). The task-level
	// resolve-all (drag-to-Done / Return-to-queue) flows through teardownTaskArtifacts.
	ah := &artifactsHandler{tx: s.tx, ws: s.ws, conversations: s.conversations, ghResolver: s.ghResolver, spawner: func() *delegate.Spawner { return s.spawner }}
	// One read for every kind; the writes are kind-scoped sub-resources, so a
	// review-shaped body can never reach the PR write path.
	s.api("GET /api/artifacts/{id}", ah.handleArtifactGet)
	s.apiMutating("PATCH /api/artifacts/{id}/pr", ah.handleArtifactPRUpdate)
	s.apiMutating("PATCH /api/artifacts/{id}/review", ah.handleArtifactReviewUpdate)
	s.api("GET /api/artifacts/{id}/diff", ah.handleArtifactDiff)
	s.apiMutating("POST /api/artifacts/{id}/approve", ah.handleArtifactApprove)
	// Pull requests only: closing one is a real GitHub write. A review is
	// abandoned through PATCH …/review {state:"dismissed"}.
	s.apiMutating("POST /api/artifacts/{id}/dismiss", ah.handleArtifactDismiss)
	s.apiMutating("POST /api/artifacts/{id}/review/refresh", ah.handleReviewRefresh)
	s.apiMutating("PATCH /api/artifacts/{id}/comments/{commentId}", ah.handleArtifactCommentUpdate)
	s.apiMutating("DELETE /api/artifacts/{id}/comments/{commentId}", ah.handleArtifactCommentDelete)

	fh := &factoryHandler{tx: s.tx}
	s.api("GET /api/factory/snapshot", fh.handleFactorySnapshot)

	ph := &promptsHandler{db: s.db, tx: s.tx, az: s.az}
	// **Declared exceptions to the paginated-list rule.** These two are not
	// lists of rows a tenant owns; they are the build's own vocabulary —
	// domain.AllEventTypes() and the schemas derived from it, fixed at compile
	// time and identical for every caller. There is nothing to filter by,
	// nothing that grows with usage, and a page token would address a set that
	// only changes when the binary does. A client reads them once and caches.
	// Their shape stays a bare list on purpose: paginating a constant is
	// ceremony that makes the surface look mutable.
	s.api("GET /api/event-types", ph.handleEventTypes)
	s.api("GET /api/event-schemas", handleEventSchemasList)
	s.api("GET /api/event-schemas/{event_type}", handleEventSchemaGet)
	// The unified event_handlers surface — one ordered list for both kinds,
	// with `kind` as the row discriminator on every read. The writes are split
	// per kind, because a rule and a trigger require different fields and
	// default `enabled` in opposite directions.
	eh := &eventHandlersHandler{tx: s.tx, az: s.az}
	s.apiMutating("POST /api/event-handlers/list", eh.handleEventHandlersList)
	// The canonical single read: five mutation routes preloaded this row
	// internally and none would hand it back.
	s.api("GET /api/event-handlers/{id}", eh.handleEventHandlerGet)
	s.apiMutating("POST /api/event-handlers/rules", eh.handleEventHandlerCreateRule)
	s.apiMutating("POST /api/event-handlers/triggers", eh.handleEventHandlerCreateTrigger)
	s.apiMutating("PUT /api/event-handlers/reorder", eh.handleEventHandlerReorder)
	s.apiMutating("PATCH /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("DELETE /api/event-handlers/{id}", eh.handleEventHandlerDelete)
	s.apiMutating("POST /api/event-handlers/{id}/promote", eh.handleEventHandlerPromote)
	s.apiMutating("POST /api/event-handlers/{id}/retarget", eh.handleEventHandlerRetarget)
	// Parked event_queue rows — the operator surface over routing work the
	// queue gave up on, and the requeue that puts it back. Org-admin gated
	// inside the handler; the store is the admin-pool EventQueueStore with
	// org_id bound by argument, the same shape as the usage-ops read.
	fe := &failedEventsHandler{az: s.az, queue: s.allStores.EventQueue}
	s.apiMutating("POST /api/events/failed/list", fe.handleFailedEventsList)
	s.api("GET /api/events/failed/{id}", fe.handleFailedEventGet)
	s.apiMutating("POST /api/events/failed/requeue", fe.handleFailedEventsRequeue)
	s.apiMutating("POST /api/prompts/list", ph.handlePromptsList)
	s.apiMutating("POST /api/prompts", ph.handlePromptCreate)
	s.api("GET /api/prompts/{id}", ph.handlePromptGet)
	s.apiMutating("PUT /api/prompts/{id}", ph.handlePromptPut)
	s.apiMutating("DELETE /api/prompts/{id}", ph.handlePromptDelete)
	s.api("GET /api/prompts/{id}/stats", ph.handlePromptStats)
	bh := &blueprintsHandler{tx: s.tx, az: s.az, spawner: func() *delegate.Spawner { return s.spawner }}
	s.apiMutating("POST /api/blueprints/list", bh.handleBlueprintsList)
	s.api("GET /api/blueprints/{id}", bh.handleBlueprintGet)
	s.apiMutating("POST /api/blueprints", bh.handleBlueprintCreate)
	s.apiMutating("PUT /api/blueprints/{id}", bh.handleBlueprintUpdate)
	s.apiMutating("DELETE /api/blueprints/{id}", bh.handleBlueprintDelete)
	// One route for the canvas's bulk read and the per-blueprint read: they
	// differed only in a filter, and a filter is what a list body is for.
	s.apiMutating("POST /api/blueprint-steps/list", bh.handleBlueprintStepsAll)
	s.apiMutating("PUT /api/blueprints/{id}/steps", bh.handleBlueprintStepsPut)
	s.apiMutating("POST /api/blueprints/{id}/merge", bh.handleBlueprintMerge)
	s.apiMutating("POST /api/blueprints/{id}/split", bh.handleBlueprintSplit)
	s.apiMutating("POST /api/blueprints/{id}/reconnect", bh.handleBlueprintReconnect)
	s.apiMutating("POST /api/blueprints/duplicate", bh.handleBlueprintDuplicate)
	s.apiMutating("POST /api/blueprint-runs/list", bh.handleBlueprintRunsList)
	s.api("GET /api/blueprint-runs/{id}", bh.handleBlueprintRunGet)
	s.apiMutating("POST /api/blueprint-runs/{id}/cancel", bh.handleBlueprintRunCancel)

	// Within-org prompt marketplace (TFAC-536) — multi-mode only; every
	// handler opens with gateMarketplace, which 404s in local mode (see
	// marketplace_handler.go).
	mh := &marketplaceHandler{tx: s.tx, az: s.az}
	s.apiMutating("POST /api/marketplace/listings/list", mh.handleMarketplaceList)
	s.apiMutating("POST /api/marketplace/listings", mh.handleMarketplacePublish)
	s.api("GET /api/marketplace/listings/{id}", mh.handleMarketplaceGet)
	s.apiMutating("POST /api/marketplace/listings/{id}/versions", mh.handleMarketplaceListingVersionCreate)
	s.apiMutating("POST /api/marketplace/listings/{id}/delist", mh.handleMarketplaceListingDelist)
	s.apiMutating("POST /api/marketplace/listings/{id}/relist", mh.handleMarketplaceListingRelist)
	s.apiMutating("PUT /api/marketplace/listings/{id}/vote", mh.handleMarketplaceVote)
	s.apiMutating("DELETE /api/marketplace/listings/{id}/vote", mh.handleMarketplaceUnvote)
	s.api("GET /api/marketplace/listings/by-source/{source_id}", mh.handleMarketplaceListingBySource)
	// Install/"copy to my team" (TFAC-538) — materializes the listing's
	// current snapshot into the caller's team as a brand-new fork.
	s.apiMutating("POST /api/marketplace/listings/{id}/install", mh.handleMarketplaceInstall)

	// The org-scoped provider surfaces are provider-first: everything the org
	// configures about GitHub hangs off /api/orgs/{org_id}/github/, and Jira
	// mirrors it. Each is sub-foldered by concern — access/ (how the org
	// reaches the provider: the org credential and the either/or transitions),
	// app/ (the registered App or OAuth app), identity/ (how the caller binds
	// their own account), connect/ (the OAuth leg of that binding). The App is
	// an access mode, not a sibling of access, which is why cutover (PAT→App)
	// and switch-to-pat (App→PAT) live one level apart rather than under two
	// unrelated parents. Provider is the stable axis: a GHES host change
	// ripples through access, identity, and webhooks together.

	// GitHub App manifest registration. The launch endpoint serves a
	// script-free bounce page (carrying its own per-response CSP) that
	// POSTs the manifest cross-origin to the org's GitHub host; the
	// callback exchanges the temp code for App credentials. Both validate
	// org membership + admin role inside the handler via
	// r.PathValue("org_id"). Works in both local and multi mode.
	s.api("GET /api/orgs/{org_id}/github/app/register/launch", s.handleGitHubAppRegisterLaunch)
	s.api("GET /api/orgs/{org_id}/github/app/register/callback", s.handleGitHubAppRegisterCallback)
	// Bring-your-own-App import: the second way into App mode for orgs
	// that can't or shouldn't create the App themselves. Validates an App ID +
	// private key via an app-JWT GET /app, permission-preflights, and persists
	// through the same path the manifest callback uses (staging rule unchanged).
	// A JSON fetch from the SPA (not a top-level navigation like launch/callback),
	// so it rides apiMutating (CSRF). Org-admin (gated inside the handler).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/import", s.handleGitHubAppImport)
	// Read-only status + install deep-link for the Workspace Settings panel.
	// Any org member (read), so RequireOrgMember rather than RequireOrgAdmin.
	s.api("GET /api/orgs/{org_id}/github/app", s.handleGitHubAppStatus)
	s.api("GET /api/orgs/{org_id}/github/app/install-url", s.handleGitHubAppInstallURL)
	// On-demand installation reconcile — the "UI panel refresh" half of D11
	// installation discovery (the poller cycle is the other). Admin-only (the
	// setup wizard's install step + the Settings App panel call it) and
	// mode-agnostic. Mutating: it reconciles the installation mirror via the
	// same API backfill the poller runs, so it rides apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/installations/refresh", s.handleGitHubAppInstallationsRefresh)
	// Replay the App's failed installation webhook deliveries — the repair for
	// an org whose receiver was rejecting them (no webhook secret, or the wrong
	// one) while its installation mirror went stale. Admin-only, and mutating
	// in the sense that matters: it asks GitHub to deliver again, so it rides
	// apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/webhook/replay", s.handleGitHubAppWebhookReplay)

	// GitHub access either/or transitions. GitHub access is
	// strictly App XOR PAT per org; these commit the switches and surface the
	// inform-only reachability diffs. All org-admin (gated inside the handler).
	//   - app/cutover: commit a staged PAT→App switch (activate App + delete PAT).
	//   - pat/switch-to: full App teardown, validate + store the new PAT.
	//   - DELETE github/app: discard a staged (not-yet-live) App registration.
	//   - app/cutover-preflight, pat/preflight: inform-only reachability diffs,
	//     each under the flavor it is a preflight for.
	// The two commits + the discard mutate state (apiMutating, CSRF); the
	// cutover preflight is a read (api); the PAT preflight POSTs a token to
	// probe reach but stores nothing — still apiMutating for the same-origin
	// guard.
	s.apiMutating("POST /api/orgs/{org_id}/github/app/cutover", s.handleGitHubAppCutover)
	s.apiMutating("POST /api/orgs/{org_id}/github/pat/switch-to", s.handleGitHubAccessSwitchToPAT)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/app", s.handleGitHubAppDiscard)
	s.api("GET /api/orgs/{org_id}/github/app/cutover-preflight", s.handleGitHubAppCutoverPreflight)
	s.apiMutating("POST /api/orgs/{org_id}/github/pat/preflight", s.handleGitHubAccessPATPreflight)

	// The org integration credentials, each an addressable resource with an
	// explicit bind + unbind rather than a field on the bulk settings save.
	// Everything a credential write owes — live validation, the vault write, the
	// poller re-due, an access change-log row — belongs to exactly one route
	// per credential, which is what keeps it from being re-derived (and
	// forgotten) from whichever fields a bulk save happened to carry. See
	// org_credentials.go. POST /api/settings/org is now pure config: no secret,
	// no outbound call, and it can no longer revoke access as a side effect.
	// The Jira half of the pair is registered with the rest of the Jira
	// surface below; the rationale above covers both.
	// The PAT is a credential flavor, and the flavor is the resource: its bind
	// and unbind here, its preflight and the switch onto it beside them, all
	// under github/pat as the App's routes sit under github/app.
	s.apiMutating("PUT /api/orgs/{org_id}/github/pat", s.handleGitHubPATPut)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/pat", s.handleGitHubPATDelete)

	// The org's pure CONFIG — hosts, poller cadence, clone transport, spend
	// governance. Path-scoped like the credential resources beside it because
	// the write is admin-gated, and PATCH rather than POST because it is a
	// partial update: absent keeps, null clears, and the caller's `version`
	// makes a concurrent save a 409 instead of a silent overwrite.
	s.api("GET /api/orgs/{org_id}/settings", s.handleOrgSettingsGet)
	s.apiMutating("PATCH /api/orgs/{org_id}/settings", s.handleOrgSettingsPatch)

	// Which event sources can reach this org — derived on every read, never
	// stored, and member-gated like the settings read above: it is the signal
	// the authoring surfaces and the source cards render from, and every
	// member renders those. See sources_handler.go.
	s.api("GET /api/orgs/{org_id}/sources", s.handleOrgSources)
	// The same collection addressed one element at a time. The read is
	// member-gated like the list; the write is org admin, because a source is
	// in no team's tracked set and pausing one lands on every team in the org —
	// and because unbinding its credential already takes an org admin.
	s.api("GET /api/orgs/{org_id}/sources/{kind}", s.handleOrgSource)
	s.apiMutating("PATCH /api/orgs/{org_id}/sources/{kind}", s.handleOrgSourcePatch)

	// The org's LLM provider credential — one resource per credential SHAPE, so
	// a route's required fields are fixed and a blank secret never selects a
	// second behaviour. Rotation is the PUT with a new value; removal is the
	// DELETE. An org may hold both providers at once: a bind replaces only
	// material of its own provider, and says so in the response. See
	// llm_credentials.go.
	s.apiMutating("PUT /api/orgs/{org_id}/llm/anthropic", se.handleAnthropicPut)
	s.apiMutating("DELETE /api/orgs/{org_id}/llm/anthropic", se.handleAnthropicDelete)
	s.apiMutating("PUT /api/orgs/{org_id}/llm/bedrock/access-keys", se.handleBedrockAccessKeysPut)
	s.apiMutating("PUT /api/orgs/{org_id}/llm/bedrock/bearer", se.handleBedrockBearerPut)
	s.apiMutating("PUT /api/orgs/{org_id}/llm/bedrock/role", se.handleBedrockRolePut)
	s.apiMutating("DELETE /api/orgs/{org_id}/llm/bedrock", se.handleBedrockDelete)

	// The models this deployment offers, as one org sees them: the shipped
	// catalog joined to the org's own enable-set. Org-scoped because that
	// enable-set is org state, and member-level because its widest reader is a
	// team admin who cannot see org settings but must know what the org
	// enabled. Unpaginated for the same reason /api/event-types is — a
	// compile-time vocabulary, not a collection that grows with use.
	//
	// The team-scoped read is the same node one scope down (as /usage is
	// mounted at both): the org's catalog minus the providers an org admin
	// restricted that team from spending against. The restriction itself is
	// written beside it and is org-admin-only — a team that could widen its own
	// would not be restricted.
	mdh := &modelsHandler{az: s.az, tx: s.tx, prober: func() modelProber { return s.modelProber }}
	s.api("GET /api/orgs/{org_id}/models", mdh.handleModelsList)
	s.api("GET /api/teams/{team_id}/models", mdh.handleTeamModelsList)
	s.apiMutating("PUT /api/teams/{team_id}/models/providers", mdh.handleTeamProvidersPut)
	// The availability probes: the same verb on the item and on the
	// collection, because "test this model" and "test this provider's
	// candidates" are two nameable things with two response shapes. Both are
	// POSTs for an effect no field write can express — each one calls the
	// provider and is billed for it.
	s.apiMutating("POST /api/orgs/{org_id}/models/{model_key}/test", mdh.handleModelTest)
	s.apiMutating("POST /api/orgs/{org_id}/models/tests", mdh.handleModelTestSweep)

	// "Connect GitHub" user-to-server OAuth — binds a host-verified GitHub
	// login to the signed-in user (identity, not access, not login).
	// start redirects to {github_base_url}/login/oauth/authorize;
	// callback exchanges the code, whoamis, and writes
	// user_github_identities(source='connect_oauth'). Both are GETs reached
	// via top-level navigation (the start from the gate page, the callback
	// from GitHub's redirect), so they ride s.api (withSession, no CSRF wrap)
	// and carry their own OAuth state-cookie CSRF defense. Any org member
	// binds their own identity. The identity-status read backs the gate.
	//
	// These two paths are frozen — they are the one part of the org-scoped
	// GitHub surface that isn't ours to rename. The callback is baked into a
	// manifest-created App's callback_urls at creation time, and a
	// bring-your-own-App owner registers it by hand (githubAppStatusResponse
	// .ConnectCallbackURL exists to hand them the exact string). It is the
	// redirect_uri sent on every user consent, and GitHub rejects a mismatch,
	// so a rename breaks per-user Connect for every already-registered App
	// until each owner edits their App settings on GitHub. start shares the
	// parent and moves with the callback or not at all.
	s.api("GET /api/orgs/{org_id}/github/connect/start", s.handleGitHubConnectStart)
	s.api("GET /api/orgs/{org_id}/github/connect/callback", s.handleGitHubConnectCallback)
	s.api("GET /api/orgs/{org_id}/github/identity", s.handleGitHubIdentityStatus)
	// Capture-and-discard per-user identity from a user-supplied PAT (PAT_2):
	// validate → whoami → write user_github_identities → drop the token. The
	// always-available fallback to Connect (and the only path when no App is
	// registered). Never stores the token.
	s.apiMutating("POST /api/orgs/{org_id}/github/identity/pat", s.handleGitHubIdentityPAT)

	// The org's Jira credential — the Jira half of the addressable-resource
	// pair whose rationale sits with the GitHub PAT routes above.
	s.apiMutating("PUT /api/orgs/{org_id}/jira/access/credential", se.handleJiraConnect)
	s.apiMutating("DELETE /api/orgs/{org_id}/jira/access/credential", s.handleJiraCredentialDelete)

	// Per-user Jira access — the Jira sibling of the GitHub identity flow
	// (jira_connect.go). status reports connected from a STORED credential
	// (Jira's user level holds access, not just identity); the PAT path
	// validates the token, STORES it (per-user vault scope), and derives the
	// user's Jira identity. DC = paste-a-PAT; connect_available now reflects
	// whether an Atlassian OAuth app resolves (the Cloud Connect gate). Any org
	// member binds their own access.
	s.api("GET /api/orgs/{org_id}/jira/identity", s.handleJiraIdentityStatus)
	s.apiMutating("POST /api/orgs/{org_id}/jira/identity/pat", s.handleJiraIdentityPAT)

	// Per-user "Connect Jira" Cloud OAuth (3LO) — the one-click counterpart of
	// the paste path, gated on connect_available (an Atlassian app resolves).
	// start redirects to auth.atlassian.com/authorize; callback exchanges the
	// code, resolves the cloud_id, whoamis, and STORES the rotating refresh
	// token (source='connect_oauth'). Both are GETs reached via top-level
	// navigation, so they ride s.api (withSession, no CSRF wrap) and carry their
	// own OAuth state-cookie CSRF defense — the GitHub Connect pattern.
	//
	// Frozen for the same reason as the GitHub connect pair above: the
	// callback is the redirect_uri registered on the Atlassian OAuth app,
	// external config an operator owns, so renaming it breaks Connect Jira
	// for every already-configured app until its owner updates it.
	s.api("GET /api/orgs/{org_id}/jira/connect/start", s.handleJiraConnectStart)
	s.api("GET /api/orgs/{org_id}/jira/connect/callback", s.handleJiraConnectCallback)

	// Per-org Atlassian OAuth (3LO) app config — the credential layer the
	// per-user "Connect Jira" flow runs against (the flow itself is a later
	// ticket). The Jira sibling of the GitHub App import card: an admin enters
	// a bring-your-own Atlassian app (client_id + client_secret), which becomes
	// the per-org override over the deployment first-party app (hosted) / is the
	// app itself (local). status is any-member (the card renders for everyone);
	// import + delete are admin (gated inside the handler). The two mutators are
	// JSON fetches from the SPA, so they ride apiMutating (CSRF).
	s.api("GET /api/orgs/{org_id}/jira/app", s.handleJiraAppStatus)
	s.apiMutating("POST /api/orgs/{org_id}/jira/app", s.handleJiraAppImport)
	s.apiMutating("DELETE /api/orgs/{org_id}/jira/app", s.handleJiraAppDelete)

	// Per-org GitHub App webhook receiver. Pre-auth (GitHub has no
	// session) and identified by org_id from the path; the handler
	// verifies the HMAC signature against that org's stored webhook
	// secret before any side effect, so it's on the preAuthAllowlist.
	//
	// Wrapped in the signed-webhook tier — the same one the Slack receiver
	// takes, and the tier that was built for this description: a route that
	// authenticates every request itself, where the cap defends the cost of
	// getting to the rejection rather than the rejection itself. Getting
	// there is not free here: resolving the secret to verify against reads
	// the org's settings, its registration row, and the vault, all before
	// the signature is checked (github_webhook_secret.go caches the result
	// per org; this bounds how fast an anonymous caller can miss that
	// cache). No-op in local mode.
	s.mux.Handle("POST /api/webhooks/github/{org_id}", s.signedWebhookRateLimit(http.HandlerFunc(s.handleGitHubWebhook)))

	// Registered server extensions (Enterprise Edition, ee/) mount their
	// routes here, each gated on its license feature. No-op in a community
	// build / unlicensed deployment. Mounted before the SPA fallback; Go's
	// ServeMux resolves by longest pattern, so specific /api/* extension
	// routes take precedence over "/" regardless.
	s.installExtensions()

	// Unknown /api/* → JSON 404 (TFAC-409 item 5). Without this, the greedy "/"
	// SPA fallback below answers any unmatched path — including a stray or
	// typo'd /api/* — with 200 + index.html, which masks client typos and reads
	// as success to an API consumer. Registered after every real /api/* route:
	// Go's ServeMux resolves by the most-specific pattern, so a concrete
	// "GET /api/whatever" still wins over this prefix. This handler is
	// method-agnostic, so it also absorbs wrong-method requests to a known path
	// (e.g. DELETE /api/health): they match here and become a JSON 404 rather
	// than a 405 — the method-aware 405 was never reachable anyway, since the
	// "/" SPA catch-all already matched every method and turned those into
	// 200 + index.html. So this is a strict improvement (404 JSON over 200 HTML),
	// not a 405 regression. Only an unregistered /api/* subtree falls here.
	s.mux.HandleFunc("/api/", s.handleAPINotFound)

	// Frontend: serve embedded SPA, with fallback to index.html for client-side routing
	s.mux.HandleFunc("/", s.handleFrontend)
}

// handleAPINotFound answers any unmatched /api/* path with a JSON 404 instead
// of letting the SPA fallback serve index.html. Keeps the API surface honest:
// an unknown endpoint reads as a 404 to a programmatic caller rather than a
// 200 page of HTML. See the registration in routes() for why this can't shadow
// a real route.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	httpx.NotFound(w, "endpoint")
}

// handleFrontend serves the embedded React SPA. Non-file requests fall back to index.html
// so that client-side routing works.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		http.Error(w, "frontend not built — run: cd frontend && pnpm install && pnpm run build", http.StatusNotFound)
		return
	}

	path := r.URL.Path
	if path == "/" {
		path = "index.html"
	} else {
		path = strings.TrimPrefix(path, "/")
	}

	// Try to serve the file directly
	if _, err := fs.Stat(s.static, path); err == nil {
		http.ServeFileFS(w, r, s.static, path)
		return
	}

	// Fallback to index.html for SPA client-side routing
	http.ServeFileFS(w, r, s.static, "index.html")
}

// SetStatic sets the embedded frontend filesystem. Also computes
// SHA-256 hashes of every inline <script> block in index.html so the
// CSP middleware can allowlist them via `'sha256-...'` directives —
// keeps script-src tight without requiring frontend changes.
func (s *Server) SetStatic(f fs.FS) {
	s.static = f
	hashes, err := computeInlineScriptHashes(f)
	if err != nil {
		serverLog.Warn("inline script hash compute failed; csp will block inline scripts", "error", err)
	}
	s.inlineScriptHashes = hashes
}

// SetSpawner sets the delegation spawner for agent conversations.
func (s *Server) SetSpawner(sp *delegate.Spawner) {
	s.spawner = sp
}

// SetOnGitHubChanged registers a callback for GitHub config changes (creds, URL, repos).
// The callback re-dues the org's GitHub poll so the new credential/repo set
// applies on the next wake; repo profiling is no longer triggered here — it's
// driven by the system:poll "profiler" subscriber off that poll's completion.
// The orgID is the tenant whose creds changed — closure re-resolves via SecretStore.
//
// The registered callback is wrapped so a FORCED reachable-repo refresh is
// kicked for the org alongside the re-due. A creds rotation, an App install, or
// a host repoint moves which repositories the org can reach without moving the
// timestamp the mirror's TTL reads, so the mirror is wrong in fact while looking
// fresh — the one staleness a TTL cannot detect, which is why the explicit force
// exists at all.
//
// The refresh is out of band by construction (it is a Manager trigger, not a
// call), so this stays as cheap as the eviction it replaces. A repo-TRACKING
// save also lands here — tracking is not reachability, so that refresh is
// redundant — and it is left in rather than special-cased: it is one
// enumeration per human Save gesture, and splitting the hook into
// "creds changed" and "repos changed" variants would put the burden of picking
// the right one on six call sites.
func (s *Server) SetOnGitHubChanged(fn func(orgID string)) {
	s.onGitHubChanged = func(orgID string) {
		s.kickReachRefresh(orgID, true)
		if fn != nil {
			fn(orgID)
		}
	}
}

// SetOnSourcesChanged registers the callback for an event-source pause/resume.
// See the field.
func (s *Server) SetOnSourcesChanged(fn func(orgID, kind string)) {
	s.onSourcesChanged = fn
}

// SetOnJiraChanged registers a callback for Jira config changes.
// This restarts only the Jira poller. See SetOnGitHubChanged for orgID semantics.
func (s *Server) SetOnJiraChanged(fn func(orgID string)) {
	s.onJiraChanged = fn
}

// SetScorerTrigger registers a callback to kick the AI scorer. Used by
// flows that create tasks outside the normal poll→event path (e.g.
// carry-over) so scoring starts immediately rather than waiting for the
// next poll cycle.
func (s *Server) SetScorerTrigger(fn func(orgID string)) {
	s.scorerTrigger = fn
}

// SetProfilerTrigger registers the per-org repo-profiling trigger (the
// Manager's Trigger). force=true bypasses the 3-day TTL. Used by the
// repo-set-change / "Re-profile" path so a re-profile starts immediately
// rather than waiting for the next poll cycle's TTL-gated pass.
func (s *Server) SetProfilerTrigger(fn func(orgID string, force bool)) {
	s.profilerTrigger = fn
}

// reachableCredentialClass resolves which credential class this org's
// reachable-repo entries are keyed under — the stored class narrowed by the
// App-XOR-PAT gate, which is the same narrowing the refresh applies when it
// writes them. The picker and the team-repos write gate both key on it, and
// keying a read differently from the write is how a mirror reads as empty for an
// org that has one.
//
// Built per call rather than held: it is two store pointers and no state, so
// constructing it here costs nothing and it always reads whichever stores this
// Server currently holds.
func (s *Server) reachableCredentialClass(ctx context.Context, orgID string) (domain.GitHubCredentialClass, error) {
	return reachcache.NewClassResolver(s.orgs, s.githubApps).For(ctx, orgID)
}

// SetReachTrigger registers the per-org reachable-repo refresh trigger (the
// reachcache Manager's Trigger). force=true bypasses its TTL. Used by the
// picker read (non-forced, when the mirror is empty or stale), the explicit
// refresh control, and every GitHub credential change.
//
// Nil until wired — bare test constructions and any role that builds no
// background managers. kickReachRefresh nil-checks it, so a picker read on such
// a Server serves whatever the mirror holds and asks for nothing.
func (s *Server) SetReachTrigger(fn func(orgID string, force bool)) {
	s.reachTrigger = fn
}

// kickReachRefresh asks for a reachable-mirror refresh, out of band. It never
// blocks the caller and never reports failure: a refresh that does not happen
// costs a staler mirror on the next read, which is the same cost as the TTL not
// having elapsed yet, and a request has nothing useful to do with the error.
func (s *Server) kickReachRefresh(orgID string, force bool) {
	if s.reachTrigger == nil || orgID == "" {
		return
	}
	s.reachTrigger(orgID, force)
}

// SetReconciler registers the shared artifact Reconciler that backs the Tier-2
// conversation-scoped refresh endpoint (TFAC-464). The same instance the background
// Tier-1 Manager runs, so a foreground refresh and a background cycle apply the
// identical reconcile path. Nil until wired — the endpoint 503s until then.
func (s *Server) SetReconciler(rc *reconcile.Reconciler) {
	s.reconciler = rc
}

// SetBedrockRoleResolver wires the Bedrock role-mode setup + connect-probe
// resolver (TFAC-616). Called once at startup in multi mode with the shared
// *llmcred.Resolver; left nil in local mode (no ambient AWS SDK), where the
// role-setup endpoint reports the method is control-service only.
func (s *Server) SetBedrockRoleResolver(r bedrockRoleResolver) {
	s.bedrockRole = r
}

// SetModelProber wires the model-availability prober. Called once at startup on
// every serving process, in either mode — the transport differs (a direct call
// in multi, the agent runtime in local) but the question and the verdicts do
// not. Nil until wired: the test routes report the deployment cannot probe.
func (s *Server) SetModelProber(p modelProber) {
	s.modelProber = p
}

// SetDashboardBackfiller registers the per-user dashboard-history backfill
// (the poller's BackfillUserDashboard). Wired in multi mode only — local mode
// leaves it nil and relies on the per-cycle Phase 1b backfill, so
// kickDashboardBackfill no-ops there (TFAC-396).
func (s *Server) SetDashboardBackfiller(fn func(ctx context.Context, orgID, userID, login, host string) error) {
	s.dashboardBackfill = fn
}

// kickDashboardBackfill fires a one-shot dashboard-history backfill for a bound
// (user, host) and returns immediately — the work runs detached so it never
// blocks the request (identity bind or dashboard read) that triggered it. The
// backfiller is marker-guarded downstream, so repeated kicks for an
// already-backfilled identity are cheap no-ops. No-op when unwired (local mode)
// or when the identity isn't fully resolved.
func (s *Server) kickDashboardBackfill(orgID, userID, login, host string) {
	fn := s.dashboardBackfill
	if fn == nil || login == "" || host == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := fn(ctx, orgID, userID, login, host); err != nil {
			serverLog.Warn("dashboard backfill failed", "org", orgID, "user", userID, "host", host, "error", err)
		}
	}()
}

// SetEventBus wires the in-process event bus so the GitHub webhook
// receiver can publish verified deliveries. Wired post-construction in
// main.go after the bus is created.
func (s *Server) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

// WSKicker is the multi-mode cross-pod session-kick publisher (TFAC-584):
// the plain *wsbackplane.Backplane satisfies this directly. Every
// CloseUserConnections call site closes THIS pod's local sockets first,
// then calls PublishKick so every OTHER control pod does the same for
// its own local sockets — see internal/wsbackplane's PublishKick doc
// comment for why the ordering (local close, then publish) is what keeps
// this from ever echoing.
type WSKicker interface {
	PublishKick(ctx context.Context, userID, sid, orgID string, code int, reason string)
}

// SetWSBackplane wires the multi-mode cross-pod kick publisher
// (TFAC-584). Wired post-construction in internal/app, mirroring
// SetEventBus; nil (the default, and always the case in local mode)
// leaves every kick call site local-only, unchanged from before this
// existed.
func (s *Server) SetWSBackplane(b WSKicker) {
	s.wsBackplane = b
}

// SetVersion records main.Version (the release tag, or "dev") for GET
// /readyz (TFAC-573). Wired in internal/app/httpserver.go's buildServer,
// right after server.New, in both modes.
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetMigrationsOK records that db.Migrate completed successfully at boot,
// for GET /readyz's "migrations" hard check (TFAC-573). Called with true
// in internal/app/app.go's New, right after buildServer succeeds — by
// that point openStores has already run db.Migrate to completion (New
// would have returned its error otherwise), so this simply makes that
// already-true fact visible to the handler. See the migrationsOK field
// doc for why the check exists despite always passing today.
func (s *Server) SetMigrationsOK(ok bool) {
	s.migrationsOK = ok
}

// SetPollerManager wires the poller Manager's Health() snapshot into GET
// /readyz (TFAC-573): the poller-alive hard check plus the per-org
// poll-staleness soft signal. Takes the bound method value rather than
// the whole *poller.Manager so readyz_handler.go depends on one function,
// not the poller package's full surface (PollSoon, RestartAll, etc).
func (s *Server) SetPollerManager(healthFn func(ctx context.Context) poller.HealthSnapshot) {
	s.pollerHealth = healthFn
}

// SetLeaseStatus wires the background-brain lease elector's status into
// GET /readyz (TFAC-583, spec §8.3): the `lease` field, and the switch
// between the holder's byte-identical-to-TFAC-573 poller hard-check and a
// standby's informational "standby" report. Called only for role=control
// (internal/app); leaving it unset (role=all / local, or a bare test
// construction) makes the handler treat this pod as the holder and omit
// the `lease` field — the frozen single-process contract.
func (s *Server) SetLeaseStatus(fn LeaseStatusFunc) {
	s.leaseStatus = fn
}

// SetIngestor wires the durable ingest seam so ExtensionAPI.PublishEvent can
// delegate to it. Wired in internal/app/subsystems.go immediately after
// ingest.New — before that point (and in any test wiring that skips it),
// ingestor is nil and PublishEvent drops loudly rather than falling back to
// a bare bus publish (which would silently skip the durable outbox).
func (s *Server) SetIngestor(i *ingest.Ingestor) {
	s.ingestor = i
}

// SetInstallationTokensInvalidHook REPLACES the callback fired when an
// installation's minted tokens stop being usable — a verified
// installation.deleted or installation.suspend webhook, and the App-credential
// change paths. New already wires that callback to the resolver's own token
// cache, so this is an override rather than the wiring: whatever it installs
// takes over, and passing nil disables the invalidate entirely. Its caller
// today is a test observing which installation a delivery fired for.
func (s *Server) SetInstallationTokensInvalidHook(fn func(orgID, installationID string)) {
	s.onInstallationTokensInvalid = fn
}

// MarkJiraRestarted records the moment orgID's Jira poller was restarted.
// Clears readiness so jiraPollReady reports false until a completion event
// arrives. Call this before kicking off a Jira poller restart.
//
// Backed by the org-scoped poll_readiness table (TFAC-583), not a
// process-local field: the pod that restarts the poller (any control pod
// handling the config-save request) is not necessarily the pod that runs
// it, and is not necessarily the pod that later serves /api/jira/stock —
// see jiraPollReady. Best-effort: a write failure is logged and the
// request proceeds; the worst case is a stale readiness read, not a
// broken save.
func (s *Server) MarkJiraRestarted(ctx context.Context, orgID string) {
	if err := s.allStores.PollReadiness.MarkRestarted(ctx, orgID, "jira"); err != nil {
		serverLog.Warn("mark jira poll restarted failed", "org", orgID, "error", err)
	}
}

// jiraPollReady returns true when orgID's Jira poller has completed at
// least one cycle since its last restart. Used by /api/jira/stock to gate
// the list response. Reads through the admin pool (same posture as the
// instances table — an already-authorized orgID, not a browsable RLS
// surface) so a standby control pod's API reflects readiness the leader's
// poller produced, not just its own (this pod may never have run a Jira
// poller at all). A read failure fails closed (not ready) — the caller
// falls back to the existing {status:"polling"} response rather than
// showing a possibly-incomplete list.
func (s *Server) jiraPollReady(ctx context.Context, orgID string) bool {
	ready, err := s.allStores.PollReadiness.Ready(ctx, orgID, "jira")
	if err != nil {
		serverLog.Warn("read jira poll readiness failed", "org", orgID, "error", err)
		return false
	}
	return ready
}

// Prompt handlers are in prompts_handler.go
// Skill import handler is in skills_handler.go
