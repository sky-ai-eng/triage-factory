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
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiraoauth"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Server is the main HTTP server for Triage Factory.
type Server struct {
	db           *sql.DB
	prompts      db.PromptStore
	swipes       db.SwipeStore
	agents       db.AgentStore           // resolves the org's agent for claim stamps
	teamAgents   db.TeamAgentStore       // re-checks team_agents.enabled on swipe-delegate / factory-delegate
	users        db.UsersStore           // display_name + Jira binding on the user row; host-scoped GitHub identity via user_github_identities
	blueprints   db.BlueprintStore       // used by event-handler + project test fixtures
	tasks        db.TaskStore            // task lifecycle, claim, queue + factory snapshot reads
	agentRuns    db.ConversationStore    // agent run lifecycle + transcript
	repos        db.RepoStore            // repo_profiles CRUD for repos/settings/projects handlers and curator pinned-repo materialization
	projects     db.ProjectStore         // projects CRUD for projects/curator/backfill/project_entities handlers
	curatorStore db.CuratorStore         // curator view of conversations/messages/claims — handler-side System writes (cancel release, pending-context producer) go through here; claims-bound reads ride tx.Curator
	events       db.EventStore           // events audit log Record/Latest for stock carry-over + factory drag-to-delegate
	taskMemory   db.TaskMemoryStore      // conversation_memory writes (human verdict capture on review/PR submit, swipe-discard cleanup)
	secrets      db.SecretStore          // canonical credential read/write path — local-mode keychain, multi-mode vault
	teams        db.TeamsStore           // resolves the request org's default team for handlers that synthesize team-scoped rows (tasks, projects, prompts)
	orgs         db.OrgsStore            // per-org settings (GitHub/Jira base URLs, poll intervals, clone protocol) post-internal/config deletion
	jiraRules    db.JiraStatusRulesStore // per-team Jira status rules (replaces the deleted config.Jira.Projects view)
	githubApps   db.GitHubAppsStore      // per-org GitHub App registrations (manifest flow)
	authEvents   db.AuthEventStore       // TFAC-76: SOC2 authentication audit log of record — written best-effort via recordAuthEvent at the auth write-sites
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
	curator     *curator.Curator
	// kb is the multi-mode knowledge-base blob seam. Wired via
	// SetKBStore after construction; nil in local mode, where the KB handlers
	// stay on their byte-identical on-disk path and never consult it. The
	// handlers gate on runmode.ModeMulti, so a nil here in local mode is never
	// dereferenced.
	kb *kbstore.Store
	// reconciler backs the Tier-2 run-scoped artifact refresh endpoint
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
	// fleetQueue backs the GET /api/fleet/queue view: per-org run-queue shares
	// (active/queued + cap). Satisfied by the RunQueue store; a narrow
	// interface so the handler test can inject canned shares.
	fleetQueue fleetQueueReader
	// ghResolver picks the right GitHub credential (org App installation
	// token → PAT) per request, given the org + target account. The per-repo
	// handler operations migrated off the old process-global PAT client —
	// review diff/submit, pending-PR submit, branches, dashboard, and the
	// project-bundle probe — resolve through it, and there is no longer a
	// process-global PAT client. (A few handlers still build a request-scoped
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
	// via onInstallationRemoved. Built in New, never nil.
	jiraTokenCache *jiraoauth.TokenCache
	// Change callbacks accept the orgID of the tenant whose integration
	// creds just rotated, so the closure can re-resolve via SecretStore.
	// Local mode always passes runmode.LocalDefaultOrgID; multi-mode
	// handlers thread the request's orgID through so the callback
	// can't fire one org's poller restart with another org's PAT.
	onGitHubChanged func(orgID string) // GitHub creds/access changed — evict the reachable-repo cache + re-due the org's GitHub poll (profiling is driven by the system:poll "profiler" subscriber, not here)
	onJiraChanged   func(orgID string) // Jira config changed — restart Jira poller only
	// kbChangedDoorbell rings the tf_ctl "kb_changed" cross-pod nudge after a
	// KB upload/delete/project-delete so the home executor materializes the
	// panel write into a live session's dir. Wired only in multi mode; nil in
	// local (no cross-pod plane), where the handlers skip it.
	kbChangedDoorbell func(op, orgID, projectID string)
	scorerTrigger     func(orgID string) // invoked after non-poll task creation (e.g. carry-over) to kick the per-org scorer immediately
	// profilerTrigger kicks the per-org repo-profiling manager. force=true
	// bypasses the 3-day TTL — the explicit "Re-profile" button and a
	// repo-set change both want an immediate re-profile rather than waiting
	// out a poll interval. Nil until SetProfilerTrigger runs.
	profilerTrigger func(orgID string, force bool)
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

	// projectMutexes serializes PATCH-style read-merge-write
	// operations per project ID so two concurrent autosaves from
	// different widgets (e.g. pinned-repos editor and tracker
	// picker) can't lost-update each other. SQLite serializes
	// individual writes via MaxOpenConns=1, but that's not enough
	// here — handler A reads pre-A state, handler B reads pre-A
	// state, A writes, B writes B's merge over pre-A state, and
	// A's contribution is lost. Holding the per-project mutex
	// across the read+write window closes that hole.
	//
	// TFAC-579: this map is now only the LOCAL-mode half of
	// acquireKeyedLock — sufficient at N=1 (there's no second process to
	// race), but not across control pods in multi mode, where
	// acquireKeyedLock instead takes a Postgres session-scoped advisory
	// lock. Both call sites go through acquireKeyedLock; nothing reaches
	// into this map directly anymore.
	projectMutexes sync.Map // map[string]*sync.Mutex

	// githubAppRegMu serializes per-org GitHub App registration so
	// two concurrent callbacks can't both pass the existence check,
	// both call GitHub's conversion endpoint, and leave an orphan
	// App. Same sync.Map pattern as projectMutexes, and the same
	// TFAC-579 local-mode-only caveat: acquireKeyedLock backs multi mode
	// with a real cross-pod advisory lock instead.
	githubAppRegMu sync.Map // map[orgID]*sync.Mutex

	// reachableRepoMu guards reachableRepoCache — the in-process
	// enumeration cache the team-repos write gate consults before
	// re-enumerating the org. The picker
	// (handleGitHubRepos) warms it on the way out; the immediate-next
	// PUT /api/settings/team/{id}/repos validates against this set in
	// ~µs instead of paying the full ListUserRepos cost a second time.
	// Entries are TTL-bounded (reachableCacheTTL) and evicted per-org
	// when GitHub creds/installations rotate (SetOnGitHubChanged).
	reachableRepoMu    sync.RWMutex
	reachableRepoCache map[string]reachableRepoEntry // key: orgID\x00userID

	// webhookSecretMu guards webhookSecretCache and its sweep clock — the
	// short-TTL per-org cache of the secret the pre-auth GitHub webhook
	// receiver verifies deliveries against. Resolving it costs a settings
	// read, a registration read, and a vault read, all of them spent before
	// the signature is checked, so an unauthenticated flood would otherwise
	// pay them per request. Entries (positives and negatives alike) expire
	// after webhookSecretTTL and are dropped explicitly when the App
	// lifecycle rotates or tears down the secret. See
	// github_webhook_secret.go.
	webhookSecretMu    sync.Mutex
	webhookSecretCache map[string]webhookSecretEntry // key: orgID
	webhookSecretSweep time.Time                     // last expiry sweep, for the once-per-TTL gate

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

// reachableRepoEntry is one cached picker enumeration: the lowercased
// "owner/repo" slug set the user's GitHub credential can reach, plus the
// wall-clock instant the entry stops being trusted.
type reachableRepoEntry struct {
	set       map[string]struct{}
	expiresAt time.Time
}

// reachableCacheTTL bounds how long a picker enumeration is trusted to satisfy
// a write. Long enough to cover the realistic "open picker → think → click
// Continue" window; short enough that a stale enumeration can't mask a
// credential revocation for more than a few minutes (eviction on
// SetOnGitHubChanged handles the explicit-rotation case immediately).
const reachableCacheTTL = 3 * time.Minute

// reachableCacheKey namespaces the cache per (orgID, userID). A NUL separator
// keeps two orgs/users whose IDs would otherwise concatenate ambiguously from
// colliding.
func reachableCacheKey(orgID, userID string) string {
	return orgID + "\x00" + userID
}

// reachableRepoCacheGet returns the cached reachable slug set for (orgID,
// userID) when present and unexpired. A miss (absent or stale) returns
// (nil, false); a stale entry is reclaimed by the next put's sweep or an
// org evict (the read path holds only an RLock and so can't delete).
func (s *Server) reachableRepoCacheGet(orgID, userID string) (map[string]struct{}, bool) {
	s.reachableRepoMu.RLock()
	defer s.reachableRepoMu.RUnlock()
	e, ok := s.reachableRepoCache[reachableCacheKey(orgID, userID)]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.set, true
}

// reachableRepoCachePut stores the picker enumeration for (orgID, userID) with
// a fresh TTL. The set is stored by reference — callers must not mutate it
// after handing it over.
//
// It also opportunistically sweeps already-expired entries while it holds the
// write lock. Reads return a miss on expiry but can't delete (RLock only), so
// without this the map would retain every distinct (orgID, userID) ever seen.
// A put happens once per picker fetch, so the very workload that would grow
// the map unboundedly — many distinct pairs churning through — is also what
// drives the sweep, keeping it to roughly the live set.
func (s *Server) reachableRepoCachePut(orgID, userID string, set map[string]struct{}) {
	now := time.Now()
	s.reachableRepoMu.Lock()
	defer s.reachableRepoMu.Unlock()
	if s.reachableRepoCache == nil {
		s.reachableRepoCache = make(map[string]reachableRepoEntry)
	}
	for k, e := range s.reachableRepoCache {
		if now.After(e.expiresAt) {
			delete(s.reachableRepoCache, k)
		}
	}
	s.reachableRepoCache[reachableCacheKey(orgID, userID)] = reachableRepoEntry{
		set:       set,
		expiresAt: now.Add(reachableCacheTTL),
	}
}

// evictReachableRepoCache drops every cached enumeration for the org. Called
// when the org's GitHub credentials or App installations rotate
// (SetOnGitHubChanged): a stale enumeration built under the old credential
// must not satisfy a write under the new one. Entries are keyed
// orgID\x00userID, so we evict by orgID prefix to clear all of the org's
// users at once.
func (s *Server) evictReachableRepoCache(orgID string) {
	prefix := orgID + "\x00"
	s.reachableRepoMu.Lock()
	defer s.reachableRepoMu.Unlock()
	for k := range s.reachableRepoCache {
		if strings.HasPrefix(k, prefix) {
			delete(s.reachableRepoCache, k)
		}
	}
}

// agentEnabledForOrg returns the resolved agent and whether the bot is
// enabled for the org's *default* team. Use only where there is no
// specific acting team in play — the team-members roster hint
// (config_handler) that just wants "is a bot generally available to
// show in the picker." Delegation paths must use agentEnabledForTeam
// with the actual acting team, or a non-default team's bot setting is
// read off the default team (a multi-team bug).
func (s *Server) agentEnabledForOrg(ctx context.Context, orgID, userID string) (*domain.Agent, bool, error) {
	var (
		a      *domain.Agent
		teamID string
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		teamID, e = tx.Teams.GetDefaultForOrg(ctx, orgID)
		return e
	}); err != nil {
		return nil, false, fmt.Errorf("default team lookup: %w", err)
	}
	// Empty teamID (teamless org) flows through agentEnabledForTeam,
	// which treats it as "team missing → disabled" — same posture as
	// before.
	a, enabled, err := s.agentEnabledForTeam(ctx, orgID, userID, teamID)
	return a, enabled, err
}

// agentEnabledForTeam returns the resolved agent and whether the
// team_agents.enabled flag is true for it *on the given team*. This is
// the acceptance rule "swipe-to-delegate re-checks
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
		db:           database,
		prompts:      stores.Prompts,
		swipes:       stores.Swipes,
		agents:       stores.Agents,
		teamAgents:   stores.TeamAgents,
		users:        stores.Users,
		blueprints:   stores.Blueprints,
		tasks:        stores.Tasks,
		agentRuns:    stores.Conversations,
		repos:        stores.Repos,
		projects:     stores.Projects,
		events:       stores.Events,
		taskMemory:   stores.TaskMemory,
		secrets:      stores.Secrets,
		curatorStore: stores.Curator,
		teams:        stores.Teams,
		orgs:         stores.Orgs,
		jiraRules:    stores.JiraStatusRules,
		githubApps:   stores.GitHubApps,
		jiraApps:     stores.JiraApps,
		authEvents:   stores.AuthEvents,
		tx:           stores.Tx,
		az:           authz.New(database, stores.Tx),
		fleetQueue:   stores.RunQueue,
		allStores:    stores,
		mux:          http.NewServeMux(),
		ws:           websocket.NewHub(),
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
	//        plus poll-staleness + active-run count (soft signals). Bare
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
	// (TFAC-809) rather than this one, alongside the Slack receiver: it
	// authenticates each delivery by HMAC, so the human-login budget would
	// throttle a legitimate sender, but "GitHub-paced" describes only the
	// legitimate traffic — an anonymous caller can hit it as fast as it
	// likes, and each attempt costs reads before the signature is checked.
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
	s.apiMutating("POST /api/integrations/setup", s.handleIntegrationsSetup)
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
	// Local-mode "Start your factory" provision action — creates
	// the synthetic tenant + materializes shipped defaults via the shared
	// bootstrap chain. Idempotent; no-op once a tenant exists.
	s.apiMutating("POST /api/setup/start", s.handleSetupStart)
	// DELETE on the collection = nuke every integration credential at once (the
	// Settings danger zone). Unbinding ONE credential goes through that
	// credential's own resource — DELETE /api/orgs/{org_id}/github/access/pat or
	// .../jira/access/credential — which is why there's no per-integration
	// subpath here any more.
	s.apiMutating("DELETE /api/integrations", s.handleIntegrationsClear)

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
	// Create a net-new org with default settings — the multi-mode
	// "Start your Factory" onboarding CTA. Multi-mode only (404 in
	// local); 403 when org creation is disabled on the instance.
	s.apiMutating("POST /api/orgs", s.handleOrgCreate)
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
		curator:   func() *curator.Curator { return s.curator },
	}
	s.api("GET /api/teams", th.handleTeamsList)
	s.apiMutating("POST /api/teams", th.handleTeamCreate)
	// PATCH /api/teams/{team_id} renames a team / edits its description
	// (hosted-only; 404 in local). Gated team-admin-or-org-admin; a plain
	// member 403s, a cross-org team_id 404s (VerifyTeamInOrg).
	s.apiMutating("PATCH /api/teams/{team_id}", th.handleTeamUpdate)
	// Team archive/restore lifecycle (TFAC-448), org-admin only, multi-mode.
	// Archive soft-deletes + force-stops the team's in-flight delegations and
	// curator sessions and blocks further writes; restore flips it back (dead
	// runs stay dead). The preview + archived-list back the confirm modal and the
	// org-admin restore surface.
	s.api("GET /api/teams/archived", th.handleTeamArchivedList)
	s.api("GET /api/teams/{team_id}/archive/preview", th.handleTeamArchivePreview)
	s.apiMutating("POST /api/teams/{team_id}/archive", th.handleTeamArchive)
	s.apiMutating("POST /api/teams/{team_id}/restore", th.handleTeamRestore)

	// Team roster (TFAC-444): list members + add + change role + remove/leave.
	// Multi-mode only (each handler 404s in local; local keeps the synthetic
	// single-member /api/team/members roster). GET is any org member;
	// POST/PATCH/DELETE gate team-admin-or-org-admin (DELETE also allows a
	// self-leave). VerifyTeamInOrg 404s a cross-org team_id; the last-admin
	// guard is a DB trigger surfaced as a 409.
	tmh := &teamMembersHandler{tx: s.tx, az: s.az}
	s.api("GET /api/teams/{team_id}/members", tmh.handleTeamRosterList)
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
	s.api("GET /api/orgs/{org_id}/members", omh.handleOrgMembersList)
	s.apiMutating("PATCH /api/orgs/{org_id}/members/{user_id}", omh.handleOrgMemberRoleChange)
	s.apiMutating("DELETE /api/orgs/{org_id}/members/{user_id}", omh.handleOrgMemberRemove)
	// Ownership transfer: owner-only (gated on tf.user_owns_org + the
	// guard_org_owner_transfer trigger). Promotes the target, repoints the
	// owner sentinel, demotes the former owner — all in one tx as the owner.
	s.apiMutating("POST /api/orgs/{org_id}/transfer-ownership", omh.handleOrgOwnershipTransfer)

	// Usage (spend layer) — the core Usage page's read API (TFAC-478). All
	// session-org-scoped (org from claims, not the path), like /api/dashboard/*.
	// Scope is role-gated: /me is any org member, /teams/{id} is team-admin OR
	// org-admin, /org is org-admin. The team/org reads use the admin-pool
	// ListSpendSystem (the role gate is the authorization for crossing RLS).
	uh := &usageHandler{tx: s.tx, az: s.az, runQueue: s.allStores.RunQueue}
	s.api("GET /api/usage/me", uh.handleUsageMe)
	s.api("GET /api/usage/teams/{team_id}", uh.handleUsageTeam)
	s.api("GET /api/usage/org", uh.handleUsageOrg)
	// Org-scoped operations subset (TFAC-589): an org admin's own queue waits +
	// run durations + failure rates. Org-admin gated, SaaS-safe (no cross-tenant
	// machine truth) — the org-facing complement to the operator-only fleet console.
	s.api("GET /api/usage/org/ops", uh.handleUsageOrgOps)
	// Activity feed (EE, FeatureGovernance): the team/org Actions (external-action
	// audit log) + Objects (artifact history) lenses, selected by ?view= — same
	// scope gates as the spend reads above, plus the entitlement (unlicensed →
	// 404). TFAC-483.
	s.api("GET /api/usage/teams/{team_id}/activity", uh.handleUsageTeamActivity)
	s.api("GET /api/usage/org/activity", uh.handleUsageOrgActivity)
	// Per-team daily spend cap (TFAC-482) — org-admin-set, EE/governance-gated
	// (the handlers 404 when unlicensed). The GET lists every active team + its cap
	// for the editor (so an idle team can be pre-capped); the PUT writes one cap and
	// is mutating, so it runs through the CSRF + session wrap. Both write/read the
	// admin pool: an org admin may cap a team they don't belong to.
	s.api("GET /api/usage/org/team-caps", uh.handleUsageTeamCaps)
	s.apiMutating("PUT /api/usage/teams/{team_id}/cap", uh.handleUsageTeamCap)
	// EE governance audit surface (TFAC-484): the access & credential change-log
	// viewer. Org-admin-gated AND FeatureGovernance-gated (404 unlicensed) inside
	// the handler — the data is core, only the cross-team lens is Enterprise.
	s.api("GET /api/usage/org/access-log", uh.handleUsageAccessLog)

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
	s.api("GET /api/invites", ih.handleInviteList)
	s.apiMutating("POST /api/invites", ih.handleInviteCreate)
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

	s.api("GET /api/queue", s.handleQueue)
	s.api("GET /api/tasks", s.handleTasks)
	s.api("GET /api/tasks/{id}", s.handleTaskGet)
	s.apiMutating("POST /api/tasks/{id}/swipe", s.handleSwipe)
	s.apiMutating("POST /api/tasks/{id}/snooze", s.handleSnooze)
	s.apiMutating("POST /api/tasks/{id}/undo", s.handleUndo)
	s.apiMutating("POST /api/tasks/{id}/requeue", s.handleRequeue)
	s.apiMutating("POST /api/tasks/{id}/advance", s.handleTaskAdvance)

	ag := &agentHandler{tx: s.tx, ws: s.ws, spawner: func() *delegate.Spawner { return s.spawner }, reconciler: func() *reconcile.Reconciler { return s.reconciler }}
	s.api("GET /api/agent/conversations/{conversationID}", ag.handleAgentStatus)
	s.api("GET /api/agent/conversations/{conversationID}/messages", ag.handleMessages)
	// Run-scoped artifact read (A·6, TFAC-465): the run's artifacts across
	// every kind, team-scoped via the run. Backs the run-detail surface (TFAC-470).
	s.api("GET /api/agent/conversations/{conversationID}/artifacts", ag.handleAgentArtifacts)
	// Run-scoped external-action read — the audit log filtered to this run.
	// Its sibling above answers "what objects does this run own"; this answers
	// "what did it do", including the writes that produce no object at all (a
	// review-thread reply, a refused merge, a denied push).
	s.api("GET /api/agent/conversations/{conversationID}/actions", ag.handleAgentActions)
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
	// Tier-2 run-scoped artifact reconcile (TFAC-464): the run view polls this
	// while open to refresh that run's non-terminal artifacts against GitHub.
	s.apiMutating("POST /api/agent/conversations/{conversationID}/artifacts/refresh", ag.handleArtifactRefresh)
	s.api("GET /api/agent/conversations", ag.handleConversations)

	// Projects. Pure CRUD over the projects table; the
	// Curator runtime that populates curator_session_id and per-project
	// entity classification land separately.
	s.apiMutating("POST /api/projects", s.handleProjectCreate)
	s.api("GET /api/projects", s.handleProjectList)
	s.api("GET /api/projects/{id}", s.handleProjectGet)
	s.apiMutating("PATCH /api/projects/{id}", s.handleProjectUpdate)
	s.apiMutating("DELETE /api/projects/{id}", s.handleProjectDelete)
	s.api("GET /api/projects/{id}/export/preview", s.handleProjectExportPreview)
	s.api("GET /api/projects/{id}/export", s.handleProjectExport)
	s.apiMutating("POST /api/projects/import", s.handleProjectImport)
	s.api("GET /api/projects/{id}/knowledge", s.handleProjectKnowledge)
	s.apiMutating("POST /api/projects/{id}/knowledge", s.handleProjectKnowledgeUpload)
	s.api("GET /api/projects/{id}/knowledge/{path}", s.handleProjectKnowledgeFile)
	s.apiMutating("DELETE /api/projects/{id}/knowledge/{path}", s.handleProjectKnowledgeDelete)
	// Project-creation backfill popup.
	bf := &backfillHandler{tx: s.tx, ws: s.ws}
	s.api("GET /api/projects/{id}/backfill-candidates", bf.handleBackfillCandidates)
	s.apiMutating("POST /api/projects/{id}/backfill", bf.handleBackfill)
	// Project entities panel.
	pe := &projectEntitiesHandler{tx: s.tx}
	s.api("GET /api/projects/{id}/entities", pe.handleProjectEntities)

	// Curator chat per project. The Curator package owns the
	// long-lived CC session lifecycle; these endpoints are the API
	// the Projects page will hit.
	ch := &curatorHandler{tx: s.tx, curatorStore: s.curatorStore, ws: s.ws, runtime: func() *curator.Curator { return s.curator }}
	s.apiMutating("POST /api/projects/{id}/curator/messages", ch.handleCuratorSend)
	s.api("GET /api/projects/{id}/curator/messages", ch.handleCuratorHistory)
	s.apiMutating("DELETE /api/projects/{id}/curator/messages/in-flight", ch.handleCuratorCancel)
	s.apiMutating("POST /api/projects/{id}/curator/reset", ch.handleCuratorReset)

	// Websocket: wrapped via s.api so the handshake sees claims in
	// r.Context() (sentinel in local mode, real values in multi).
	// handleWS pulls (userID, orgID) out and threads them into the
	// hub's HandleWS so the per-connection scoping in pkg/websocket
	// can filter Broadcast fanout without importing internal/server.
	// Treated as GET-equivalent — no CSRF wrap.
	s.api("GET /api/ws", s.handleWS)

	dh := &dashboardHandler{tx: s.tx, ghResolver: s.ghResolver, backfill: s.kickDashboardBackfill}
	s.api("GET /api/dashboard/stats", dh.handleDashboardStats)
	s.api("GET /api/dashboard/prs", dh.handleDashboardPRs)
	s.api("GET /api/dashboard/prs/{number}/status", dh.handleDashboardPRStatus)
	s.apiMutating("POST /api/dashboard/prs/{number}/draft", dh.handleDashboardPRDraft)

	s.api("GET /api/settings/user", s.handleUserSettingsGet)
	s.apiMutating("POST /api/settings/user", s.handleUserSettingsPost)
	s.api("GET /api/settings/team/{team_id}", s.handleTeamSettingsGet)
	s.apiMutating("POST /api/settings/team/{team_id}", s.handleTeamSettingsPost)
	s.api("GET /api/settings/team/{team_id}/github-groups", s.handleTeamGitHubGroupsGet)
	s.apiMutating("PUT /api/settings/team/{team_id}/github-groups", s.handleTeamGitHubGroupsPut)
	s.api("GET /api/settings/team/{team_id}/repos", s.handleTeamReposGet)
	s.apiMutating("PUT /api/settings/team/{team_id}/repos", s.handleTeamReposPut)
	s.api("GET /api/settings/org", s.handleOrgSettingsGet)
	s.apiMutating("POST /api/settings/org", s.handleOrgSettingsPost)

	// Team roster for the predicate editor. Fetched fresh on
	// every consumer mount (the FE dedups concurrent in-flight calls
	// within a render but doesn't hold a persistent cache — the roster
	// is mutable mid-session). One SELECT per call. /api/config — the
	// AuthGate boot endpoint — is mounted pre-auth above; per-user
	// identity that used to live on /api/config moved to /api/me.
	s.api("GET /api/team/members", s.handleTeamMembers)
	sk := &skillsHandler{db: s.db, prompts: s.prompts, tx: s.tx, az: s.az}
	s.apiMutating("POST /api/skills/import", sk.handleSkillsImport)
	s.apiMutating("POST /api/skills/upload", sk.handleSkillUpload)
	s.api("GET /api/github/repos", s.handleGitHubRepos)
	se := &settingsHandler{tx: s.tx, az: s.az, bedrockRole: func() bedrockRoleResolver { return s.bedrockRole }}
	s.apiMutating("POST /api/github/preflight-ssh", se.handleGitHubPreflightSSH)
	// URL-only host reachability (the wizard's URL sub-step) — no auth sent,
	// distinct from the creds stage (auth.ValidateGitHub / /api/jira/connect).
	s.apiMutating("POST /api/github/reachability", handleGitHubReachability)
	s.api("GET /api/repos", s.handleRepoProfiles)
	s.apiMutating("PATCH /api/repos/{owner}/{repo}", s.handleRepoUpdate)
	s.api("GET /api/repos/{owner}/{repo}/branches", s.handleRepoBranches)
	s.apiMutating("POST /api/jira/reachability", handleJiraReachability)
	// Validated org Anthropic-key capture — the single write path for the
	// anthropic_api_key vault secret (an empty key clears it for "system creds").
	s.apiMutating("POST /api/anthropic/connect", se.handleAnthropicConnect)
	// Validated org Bedrock-credential capture — the single write path for
	// the aws_* / bedrock_* vault secrets (auth_method "none" clears them).
	s.apiMutating("POST /api/bedrock/connect", se.handleBedrockConnect)
	// Bedrock role-mode setup (TFAC-616): returns the control service's caller
	// ARN + the TF-generated External ID so the UI can render a filled
	// trust-policy snippet. POST (not GET) because first entry generates +
	// persists the External ID — a state change that needs CSRF protection,
	// per the mutating-verb convention (route_auth_test enforces GET ≠
	// apiMutating).
	s.apiMutating("POST /api/bedrock/role-setup", se.handleBedrockRoleSetup)
	s.api("GET /api/jira/statuses", se.handleJiraStatuses)
	s.api("GET /api/jira/stock", s.handleJiraStockGet)
	s.apiMutating("POST /api/jira/stock", s.handleJiraStockPost)

	// Artifact-id-addressed endpoints back both the GitHub-native PR preview and
	// the review preview (kind-dispatched inside the handlers). The review path
	// (TFAC-463) replaced the local pending_reviews path: GET serves the artifact
	// + live pending-review comments, PATCH stages body/event, the comment routes
	// edit/delete inline comments on the live pending review, approve submits it,
	// and dismiss resolves a single artifact (per-item). The task-level
	// resolve-all (drag-to-Done / Return-to-queue) flows through teardownTaskArtifacts.
	ah := &artifactsHandler{tx: s.tx, ws: s.ws, agentRuns: s.agentRuns, ghResolver: s.ghResolver, spawner: func() *delegate.Spawner { return s.spawner }}
	s.api("GET /api/artifacts/{id}", ah.handleArtifactGet)
	s.apiMutating("PATCH /api/artifacts/{id}", ah.handleArtifactUpdate)
	s.api("GET /api/artifacts/{id}/diff", ah.handleArtifactDiff)
	s.apiMutating("POST /api/artifacts/{id}/approve", ah.handleArtifactApprove)
	s.apiMutating("POST /api/artifacts/{id}/dismiss", ah.handleArtifactDismiss)
	s.apiMutating("POST /api/artifacts/{id}/review/refresh", ah.handleReviewRefresh)
	s.apiMutating("PUT /api/artifacts/{id}/comments/{commentId}", ah.handleArtifactCommentUpdate)
	s.apiMutating("DELETE /api/artifacts/{id}/comments/{commentId}", ah.handleArtifactCommentDelete)

	fh := &factoryHandler{tx: s.tx}
	s.api("GET /api/factory/snapshot", fh.handleFactorySnapshot)
	s.apiMutating("POST /api/factory/delegate", s.handleFactoryDelegate)

	ph := &promptsHandler{db: s.db, tx: s.tx, az: s.az}
	s.api("GET /api/event-types", ph.handleEventTypes)
	s.api("GET /api/event-schemas", handleEventSchemasList)
	s.api("GET /api/event-schemas/{event_type}", handleEventSchemaGet)
	// Unified event_handlers endpoints. Replace the former
	// /api/task-rules + /api/triggers split — kind is passed as ?kind=
	// on list, in the body on create, derived on update.
	eh := &eventHandlersHandler{tx: s.tx, az: s.az}
	s.api("GET /api/event-handlers", eh.handleEventHandlersList)
	s.apiMutating("POST /api/event-handlers", eh.handleEventHandlerCreate)
	s.apiMutating("PUT /api/event-handlers/reorder", eh.handleEventHandlerReorder)
	s.apiMutating("PATCH /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("PUT /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("DELETE /api/event-handlers/{id}", eh.handleEventHandlerDelete)
	s.apiMutating("POST /api/event-handlers/{id}/toggle", eh.handleEventHandlerToggle)
	s.apiMutating("POST /api/event-handlers/{id}/promote", eh.handleEventHandlerPromote)
	s.apiMutating("POST /api/event-handlers/{id}/retarget", eh.handleEventHandlerRetarget)
	// Parked event_queue rows — the operator surface over routing work the
	// queue gave up on, and the requeue that puts it back. Org-admin gated
	// inside the handler; the store is the admin-pool EventQueueStore with
	// org_id bound by argument, the same shape as the usage-ops read.
	fe := &failedEventsHandler{az: s.az, queue: s.allStores.EventQueue}
	s.api("GET /api/events/failed", fe.handleFailedEventsList)
	s.apiMutating("POST /api/events/failed/requeue", fe.handleFailedEventsRequeue)
	s.api("GET /api/prompts", ph.handlePromptsList)
	s.apiMutating("POST /api/prompts", ph.handlePromptCreate)
	s.api("GET /api/prompts/{id}", ph.handlePromptGet)
	s.apiMutating("PUT /api/prompts/{id}", ph.handlePromptPut)
	s.apiMutating("DELETE /api/prompts/{id}", ph.handlePromptDelete)
	s.api("GET /api/prompts/{id}/stats", ph.handlePromptStats)
	bh := &blueprintsHandler{tx: s.tx, az: s.az, spawner: func() *delegate.Spawner { return s.spawner }}
	s.api("GET /api/blueprints", bh.handleBlueprintsList)
	s.apiMutating("POST /api/blueprints", bh.handleBlueprintCreate)
	s.apiMutating("PUT /api/blueprints/{id}", bh.handleBlueprintUpdate)
	s.apiMutating("DELETE /api/blueprints/{id}", bh.handleBlueprintDelete)
	s.api("GET /api/blueprint-steps", bh.handleBlueprintStepsAll)
	s.api("GET /api/blueprints/{id}/steps", bh.handleBlueprintStepsGet)
	s.apiMutating("PUT /api/blueprints/{id}/steps", bh.handleBlueprintStepsPut)
	s.apiMutating("POST /api/blueprints/{id}/merge", bh.handleBlueprintMerge)
	s.apiMutating("POST /api/blueprints/{id}/split", bh.handleBlueprintSplit)
	s.apiMutating("POST /api/blueprints/{id}/reconnect", bh.handleBlueprintReconnect)
	s.apiMutating("POST /api/blueprints/duplicate", bh.handleBlueprintDuplicate)
	s.api("GET /api/blueprint-runs/{id}", bh.handleBlueprintRunGet)
	s.apiMutating("POST /api/blueprint-runs/{id}/cancel", bh.handleBlueprintRunCancel)

	// Within-org prompt marketplace (TFAC-536) — multi-mode only; every
	// handler opens with gateMarketplace, which 404s in local mode (see
	// marketplace_handler.go).
	mh := &marketplaceHandler{tx: s.tx, az: s.az}
	s.api("GET /api/marketplace/listings", mh.handleMarketplaceList)
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
	// Any org member (read), so requireOrgMember rather than requireOrgAdmin.
	s.api("GET /api/orgs/{org_id}/github/app", s.handleGitHubAppStatus)
	s.api("GET /api/orgs/{org_id}/github/app/install-url", s.handleGitHubAppInstallURL)
	// On-demand installation reconcile — the "UI panel refresh" half of D11
	// installation discovery (the poller cycle is the other). Admin-only (the
	// setup wizard's install step + the Settings App panel call it) and
	// mode-agnostic. Mutating: it reconciles the installation mirror via the
	// same API backfill the poller runs, so it rides apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github/app/installations/refresh", s.handleGitHubAppInstallationsRefresh)

	// GitHub access either/or transitions. GitHub access is
	// strictly App XOR PAT per org; these commit the switches and surface the
	// inform-only reachability diffs. All org-admin (gated inside the handler).
	//   - cutover: commit a staged PAT→App switch (activate App + delete PAT).
	//   - switch-to-pat: full App teardown, validate + store the new PAT.
	//   - DELETE github/app: discard a staged (not-yet-live) App registration.
	//   - cutover-preflight / pat-preflight: inform-only reachability diffs.
	// The two commits + the discard mutate state (apiMutating, CSRF); the
	// cutover-preflight is a read (api); pat-preflight POSTs a token to probe
	// reach but stores nothing — still apiMutating for the same-origin guard.
	s.apiMutating("POST /api/orgs/{org_id}/github/app/cutover", s.handleGitHubAppCutover)
	s.apiMutating("POST /api/orgs/{org_id}/github/access/switch-to-pat", s.handleGitHubAccessSwitchToPAT)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/app", s.handleGitHubAppDiscard)
	s.api("GET /api/orgs/{org_id}/github/app/cutover-preflight", s.handleGitHubAppCutoverPreflight)
	s.apiMutating("POST /api/orgs/{org_id}/github/access/pat-preflight", s.handleGitHubAccessPATPreflight)

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
	s.apiMutating("PUT /api/orgs/{org_id}/github/access/pat", s.handleGitHubPATPut)
	s.apiMutating("DELETE /api/orgs/{org_id}/github/access/pat", s.handleGitHubPATDelete)

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

// SetSpawner sets the delegation spawner for agent runs.
func (s *Server) SetSpawner(sp *delegate.Spawner) {
	s.spawner = sp
}

// SetCurator wires the Curator runtime into the server so the
// /api/projects/{id}/curator/* endpoints can dispatch messages and
// the project-delete handler can cancel in-flight chats. Wired
// post-construction (mirrors SetSpawner) so main.go can build the
// Curator after the websocket hub is constructed.
func (s *Server) SetCurator(c *curator.Curator) {
	s.curator = c
}

// SetKBStore wires the knowledge-base blob store into the server so the
// /api/projects/{id}/knowledge/* handlers serve the KB from the object store
// in multi mode. Wired post-construction (mirrors SetSpawner) once the shared
// storage.Storage exists. Left nil in local mode — the handlers branch on
// runmode and never dereference it there.
func (s *Server) SetKBStore(kb *kbstore.Store) {
	s.kb = kb
}

// SetKBChangedDoorbell wires the cross-pod tf_ctl publisher the KB
// upload/delete/project-delete handlers ring so the home executor materializes
// the panel write mid-session. Multi mode only; nil elsewhere degrades to
// the executor's turn-start materialize latency, never lost data.
func (s *Server) SetKBChangedDoorbell(fn func(op, orgID, projectID string)) {
	s.kbChangedDoorbell = fn
}

// SetOnGitHubChanged registers a callback for GitHub config changes (creds, URL, repos).
// The callback re-dues the org's GitHub poll so the new credential/repo set
// applies on the next wake; repo profiling is no longer triggered here — it's
// driven by the system:poll "profiler" subscriber off that poll's completion.
// The orgID is the tenant whose creds changed — closure re-resolves via SecretStore.
//
// The registered callback is wrapped so the reachable-repo enumeration cache
// is evicted for the org *before* the re-due runs: a creds
// rotation, App install, or repo-set change can move which repos the org can
// reach, and a stale cached enumeration must never satisfy the next write.
//
// Handlers fire this callback in a goroutine, so the
// eviction is asynchronous: a second write landing in the same instant as a
// first could still read the pre-eviction cache. That race is benign — the
// cache was just validated as correct, and the near-simultaneous second write
// is still checked against it. Eviction exists to retire a *stale* enumeration
// over the TTL window, not to serialize against a concurrent write.
func (s *Server) SetOnGitHubChanged(fn func(orgID string)) {
	s.onGitHubChanged = func(orgID string) {
		s.evictReachableRepoCache(orgID)
		if fn != nil {
			fn(orgID)
		}
	}
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

// SetReconciler registers the shared artifact Reconciler that backs the Tier-2
// run-scoped refresh endpoint (TFAC-464). The same instance the background
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
