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

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/github/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jira/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linear/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/avatars"
	"github.com/sky-ai-eng/triage-factory/internal/server/blueprints"
	"github.com/sky-ai-eng/triage-factory/internal/server/deliverables"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/marketplace"
	"github.com/sky-ai-eng/triage-factory/internal/server/models"
	"github.com/sky-ai-eng/triage-factory/internal/server/prompts"
	"github.com/sky-ai-eng/triage-factory/internal/server/teams"
	"github.com/sky-ai-eng/triage-factory/internal/server/usage"
	"github.com/sky-ai-eng/triage-factory/internal/server/work"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/knowledgeevent"
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
	repos            db.RepositoryStore      // repositories CRUD for repos/settings handlers
	events           db.EventStore           // events audit log Record/Latest for stock carry-over + factory drag-to-delegate
	secrets          db.SecretStore          // canonical credential read/write path — local-mode keychain, multi-mode vault
	teams            db.TeamsStore           // resolves the request org's default team for handlers that synthesize team-scoped rows (tasks, prompts)
	orgs             db.OrgsStore            // per-org settings: GitHub/Jira base URLs, poll intervals, clone protocol
	jiraRules        db.JiraStatusRulesStore // per-team Jira status rules
	linearRules      db.LinearTeamRulesStore // per-team Linear tracking rules
	githubApps       db.GitHubAppsStore      // per-org GitHub App registrations (manifest flow)
	reachableRepos   db.ReachableReposStore  // the reachable-repo mirror the picker lists from and the team-repos write gate validates against
	githubDeliveries db.GitHubDeliveryStore  // applied webhook deliveries, so a redelivery is dropped before the mirror write and the bus publish
	// githubPendingBinds holds the in-flight deployment-App bind ceremonies —
	// the durable half of the CSRF pair the managed-bind callback consumes to
	// learn which org a returning installation belongs to.
	githubPendingBinds db.GitHubPendingBindStore

	// deploymentApp is the deployment's own GitHub App credential, read once
	// from the environment at construction (multi mode only) and shared with
	// the resolver. The bind ceremony reads its client secret; nothing else
	// here touches it.
	deploymentApp githubapp.DeploymentApp
	authEvents    db.AuthEventStore // SOC2 authentication audit log of record — written best-effort via recordAuthEvent at the auth write-sites
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
	mux       *routeMux
	static    fs.FS
	ws        *websocket.Hub
	// wsBackplane is the optional multi-mode cross-pod kick publisher, wired
	// via SetWSBackplane. Nil in local mode and before wiring; every call site
	// nil-checks it, so on a single-pod deployment a session revoke closes
	// only this pod's own sockets.
	wsBackplane WSKicker
	// teamKB is the team knowledge-base seam (internal/kbstore) the /kb routes
	// read and write through — the object store in multi, plain files under
	// the state root in local. Wired via SetTeamKB after construction, because
	// the blob store it is built over is resolved with the rest of the
	// execution subsystem; teamKBNotifier is its websocket half, built
	// alongside it so the two can never disagree about which deployment shape
	// they are in.
	teamKB         kbstore.KB
	teamKBNotifier *knowledgeevent.Notifier
	spawner        *delegate.Spawner
	// reconciler backs the Tier-2 conversation-scoped deliverable refresh
	// endpoint — the same Reconciler the background Tier-1 Manager runs. Wired
	// via SetReconciler after construction; nil until then, so the endpoint
	// 503s rather than panicking if a request races startup.
	reconciler *reconcile.Reconciler
	// bedrockRole resolves the Bedrock role-mode setup + connect probe's live
	// AWS calls. Wired via SetBedrockRoleResolver after construction; nil in
	// local mode and until then, so the role-setup endpoint reports "role mode
	// requires the control service" rather than panicking.
	bedrockRole bedrockRoleResolver
	// placement backs the GET /api/fleet/placement explainer: the computed
	// capacity-weighted rendezvous candidate order for a key. Wired via
	// SetPlacementResolver after construction; nil until then so the endpoint
	// 503s rather than panicking if a request races startup.
	placement placementResolver
	// modelProber spends one minimal request establishing whether an org's
	// credentials can invoke a given model. Wired via SetModelProber after
	// construction; nil in local mode, where nothing probes and the test
	// routes say the deployment cannot answer.
	modelProber models.Prober
	// fleetQueue backs the GET /api/fleet/queue view: per-org
	// conversation-queue shares (active/queued + cap). Satisfied by the
	// ConversationQueue store; a narrow interface so the handler test can
	// inject canned shares.
	fleetQueue fleetQueueReader
	// deploymentSigner is this control pod's deployment commit signer, nil
	// when none is configured (SetDeploymentCommitSigner).
	deploymentSigner *commitsigner.DeploymentSigner
	// ghResolver picks the right GitHub credential (org App installation token
	// → PAT) per request, given the org + target account. Every per-repo
	// handler operation goes through it — review diff/submit, pending-PR
	// submit, branches, dashboard — and there is no process-global PAT client.
	// Two paths build a request-scoped PAT client directly because they need
	// the PAT identity specifically: the repo picker's fallback and
	// GitHub-teams discovery. Built in New from the stores, so it's never nil
	// — handlers don't guard.
	ghResolver ghclient.Resolver
	// ghTokenCache is the per-installation App-token cache backing ghResolver.
	// Retained on the Server (not just handed to the resolver) so the paths
	// that learn a token has died — installation.deleted,
	// installation.suspend, an App cutover or teardown — can Invalidate it,
	// the same reason jiraTokenCache is held here. Built in New, never nil.
	ghTokenCache ghclient.TokenCache
	// jiraResolver routes Jira writes by provenance: ForSystem for the org/bot
	// service cred, ForUser for the acting user's own stored credential.
	// User-initiated board claim / undo / requeue resolve via ForUser so the
	// write is attributed to the user, not the service account. Built in New
	// from the stores, so it's never nil — handlers don't guard.
	jiraResolver jira.Resolver
	// linearResolver resolves the org's Linear service credential for the
	// Linear picker reads and the team-rules write gate, and a user's own
	// credential for the writes they make. Built in New, never nil; tests swap
	// it to point the client at a fake GraphQL endpoint.
	linearResolver linear.Resolver
	// linearOAuthApps resolves the Linear OAuth app an org's install ceremony
	// and its refreshes run against (the org's own row → the deployment app).
	// Built in New, never nil.
	linearOAuthApps linear.OAuthAppResolver
	// linearOAuthMinter performs Linear's OAuth exchanges for the install
	// ceremony and the disconnect's revoke. Built in New, never nil; tests
	// point it at a fake token endpoint.
	linearOAuthMinter *linearoauth.Minter
	// linearInstalls is org_linear_installs on the admin pool, for the reads
	// and writes the install ceremony, the disconnect and the app card make
	// outside a transaction.
	linearInstalls db.LinearInstallsStore
	// validateLinear checks a Linear credential live and reports who it acts
	// as. auth.ValidateLinear in production; tests point it at a fake GraphQL
	// endpoint.
	validateLinear func(ctx context.Context, cfg linear.Config) (*auth.LinearUser, *auth.LinearOrganization, error)
	// anthropicCredentialMu and bedrockCredentialMu
	// serialize each credential's local-mode bind and unbind across snapshot,
	// transaction and restore (guardLocalSecretWrite). The keychain sits
	// outside the SQLite transaction, so two overlapping writes could
	// otherwise snapshot the same prior key and the one that fails would
	// restore it over the one that committed. GitHub, Jira and Linear need no
	// mutex of their own: every GitHub credential transition holds
	// githubAppRegMu for the org, every Jira one jiraCredentialMu, and every
	// Linear writer, the token cache's rotation included, holds
	// linearCredentialLock.
	anthropicCredentialMu sync.Mutex
	bedrockCredentialMu   sync.Mutex
	// linearCredentialLock is the per-org lock every writer of an org's
	// Linear credential holds, this server's handlers and every token cache
	// alike (lockLinearCredential).
	linearCredentialLock *linearoauth.CredentialLock
	// linearUserCredentialLock is the per-(org, user) lock every writer of a
	// user's Linear credential holds: the identity handlers, the member
	// removal sweep and the user token cache.
	linearUserCredentialLock *linearoauth.UserCredentialLock
	// jiraApps owns the org_jira_apps table — per-org Atlassian OAuth app
	// registrations (the BYO-app override / local-supplied app). The settings
	// handlers read/write it; the resolver reads it (system door) to resolve
	// the app the per-user Connect flow runs against.
	jiraApps db.JiraAppsStore
	// jiraOAuthApps resolves the Atlassian OAuth app for an org (per-org
	// override → the deployment app in multi; local-supplied BYO else
	// not-configured). Backs the Jira app settings card's status + the
	// connect_available signal that the per-user Jira status endpoint returns.
	// Built in New, so it's never nil — handlers don't guard.
	jiraOAuthApps jira.OAuthAppResolver
	// jiraOAuthMinter performs the stateless Atlassian OAuth HTTP exchanges
	// for the per-user Connect flow (authorize-code exchange,
	// accessible-resources). The per-request refresh + rotation write-back
	// lives in the token cache wired into jiraResolver; this minter is the
	// connect handlers' direct seam. Built in New, never nil.
	jiraOAuthMinter *jiraoauth.Minter
	// jiraTokenCache is the per-user Cloud OAuth access-token cache backing
	// jiraResolver's OAuth branch. Retained on the Server (not just handed to
	// the resolver) so the connect callback + a paste-over-OAuth re-bind can
	// Invalidate the stale cached token — mirrors how ghTokenCache is
	// reachable via onInstallationTokensInvalid. Built in New, never nil.
	jiraTokenCache *jiraoauth.TokenCache
	// Change callbacks accept the orgID of the tenant whose integration
	// creds just rotated, so the closure can re-resolve via SecretStore.
	// Local mode always passes runmode.LocalDefaultOrgID; multi-mode
	// handlers thread the request's orgID through so the callback
	// can't fire one org's poller restart with another org's PAT.
	onGitHubChanged func(orgID string) // GitHub creds/access changed — evict the reachable-repo cache + re-due the org's GitHub poll (profiling is driven by the system:poll "profiler" subscriber, not here)
	onJiraChanged   func(orgID string) // Jira config changed — restart Jira poller only
	onLinearChanged func(orgID string) // a team's armed Linear rules changed — re-due the org's Linear poll
	// onSourcesChanged fires after an org admin pauses or resumes one event
	// source. It carries the kind because both reactions are per-source: the
	// router's disabled-source gate is invalidated for this org (in process
	// when this pod runs the brain, over the tf_ctl relay otherwise), and a
	// resumed polled source is re-dued so it starts again on the next wake
	// rather than after a full interval. Nil until SetOnSourcesChanged runs.
	onSourcesChanged func(orgID, kind string)
	// onMemoryOwed fires after a boundary this server stamped ends a
	// conversation, so the brain settles the memory it may owe now rather than
	// at the next backstop sweep. It carries the conversation because that is
	// what is owed against; the empty-id form is the org-wide re-kick a
	// configuration save publishes, when the setting the last attempts failed
	// on may have just been fixed.
	//
	// The callback relays — in process when this pod runs the brain, over
	// tf_ctl otherwise — so a handler served by a standby control pod reaches
	// the holder that actually generates. Nil until SetOnMemoryOwed runs, and
	// nil is survivable: the doorbell only ever shortens the wait, it is never
	// the thing that makes the memory land.
	onMemoryOwed  func(orgID, conversationID string)
	scorerTrigger func(orgID string) // invoked after non-poll task creation (e.g. carry-over) to kick the per-org scorer immediately
	// profilerTrigger kicks the per-org repo-profiling manager. force=true
	// bypasses the 3-day TTL — the explicit "Re-profile" button and a
	// repo-set change both want an immediate re-profile rather than waiting
	// out a poll interval. Nil until SetProfilerTrigger runs.
	profilerTrigger func(orgID string, force bool)
	// reachTrigger kicks the per-org reachable-repo refresh manager.
	// force=true bypasses its TTL — the picker's refresh control and every
	// credential change. Nil until SetReachTrigger runs.
	reachTrigger func(orgID string, force bool)
	// dashboardBackfill seeds a bound user's trailing-window PR history into
	// the entity store so the personal dashboard isn't blank for history that
	// predates tracking. Multi-mode only — wired to the poller's
	// BackfillUserDashboard; nil in local mode, where per-cycle Phase 1b owns
	// history. kickDashboardBackfill runs it fire-and-forget, marker-guarded.
	dashboardBackfill func(ctx context.Context, orgID, userID, login, host string) error

	// bus is the in-process event bus. The GitHub webhook receiver
	// publishes verified deliveries here; nil until SetEventBus runs
	// (the receiver no-ops the publish if so). Content-event processing
	// is a downstream subscriber's concern.
	bus *eventbus.Bus
	// ingestor is the durable emit seam (events audit row + queue row, then
	// bus fan-out). nil until SetIngestor runs (wired in
	// internal/app/subsystems.go immediately after ingest.New). Backs
	// ExtensionAPI.PublishPreEnqueued — ee/ ingest fans out through here,
	// never through the raw bus, so the drain worker is nudged for the
	// queue row the ingest committed.
	ingestor *ingest.Ingestor

	// onInstallationTokensInvalid fires when an installation's already-minted
	// tokens stop being usable — a verified installation.deleted or
	// installation.suspend webhook, and the App-credential change paths — so
	// the credential resolver's per-installation token cache can drop the
	// now-dead entry. New wires it to ghTokenCache.Invalidate, so it is set on
	// every Server; SetInstallationTokensInvalidHook replaces it (tests
	// observe the firings that way), and callers still nil-guard because that
	// setter accepts a nil.
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

	// refreshGroup dedupes concurrent JWT refresh attempts per session: at
	// most one gotrue refresh runs per session ID at a time, and every waiter
	// gets that result. The key is cleared once the in-flight call returns, so
	// no per-session state accumulates over the process lifetime.
	refreshGroup singleflight.Group

	// inlineScriptHashes is the base64-encoded SHA-256 of each inline
	// <script> block in the served index.html. Populated by SetStatic;
	// the CSP middleware (withSecurityHeaders) injects them into
	// script-src as `'sha256-<hash>'` directives.
	inlineScriptHashes []string

	// githubAppRegMu serializes per-org GitHub App registration so two
	// concurrent callbacks can't both pass the existence check, both call
	// GitHub's conversion endpoint, and leave an orphan App. It is the
	// LOCAL-mode half of acquireKeyedLock — sufficient at N=1, where no second
	// process can race, but not across control pods in multi mode, where
	// acquireKeyedLock takes a Postgres session-scoped advisory lock instead.
	// Reach it only through acquireKeyedLock.
	githubAppRegMu sync.Map // map[orgID]chan struct{}, db.AcquireKeyedLock's gates

	// jiraCredentialMu is the local-mode half of the org's Jira credential
	// lock (lockJiraCredential), the same mechanism as githubAppRegMu. Reach
	// it only through lockJiraCredential.
	jiraCredentialMu sync.Map // map[orgID]chan struct{}, db.AcquireKeyedLock's gates

	// githubInstallationBindMu is the same mechanism over a different keyspace:
	// one installation on one GitHub host, rather than one org. The
	// managed bind's uniqueness check asks a question ACROSS orgs — does
	// any other org already hold this installation — which a per-org lock
	// cannot serialize, because the two racers hold different org keys.
	// Local-mode half of acquireKeyedLock; reach it only through that.
	githubInstallationBindMu sync.Map // map["<installationID> <host>"]*sync.Mutex

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

	// preAuthLimiter is the per-IP token-bucket cap on the pre-auth allowlist
	// routes. Those mounts run before any session exists, so they lack the
	// implicit "needs a valid session" bound the s.api/apiMutating routes get
	// for free; this blunts a single-source flood (e.g. SSO-discovery domain
	// enumeration). Constructed in New so it's never nil; the wrapper no-ops
	// in local mode. See ratelimit.go.
	preAuthLimiter *ipRateLimiter
	// signedWebhookLimiter is the per-IP token-bucket cap for pre-auth mounts
	// that authenticate themselves via a cryptographic signature before any
	// side effect (the Slack Events API receiver) — a separate, much
	// higher-throughput tier than preAuthLimiter's human-login budget, since a
	// legitimate sender at this tier already proves its authenticity and a
	// shared low budget would 429 it into looking like a failing endpoint.
	// Constructed in New so it's never nil; the wrapper no-ops in local mode.
	// See ratelimit.go.
	signedWebhookLimiter *ipRateLimiter
	// tokenAuthFailureLimiter is the per-IP budget the Bearer path charges
	// when a token fails to authenticate. Unlike the two above it does not
	// wrap any route: withSession consults it around the lookup itself, so a
	// working token costs nothing and only a stream of failures is bounded.
	// Constructed in New so it's never nil; the Bearer path it guards is
	// multi-mode only. See ratelimit.go.
	tokenAuthFailureLimiter *ipRateLimiter

	// readyHooks are brain-gated OnReady hooks registered by extensions
	// during installExtensions (routes()). Collected single-threaded at
	// startup — same no-lock contract as registeredExtensions — then fired
	// by StartBrainExtensionWorkers every time this pod's brain starts — at
	// TF_ROLE=all / local, that is once, at boot.
	readyHooks []func(context.Context)
	// replicaSafeHooks are OnReadyReplicaSafe hooks — the escape hatch for
	// a worker that's safe to run on every pod regardless of brain-lease
	// state. Fired once, unconditionally, by StartExtensionWorkers.
	replicaSafeHooks []func(context.Context)
	// workersStarted guards StartExtensionWorkers so a double call (e.g. a
	// test double-invoking Run) can't fire replicaSafeHooks twice.
	workersStarted sync.Once

	// version is main.Version (the release tag, or "dev" for an unreleased
	// build), threaded through for GET /readyz so an operator can confirm a
	// deploy landed without shelling into the host. Set via SetVersion; the
	// zero value "" renders as an empty string rather than panicking for any
	// Server built without it (bare test constructions).
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
	// lease, wired via SetLeaseStatus — role=control in multi mode only. Nil
	// at role=all / local, where the single process always self-holds, so
	// /readyz hard-checks the poller unconditionally and omits the `lease`
	// field; nil on an executor too, which serves a separate localhost healthz
	// instead.
	leaseStatus LeaseStatusFunc
}

// agentEnabledForTeam returns the resolved agent and whether
// team_agents.enabled is true for it *on the given team*. Delegating re-checks
// the flag at delegate time, scoped to the ACTING team rather than the org
// default — so a multi-team user delegating for team B is gated on B's bot
// setting.
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
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		a, e = tx.Agents.GetForOrg(ctx, orgID)
		if e != nil {
			return fmt.Errorf("agent lookup: %w", e)
		}
		if a == nil {
			return fmt.Errorf("no agent bootstrapped — set up the bot first")
		}
		if s.teamAgents == nil {
			// No team-agent store wired (bare test constructions): there is
			// nothing to re-check, so the delegate proceeds.
			enabled = true
			return nil
		}
		if teamID == "" {
			// No team supplied (teamless org). Production installs always have
			// a team (multi-mode org provisioning; local-mode baseline
			// migration), so this is a bootstrap bug — surface as disabled
			// rather than minting a wrong-team row.
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

// New creates a server over the given database + the full per-resource store
// bundle, and registers every route. The Server holds individual store fields
// rather than the db.Stores struct so a handler names the one store it needs;
// the constructor unpacks the bundle once. The raw *sql.DB is retained for the
// few paths that issue SQL directly.
func New(database *sql.DB, stores db.Stores) *Server {
	s := &Server{
		db:                 database,
		prompts:            stores.Prompts,
		swipes:             stores.Swipes,
		agents:             stores.Agents,
		teamAgents:         stores.TeamAgents,
		users:              stores.Users,
		blueprints:         stores.Blueprints,
		tasks:              stores.Tasks,
		repos:              stores.Repos,
		events:             stores.Events,
		secrets:            stores.Secrets,
		teams:              stores.Teams,
		orgs:               stores.Orgs,
		jiraRules:          stores.JiraStatusRules,
		linearRules:        stores.LinearTeamRules,
		githubApps:         stores.GitHubApps,
		reachableRepos:     stores.ReachableRepos,
		githubDeliveries:   stores.GitHubDeliveries,
		githubPendingBinds: stores.GitHubPendingBinds,
		jiraApps:           stores.JiraApps,
		authEvents:         stores.AuthEvents,
		tx:                 stores.Tx,
		az:                 authz.New(database, stores.Tx),
		fleetQueue:         stores.ConversationQueue,
		allStores:          stores,
		mux:                &routeMux{ServeMux: http.NewServeMux()},
		ws:                 websocket.NewHub(),
	}
	// Per-IP rate limiters for the pre-auth allowlist, the signed-webhook
	// tier, and API-token authentication failures. Built here (not injected)
	// so a Server is always usable without external wiring; every consumer
	// that reads them is inert in local mode.
	s.preAuthLimiter = newIPRateLimiter(preAuthRatePerSec, preAuthBurst, rateLimitBucketTTL)
	s.signedWebhookLimiter = newIPRateLimiter(signedWebhookRatePerSec, signedWebhookBurst, rateLimitBucketTTL)
	s.tokenAuthFailureLimiter = newIPRateLimiter(tokenAuthFailureRatePerSec, tokenAuthFailureBurst, rateLimitBucketTTL)
	// GitHub credential resolver + its installation-token cache, built from
	// the same stores. Constructed here (not injected) so a Server is always
	// usable without external wiring — tests that call New directly get a
	// working resolver too. A verified installation.deleted or
	// installation.suspend webhook drops the dead token from the cache via
	// onInstallationTokensInvalid.
	s.ghTokenCache = ghclient.NewMemoryTokenCache()
	// The deployment App comes off the deployment env, and only in multi mode —
	// local has no shared App key to ship, so this resolves to the zero App
	// there and every org brings its own credential.
	//
	// Read ONCE, here, and handed to both consumers: the resolver, which mints
	// tier-2 installation tokens for an org whose credential class says it rides
	// the shared App, and the bind ceremony, which needs the client secret for
	// the OAuth exchange that identifies the person completing an installation.
	// Two independent env reads would be two answers that could disagree.
	//
	// A malformed configuration is logged and left as the zero App rather than
	// failing the boot: a deployment whose orgs all bring their own App must
	// still start, and what an absent deployment App means everywhere
	// downstream is "refuse every managed operation", which is the failing-closed
	// direction.
	deploymentApp, err := githubapp.DeploymentAppFromEnv()
	if err != nil {
		serverLog.Error("read deployment github app from the environment failed; managed-class orgs will resolve nothing and no organization can bind one",
			"error", err)
		deploymentApp = githubapp.DeploymentApp{}
	}
	s.deploymentApp = deploymentApp
	s.ghResolver = ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, s.ghTokenCache,
		ghclient.WithDeploymentApp(deploymentApp))
	// Atlassian OAuth app resolver: per-org override → the deployment app
	// (multi) / local-supplied (local). The deployment app is read from the
	// deployment env, and only in multi mode — local has no deployment
	// default, so DeploymentOAuthAppFromEnv returns the zero app there and the
	// resolver relies on the org's BYO row.
	s.jiraOAuthApps = jira.NewOAuthAppResolver(stores.JiraApps, stores.Secrets, jira.DeploymentOAuthAppFromEnv())
	// Cloud OAuth minter + per-user access-token cache. The cache reads the
	// stored refresh token, mints an access token, and writes the rotated
	// refresh token back (PutUserSystem) — the structural difference from the
	// static PAT/API-token creds. It's the OAuth source the resolver delegates
	// its AuthMethodCloudOAuth branch to.
	s.jiraOAuthMinter = jiraoauth.NewMinter()
	s.jiraTokenCache = jiraoauth.NewTokenCache(s.jiraOAuthMinter, s.jiraOAuthApps, stores.Secrets)
	// Jira write-actor resolver: ForSystem (org/bot cred) + ForUser (acting
	// user's cred, incl. the Cloud OAuth mint path). Constructed here like
	// ghResolver so a Server is always usable without external wiring.
	s.jiraResolver = jira.NewResolverWithOAuth(stores.Secrets, stores.Orgs,
		eventsource.ResolverGate(stores.OrgEventSources, eventsource.KindJira), s.jiraTokenCache)
	// Linear credential resolver. The picker and the team-rules write gate
	// read the org's catalog through ForSystem, whichever shape the org's
	// service credential takes; an installed org's access token comes from the
	// token cache, which refreshes it off the install's rotating refresh
	// token. ForUser resolves a user's pasted key directly and a Connected
	// user's access token through the user token cache.
	s.linearOAuthApps = linear.NewOAuthAppResolver(stores.LinearApps, stores.Secrets, linear.DeploymentOAuthAppFromEnv())
	s.linearOAuthMinter = linearoauth.NewMinter()
	s.linearInstalls = stores.LinearInstalls
	s.linearCredentialLock = linearoauth.NewCredentialLock(database)
	s.linearUserCredentialLock = linearoauth.NewUserCredentialLock(database)
	s.linearResolver = linear.NewResolverWithOAuth(stores.Secrets, stores.Orgs,
		eventsource.ResolverGate(stores.OrgEventSources, eventsource.KindLinear),
		linearoauth.NewTokenCache(s.linearOAuthMinter, s.linearOAuthApps, stores.Secrets, stores.LinearInstalls, s.linearCredentialLock),
		linearoauth.NewUserTokenCache(s.linearOAuthMinter, s.linearOAuthApps, stores.Secrets, stores.Tx, s.linearUserCredentialLock))
	s.validateLinear = auth.ValidateLinear
	s.onInstallationTokensInvalid = func(orgID, installationID string) {
		s.ghTokenCache.Invalidate(orgID, installationID)
	}
	// The invoke actions of notification kinds whose effect is a write this
	// server owns.
	kinds.LinearTeamUnreadable.HandleAction("untrack", s.untrackUnreadableLinearTeam)
	kinds.MemoryGenerationFailing.HandleAction("retry", s.retryFailingMemoryGeneration)
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

// ListenAndServeContext starts the HTTP server on the given address and shuts
// it down gracefully when ctx is cancelled (SIGINT/SIGTERM at the process
// boundary). The mux is wrapped in withSecurityHeaders so every response
// carries the standard set (HSTS conditionally, CSP, X-Frame-Options, etc.),
// and in otelhttp outside that — see tracedHandler. Returns nil on a clean
// ctx-driven shutdown; a non-ErrServerClosed listen error otherwise.
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
	s.mux.mount(pattern, mountAPI, s.withSession(h))
}

// mounts hands a handler subpackage the two mounts above.
func (s *Server) mounts() httpx.Mounts {
	return httpx.Mounts{API: s.api, APIMutating: s.apiMutating}
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
	s.mux.mount(pattern, mountAPIMutating, s.withCSRFOriginCheck(s.withSession(h)))
}

// How a route was mounted: through api, through apiMutating, or straight on
// the mux with no session or CSRF wrap.
const (
	mountAPI         = "api"
	mountAPIMutating = "apiMutating"
	mountRaw         = "raw"
)

// routeMount is one pattern mounted on the server and how it was mounted.
type routeMount struct {
	pattern string
	via     string
}

// routeMux is the server's ServeMux, recording every mount made on it. The
// route tests audit that record — every raw /api/* mount must be on the
// pre-auth allowlist, every wrapped one must 401 without a session — so they
// see each route the server serves wherever it was registered, rather than
// the mounts one function's source happens to spell out. Mounting happens
// during construction, before the server serves, so the slice needs no lock.
type routeMux struct {
	*http.ServeMux
	mounts []routeMount
}

// Handle and HandleFunc are the raw mounts: anything that reaches the mux
// without going through api or apiMutating is recorded as raw.
func (m *routeMux) Handle(pattern string, h http.Handler) { m.mount(pattern, mountRaw, h) }

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.mount(pattern, mountRaw, http.HandlerFunc(h))
}

func (m *routeMux) mount(pattern, via string, h http.Handler) {
	m.mounts = append(m.mounts, routeMount{pattern: pattern, via: via})
	m.ServeMux.Handle(pattern, h)
}

func (s *Server) routes() {
	// Pre-auth allowlist — these /api/* (and /auth/v1/*, /) routes
	// intentionally DO NOT go through s.api / s.apiMutating because
	// they must run before any session exists, or have no identity
	// dependency at all. Any addition here must be deliberate; the
	// routes_coverage_test guards against accidental wrap-stripping.
	// The ones that belong to a domain (the webhook receivers, the invite
	// preview, the managed-bind callback) are mounted by that domain's
	// register method; the rest are mounted here.
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
	//   GET  /readyz                    — readiness probe: DB +
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
	//   POST /api/webhooks/github          — the deployment App's webhook
	//        receiver; verifies against the one deployment secret from the
	//        environment before any org is known, then routes the delivery
	//        by the installation's binding. Multi mode only (404 in local).
	//   GET  /api/invites/preview          — invite-token preview; the
	//        recipient hasn't authenticated yet, so this can't gate on a
	//        session. Runs on the admin pool; the token is the bearer secret.
	//   GET  /api/github/managed/callback  — the deployment-App bind
	//        callback. Mounted raw and applies withSession ITSELF, to the one
	//        branch that needs it: a caller carrying the bind cookie is
	//        completing a ceremony and about to write a credential, so it is
	//        authenticated as usual, while a caller with no cookie installed
	//        the App from its public page on GitHub and typically has no
	//        session at all — that branch resolves no identity, reads nothing
	//        and writes nothing, so a blanket 401 in front of it would only
	//        dead-end the one person it exists for.
	//   POST /api/sso/discover             — identifier-first login lookup;
	//        the visitor is anonymous (pre-login), so it can't gate on a
	//        session. No side effect, admin-pool read, privacy-safe reply
	//        (mounted by ee/sso — see the SSO note at installExtensions).
	//   /auth/v1/                        — GoTrue reverse proxy; auth
	//        happens upstream, not in our middleware.
	//   /api/                            — JSON 404 for any unmatched
	//        /api/* path; no identity dependency.
	//   /                                — SPA fallback; static-file
	//        serving with no identity dependency.
	//
	// IP rate limiting: the interactive pre-auth mounts — oauth start, logout,
	// invite preview, and SSO discovery — are wrapped in
	// s.preAuthRateLimit so an anonymous caller can't flood them (e.g.
	// discovery domain enumeration). The cap is declared at the mount,
	// not sprinkled across handlers; it no-ops in local mode.
	// Deliberately NOT wrapped: the OAuth callback (already bounded per-flow
	// by its HMAC-signed PKCE state cookie + single-use IdP code, so an IP cap
	// adds ~no protection — and a 429 mid-OAuth on a shared NAT would break a
	// top-level navigation for no gain), the managed-bind callback (same
	// shape, and its pre-auth branch renders a fixed page without a single
	// read — there is no work behind it to flood), /api/health (platform liveness probes
	// hit it often), /api/config (the AuthGate boot read), the /auth/v1/
	// GoTrue proxy (rate-limited upstream), and the SPA fallback (every static
	// asset).
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
	// Readiness probe — bare path (not under /api/*), matching the universal
	// k8s/ALB probe convention; Go 1.22's ServeMux matches this exact literal
	// ahead of the "/" SPA catch-all regardless of registration order.
	// Pre-auth for the same reason as /api/health above. See
	// readyz_handler.go.
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
	s.registerAuthRoutes()
	s.registerFleetRoutes()
	s.registerAPITokenRoutes()
	s.registerSettingsRoutes()
	s.registerNotificationRoutes()
	s.registerOrgRoutes()
	s.registerTeamActivityRoutes()
	s.registerTeamPRRoutes()
	s.registerTrackedSetRoutes()
	s.registerTeamKnowledgeRoutes()
	s.registerTeamMemberRoutes()
	s.registerOrgMemberRoutes()
	s.registerInviteRoutes()
	s.registerTaskRoutes()
	s.registerAgentRoutes()

	// Websocket: wrapped via s.api so the handshake sees claims in
	// r.Context() (sentinel in local mode, real values in multi).
	// handleWS pulls (userID, orgID) out and threads them into the
	// hub's HandleWS so the per-connection scoping in pkg/websocket
	// can filter Broadcast fanout without importing internal/server.
	// Treated as GET-equivalent — no CSRF wrap.
	s.api("GET /api/ws", s.handleWS)
	s.registerDashboardRoutes()
	s.registerPromptImportRoutes()
	s.registerRepoRoutes()
	s.registerSettingsHandlerRoutes()
	s.registerReachabilityRoutes()
	s.registerJiraRoutes()
	s.registerLinearRoutes()
	s.registerStockRoutes()
	s.registerFactoryRoutes()
	s.registerEventHandlerRoutes()
	s.registerGitHubRoutes()
	s.registerSourceRoutes()

	// The domains that live in their own subpackages register through the
	// same two mounts, handed over as httpx.Mounts.
	m := s.mounts()
	models.New(s.az, s.tx, func() models.Prober { return s.modelProber }).Register(m)
	deliverables.New(s.tx, s.ws, s.ghResolver,
		func() *delegate.Spawner { return s.spawner },
		func() *reconcile.Reconciler { return s.reconciler },
		func() string {
			if s.deployCfg == nil {
				return ""
			}
			return s.deployCfg.publicURL
		},
	).Register(m)
	usage.New(s.tx, s.az, s.allStores.ConversationQueue).Register(m)
	teams.New(s.tx, s.az, s.allStores, func() *delegate.Spawner { return s.spawner }, s.kickMemoryOwed).Register(m)
	prompts.New(s.db, s.tx, s.az).Register(m)
	blueprints.New(s.tx, s.az, func() *delegate.Spawner { return s.spawner }).Register(m)
	work.New(s.az, s.allStores.WorkKinds).Register(m)
	marketplace.New(s.tx, s.az).Register(m)
	avatars.New(s.tx).Register(m)

	// SSO (connection / domains / break-glass admin surface, the verify-
	// before-enforce test-start, and the identifier-first discovery probe) is
	// an Enterprise Edition feature: ee/sso mounts these routes through the
	// route-extension seam (installExtensions, gated on the `sso` entitlement)
	// and registers the LoginExtension that drives SAML start + the callback's
	// enforcement/JIT/test forks. Core holds no SSO symbols.
	//
	// Registered server extensions (Enterprise Edition, ee/) mount their
	// routes here, each gated on its license feature. No-op in a community
	// build / unlicensed deployment. Mounted before the SPA fallback; Go's
	// ServeMux resolves by longest pattern, so specific /api/* extension
	// routes take precedence over "/" regardless.
	s.installExtensions()

	// Unknown /api/* → JSON 404. Without this the greedy "/" SPA fallback
	// below answers any unmatched path — a stray or typo'd /api/* included —
	// with 200 + index.html, which masks client typos and reads as success to
	// an API consumer. Go's ServeMux resolves by the most-specific pattern, so
	// a concrete "GET /api/whatever" still wins over this prefix and only an
	// unregistered /api/* subtree falls here. The handler is method-agnostic,
	// so a wrong-method request to a known path (DELETE /api/health, say)
	// lands here as a JSON 404 rather than a 405.
	s.mux.HandleFunc("/api/", s.handleAPINotFound)

	// Frontend: the embedded SPA, falling back to index.html so client-side
	// routing resolves.
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

// handleFrontend serves the embedded React SPA. Non-file requests fall back to
// index.html so that client-side routing works.
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

	if _, err := fs.Stat(s.static, path); err == nil {
		http.ServeFileFS(w, r, s.static, path)
		return
	}

	// Anything that isn't a real file is a client-side route.
	http.ServeFileFS(w, r, s.static, "index.html")
}

// SetStatic sets the embedded frontend filesystem, and hashes every inline
// <script> block in index.html so the CSP middleware can allowlist them via
// `'sha256-...'` — script-src stays tight with no frontend change.
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

// SetTeamKB wires the team knowledge-base seam and its websocket notifier.
// Called once at startup, after the blob store the multi-mode backend is built
// over has been resolved.
//
// The notifier's audience resolution is the mode branch: multi resolves a
// private-root change to the team's members at emit time, while local passes
// nil and broadcasts org-wide, because N=1 has one user and no team boundary
// to mirror.
func (s *Server) SetTeamKB(kb kbstore.KB) {
	s.teamKB = kb
	var members knowledgeevent.MembersFunc
	if runmode.Current() == runmode.ModeMulti {
		members = s.teams.MemberIDsSystem
	}
	s.teamKBNotifier = knowledgeevent.NewNotifier(s.ws, members)
}

// SetOnGitHubChanged registers a callback for GitHub config changes (creds,
// URL, repos). The callback re-dues the org's GitHub poll so the new
// credential/repo set applies on the next wake; repo profiling is no longer
// triggered here — it's driven by the system:poll "profiler" subscriber off
// that poll's completion. The orgID is the tenant whose creds changed —
// closure re-resolves via SecretStore.
//
// The registered callback is wrapped so a FORCED reachable-repo refresh is
// kicked for the org alongside the re-due. A creds rotation, an App install,
// or a host repoint moves which repositories the org can reach without moving
// the timestamp the mirror's TTL reads, so the mirror is wrong in fact while
// looking fresh — the one staleness a TTL cannot detect, which is why the
// explicit force exists at all.
//
// The refresh is out of band by construction (it is a Manager trigger, not a
// call), so this stays as cheap as the eviction it replaces. A repo-TRACKING
// save also lands here — tracking is not reachability, so that refresh is
// redundant — and it is left in rather than special-cased: it is one
// enumeration per human Save gesture, and splitting the hook into "creds
// changed" and "repos changed" variants would put the burden of picking the
// right one on six call sites.
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

// SetOnMemoryOwed registers the memory doorbell. See the field.
func (s *Server) SetOnMemoryOwed(fn func(orgID, conversationID string)) {
	s.onMemoryOwed = fn
}

// kickMemoryOwed rings the doorbell for one ended conversation, or — with an
// empty id — for the whole org. The single call site shape for every stamper in
// this package and for the configuration saves, so a caller never has to
// remember the nil check.
//
// Synchronous: the relay is a nil-safe channel nudge on the brain and one
// pg_notify off it, neither of which is worth a goroutine per boundary. The
// generation it triggers is the provisioner's own detached work.
func (s *Server) kickMemoryOwed(orgID, conversationID string) {
	if s.onMemoryOwed == nil {
		return
	}
	s.onMemoryOwed(orgID, conversationID)
}

// retryFailingMemoryGeneration is kinds.MemoryGenerationFailing's "Retry now",
// the org-wide doorbell a background jobs model save rings. It reports the
// condition unresolved, because the attempts the re-sweep makes decide that.
func (s *Server) retryFailingMemoryGeneration(_ context.Context, call notify.ActionCall[kinds.MemoryFailure]) (bool, error) {
	s.kickMemoryOwed(call.OrgID, "")
	return false, nil
}

// SetOnJiraChanged registers a callback for Jira config changes. This restarts
// only the Jira poller. See SetOnGitHubChanged for orgID semantics.
func (s *Server) SetOnJiraChanged(fn func(orgID string)) {
	s.onJiraChanged = fn
}

// SetOnLinearChanged registers the callback for a change to an org's armed
// Linear configuration. See the field.
func (s *Server) SetOnLinearChanged(fn func(orgID string)) {
	s.onLinearChanged = fn
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
// keying a read differently from the write is how a mirror reads as empty for
// an org that has one.
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
// background managers. kickReachRefresh nil-checks it, so a picker read on
// such a Server serves whatever the mirror holds and asks for nothing.
func (s *Server) SetReachTrigger(fn func(orgID string, force bool)) {
	s.reachTrigger = fn
}

// kickReachRefresh asks for a reachable-mirror refresh, out of band. It never
// blocks the caller and never reports failure: a refresh that does not happen
// costs a staler mirror on the next read, which is the same cost as the TTL
// not having elapsed yet, and a request has nothing useful to do with the
// error.
func (s *Server) kickReachRefresh(orgID string, force bool) {
	if s.reachTrigger == nil || orgID == "" {
		return
	}
	s.reachTrigger(orgID, force)
}

// SetReconciler registers the shared deliverable Reconciler that backs the Tier-2
// conversation-scoped refresh endpoint — the same instance the background
// Tier-1 Manager runs, so a foreground refresh and a background cycle apply
// the identical reconcile path. Nil until wired; the endpoint 503s until then.
func (s *Server) SetReconciler(rc *reconcile.Reconciler) {
	s.reconciler = rc
}

// SetBedrockRoleResolver wires the Bedrock role-mode setup + connect-probe
// resolver. Called once at startup in multi mode with the shared
// *llmcred.Resolver; left nil in local mode (no ambient AWS SDK), where the
// role-setup endpoint reports the method is control-service only.
func (s *Server) SetBedrockRoleResolver(r bedrockRoleResolver) {
	s.bedrockRole = r
}

// SetModelProber wires the model-availability prober. Called once at startup
// on every serving process, in either mode — the transport differs (a direct
// call in multi, the agent runtime in local) but the question and the verdicts
// do not. Nil until wired: the test routes report the deployment cannot probe.
func (s *Server) SetModelProber(p models.Prober) {
	s.modelProber = p
}

// SetDashboardBackfiller registers the per-user dashboard-history backfill
// (the poller's BackfillUserDashboard). Wired in multi mode only — local mode
// leaves it nil and relies on the per-cycle Phase 1b backfill, so
// kickDashboardBackfill no-ops there.
func (s *Server) SetDashboardBackfiller(fn func(ctx context.Context, orgID, userID, login, host string) error) {
	s.dashboardBackfill = fn
}

// kickDashboardBackfill fires a one-shot dashboard-history backfill for a
// bound (user, host) and returns immediately — the work runs detached so it
// never blocks the request (identity bind or dashboard read) that triggered
// it. The backfiller is marker-guarded downstream, so repeated kicks for an
// already-backfilled identity are cheap no-ops. No-op when unwired (local
// mode) or when the identity isn't fully resolved.
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

// WSKicker is the multi-mode cross-pod session-kick publisher;
// *wsbackplane.Backplane satisfies it directly. Every CloseUserConnections
// call site closes THIS pod's local sockets first, then calls PublishKick so
// every OTHER control pod does the same for its own — see
// internal/pgnotify/wsbackplane's PublishKick doc for why that ordering is what keeps
// the kick from echoing.
type WSKicker interface {
	PublishKick(ctx context.Context, userID, sid, orgID string, code int, reason string)
}

// SetWSBackplane wires the multi-mode cross-pod kick publisher. Wired
// post-construction in internal/app, mirroring SetEventBus; nil — the
// default, and always the case in local mode — leaves every kick call site
// local-only.
func (s *Server) SetWSBackplane(b WSKicker) {
	s.wsBackplane = b
}

// SetVersion records main.Version (the release tag, or "dev") for GET
// /readyz. Wired in internal/app/httpserver.go's buildServer, right after
// server.New, in both modes.
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetMigrationsOK records that db.Migrate completed successfully at boot, for
// GET /readyz's "migrations" hard check. Called with true in
// internal/app/app.go's New, right after buildServer succeeds — by that point
// openStores has already run db.Migrate to completion (New would have returned
// its error otherwise), so this simply makes that already-true fact visible to
// the handler. See the migrationsOK field doc for why the check exists despite
// always passing today.
func (s *Server) SetMigrationsOK(ok bool) {
	s.migrationsOK = ok
}

// SetPollerManager wires the poller Manager's Health() snapshot into GET
// /readyz: the poller-alive hard check plus the per-org poll-staleness soft
// signal. Takes the bound method value rather than the whole *poller.Manager
// so readyz_handler.go depends on one function, not the poller package's full
// surface (PollSoon, RestartAll, etc).
func (s *Server) SetPollerManager(healthFn func(ctx context.Context) poller.HealthSnapshot) {
	s.pollerHealth = healthFn
}

// SetLeaseStatus wires the background-brain lease elector's status into GET
// /readyz: the `lease` field, and the switch between the holder's poller
// hard-check and a standby's informational "standby" report. Called only for
// role=control (internal/app); leaving it unset (role=all / local, or a bare
// test construction) makes the handler treat this pod as the holder and omit
// the `lease` field — the single-process contract.
func (s *Server) SetLeaseStatus(fn LeaseStatusFunc) {
	s.leaseStatus = fn
}

// SetIngestor wires the durable ingest seam so ExtensionAPI.PublishPreEnqueued
// can delegate to it. Wired in internal/app/subsystems.go immediately after
// ingest.New — before that point (and in any test wiring that skips it),
// ingestor is nil and PublishPreEnqueued logs and skips the fan-out.
func (s *Server) SetIngestor(i *ingest.Ingestor) {
	s.ingestor = i
}

// SetInstallationTokensInvalidHook REPLACES the callback fired when an
// installation's minted tokens stop being usable — a verified
// installation.deleted or installation.suspend webhook, and the App-credential
// change paths. New already wires that callback to the resolver's own token
// cache, so this is an override rather than the wiring: whatever it installs
// takes over, and passing nil disables the invalidate entirely.
func (s *Server) SetInstallationTokensInvalidHook(fn func(orgID, installationID string)) {
	s.onInstallationTokensInvalid = fn
}

// MarkJiraRestarted records the moment orgID's Jira poller was restarted.
// Clears readiness so jiraPollReady reports false until a completion event
// arrives. Call this before kicking off a Jira poller restart.
//
// Backed by the org-scoped poll_readiness table, not a process-local field:
// the pod that restarts the poller (any control pod handling the config-save
// request) is not necessarily the pod that runs it, and is not necessarily the
// pod that later serves /api/jira/stock — see jiraPollReady. Best-effort: a
// write failure is logged and the request proceeds; the worst case is a stale
// readiness read, not a broken save.
func (s *Server) MarkJiraRestarted(ctx context.Context, orgID string) {
	if err := s.allStores.PollReadiness.MarkRestarted(ctx, orgID, "jira"); err != nil {
		serverLog.Warn("mark jira poll restarted failed", "org", orgID, "error", err)
	}
}

// MarkGitHubRestarted is MarkJiraRestarted for GitHub: it clears orgID's
// GitHub readiness until the poller's next completion, so the last-poll time
// the team activity page shows is never one taken on a GitHub host or under a
// credential the org has since replaced. Best-effort for the same reasons.
func (s *Server) MarkGitHubRestarted(ctx context.Context, orgID string) {
	if err := s.allStores.PollReadiness.MarkRestarted(ctx, orgID, "github"); err != nil {
		serverLog.Warn("mark github poll restarted failed", "org", orgID, "error", err)
	}
}

// MarkLinearRestarted is MarkJiraRestarted for Linear: it clears orgID's
// Linear readiness until the poller's next completion, which is what the
// team activity page's last-poll time and the "config took effect" toast
// read. Best-effort for the same reasons.
func (s *Server) MarkLinearRestarted(ctx context.Context, orgID string) {
	if err := s.allStores.PollReadiness.MarkRestarted(ctx, orgID, "linear"); err != nil {
		serverLog.Warn("mark linear poll restarted failed", "org", orgID, "error", err)
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
