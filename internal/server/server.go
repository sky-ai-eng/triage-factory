package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Server is the main HTTP server for Triage Factory.
type Server struct {
	db           *sql.DB
	prompts      db.PromptStore
	swipes       db.SwipeStore
	agents       db.AgentStore           // SKY-261 D-Claims: resolves the org's agent for claim stamps
	teamAgents   db.TeamAgentStore       // SKY-261 D-Claims: re-checks team_agents.enabled on swipe-delegate / factory-delegate
	users        db.UsersStore           // display_name + Jira binding on the user row; host-scoped GitHub identity via user_github_identities (SKY-396)
	blueprints   db.BlueprintStore       // used by event-handler + project test fixtures
	tasks        db.TaskStore            // SKY-283: task lifecycle, claim, queue + factory snapshot reads
	agentRuns    db.AgentRunStore        // SKY-285: agent run lifecycle + transcript
	repos        db.RepoStore            // SKY-288: repo_profiles CRUD for repos/settings/projects handlers and curator pinned-repo materialization
	projects     db.ProjectStore         // SKY-290: projects CRUD for projects/curator/backfill/project_entities handlers
	curatorStore db.CuratorStore         // curator-runtime CRUD (curator_requests / curator_messages / curator_pending_context) — handler-side writes go through here so Postgres mode honors RLS and uses the right placeholder syntax
	events       db.EventStore           // SKY-305: events audit log Record/Latest for stock carry-over + factory drag-to-delegate
	taskMemory   db.TaskMemoryStore      // run_memory writes (human verdict capture on review/PR submit, swipe-discard cleanup)
	secrets      db.SecretStore          // canonical credential read/write path — local-mode keychain, multi-mode vault
	teams        db.TeamsStore           // resolves the request org's default team for handlers that synthesize team-scoped rows (tasks, projects, prompts)
	orgs         db.OrgsStore            // per-org settings (GitHub/Jira base URLs, poll intervals, clone protocol) post-internal/config deletion
	jiraRules    db.JiraStatusRulesStore // per-team Jira status rules (replaces the deleted config.Jira.Projects view)
	githubApps   db.GitHubAppsStore      // per-org GitHub App registrations (manifest flow)
	orgTemplate  db.OrgTemplateStore     // SKY-381: org-admin-editable template new teams are seeded from
	// serverPort is the stored instance_config.server_port value
	// surfaced to the settings GET response. The actual bind port
	// comes from --port at boot, not this field — the Settings page
	// reads it to populate its server_port input.
	serverPort int
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
	spawner   *delegate.Spawner
	curator   *curator.Curator
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
	// jiraResolver routes Jira writes by provenance (SKY-463): ForSystem for
	// the org/bot service cred, ForUser for the acting user's own stored
	// credential. User-initiated board claim / undo / requeue resolve via
	// ForUser so the write is attributed to the user, not the service account.
	// Built in New from the stores, so it's never nil — handlers don't guard.
	jiraResolver jira.Resolver
	// Change callbacks accept the orgID of the tenant whose integration
	// creds just rotated, so the closure can re-resolve via SecretStore.
	// Local mode always passes runmode.LocalDefaultOrgID; multi-mode
	// handlers thread the request's orgID through so the callback
	// can't fire one org's poller restart with another org's PAT.
	onGitHubChanged func(orgID string) // GitHub creds/repos changed — full restart + re-profile
	onJiraChanged   func(orgID string) // Jira config changed — restart Jira poller only
	scorerTrigger   func(orgID string) // invoked after non-poll task creation (e.g. carry-over) to kick the per-org scorer immediately

	// bus is the in-process event bus. The GitHub webhook receiver
	// publishes verified deliveries here; nil until SetEventBus runs
	// (the receiver no-ops the publish if so). Content-event processing
	// is a downstream subscriber's concern.
	bus *eventbus.Bus

	// onInstallationRemoved, when set, fires on a verified
	// installation.deleted webhook so the credential resolver's
	// per-installation token cache can drop the now-dead entry. Nil
	// until the resolver wires it; the receiver skips the call when nil.
	onInstallationRemoved func(orgID, installationID string)

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

	// Jira poll readiness — used by /api/jira/stock to decide whether the
	// poller has completed its first cycle after a restart. Carry-over reads
	// from the DB and needs snapshots to be populated before showing tickets.
	jiraPollMu      sync.RWMutex
	jiraRestartedAt time.Time
	jiraLastPollAt  time.Time

	// projectMutexes serializes PATCH-style read-merge-write
	// operations per project ID so two concurrent autosaves from
	// different widgets (e.g. pinned-repos editor and tracker
	// picker) can't lost-update each other. SQLite serializes
	// individual writes via MaxOpenConns=1, but that's not enough
	// here — handler A reads pre-A state, handler B reads pre-A
	// state, A writes, B writes B's merge over pre-A state, and
	// A's contribution is lost. Holding the per-project mutex
	// across the read+write window closes that hole.
	projectMutexes sync.Map // map[string]*sync.Mutex

	// githubAppRegMu serializes per-org GitHub App registration so
	// two concurrent callbacks can't both pass the existence check,
	// both call GitHub's conversion endpoint, and leave an orphan
	// App. Same sync.Map pattern as projectMutexes.
	githubAppRegMu sync.Map // map[orgID]*sync.Mutex

	// reachableRepoMu guards reachableRepoCache — the in-process
	// enumeration cache the team-repos write gate consults before
	// re-enumerating the org (SKY-409). The picker
	// (handleGitHubRepos) warms it on the way out; the immediate-next
	// PUT /api/settings/team/{id}/repos validates against this set in
	// ~µs instead of paying the full ListUserRepos cost a second time.
	// Entries are TTL-bounded (reachableCacheTTL) and evicted per-org
	// when GitHub creds/installations rotate (SetOnGitHubChanged).
	reachableRepoMu    sync.RWMutex
	reachableRepoCache map[string]reachableRepoEntry // key: orgID\x00userID
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

// projectMutex returns the per-project mutex for serializing
// read-merge-write handlers. Created on demand via LoadOrStore; the
// map grows monotonically with project count, which is fine — they
// stay user-curated and small. Project deletion doesn't bother
// removing the entry: a stale mutex on a missing project is just
// unused memory, and the next call for that ID is a no-op.
func (s *Server) projectMutex(id string) *sync.Mutex {
	if v, ok := s.projectMutexes.Load(id); ok {
		return v.(*sync.Mutex)
	}
	v, _ := s.projectMutexes.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// agentEnabledForOrg returns the resolved agent and whether the bot is
// enabled for the org's *default* team. Use only where there is no
// specific acting team in play — the team-members roster hint
// (config_handler) that just wants "is a bot generally available to
// show in the picker." Delegation paths must use agentEnabledForTeam
// with the actual acting team, or a non-default team's bot setting is
// read off the default team (the SKY-378 multi-team bug).
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
// the SKY-261 acceptance rule "swipe-to-delegate re-checks
// team_agents.enabled at delegate time," now correctly scoped to the
// acting team (SKY-378) rather than always the org default — so a
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
			// v1.11.0 baseline migration), so this is a bootstrap bug —
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
// per-resource store bundle + the boot-time stored server port, and
// registers all routes. The Server retains individual store fields
// rather than a single db.Stores struct so existing handler code keeps
// working — the constructor just unpacks the bundle once instead of
// forcing every caller to enumerate 20+ stores positionally.
//
// raw *sql.DB stays available for handlers that haven't been
// ported to a store yet.
func New(database *sql.DB, stores db.Stores, serverPort int) *Server {
	s := &Server{
		db:           database,
		prompts:      stores.Prompts,
		swipes:       stores.Swipes,
		agents:       stores.Agents,
		teamAgents:   stores.TeamAgents,
		users:        stores.Users,
		blueprints:   stores.Blueprints,
		tasks:        stores.Tasks,
		agentRuns:    stores.AgentRuns,
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
		orgTemplate:  stores.OrgTemplate,
		tx:           stores.Tx,
		az:           authz.New(database, stores.Tx),
		allStores:    stores,
		serverPort:   serverPort,
		mux:          http.NewServeMux(),
		ws:           websocket.NewHub(),
	}
	// GitHub credential resolver + its installation-token cache, built from
	// the same stores. Constructed here (not injected) so a Server is always
	// usable without external wiring — tests that call New directly get a
	// working resolver too. A verified installation.deleted webhook drops
	// the dead token from the cache via onInstallationRemoved.
	ghTokenCache := ghclient.NewMemoryTokenCache()
	s.ghResolver = ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, ghTokenCache)
	// Jira write-actor resolver (SKY-463): ForSystem (org/bot cred) +
	// ForUser (acting user's cred). Constructed here like ghResolver so a
	// Server is always usable without external wiring.
	s.jiraResolver = jira.NewResolver(stores.Secrets, stores.Orgs)
	s.onInstallationRemoved = func(orgID, installationID string) {
		ghTokenCache.Invalidate(orgID, installationID)
	}
	s.routes()
	return s
}

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
// X-Frame-Options, etc.). Returns nil on a clean ctx-driven shutdown; a
// non-ErrServerClosed listen error otherwise.
func (s *Server) ListenAndServeContext(ctx context.Context, addr string) error {
	httpSrv := &http.Server{Addr: addr, Handler: s.withSecurityHeaders(s.mux)}

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
	//   POST /api/webhooks/github/{org_id} — GitHub App webhook receiver;
	//        GitHub has no session, and the handler verifies the HMAC
	//        signature against the org's stored webhook secret itself.
	//   /auth/v1/                        — GoTrue reverse proxy; auth
	//        happens upstream, not in our middleware.
	//   /                                — SPA fallback; static-file
	//        serving with no identity dependency.
	s.mux.HandleFunc("GET /api/auth/oauth/{provider}", s.handleOAuthStart)
	s.mux.HandleFunc("GET /api/auth/callback", s.handleOAuthCallback)
	s.mux.Handle("POST /api/auth/logout", s.withCSRFOriginCheck(http.HandlerFunc(s.handleLogout)))
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	// Liveness probe — pre-auth so platform healthchecks (Fly checks,
	// compose healthcheck, k8s liveness) can hit it without a session.
	// Plain 200 OK with a tiny JSON body. Don't expand this into a
	// readiness probe (which would couple to DB + integrations) — the
	// platforms use auto-restart on liveness failure and we don't want
	// a flapping integration to recycle the whole process.
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
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
	// the post-SKY-251 cleanup so /api/auth/* unambiguously means
	// "session authentication." Wrapped via s.api/apiMutating since you
	// need to be logged in to manage your integration credentials.
	s.apiMutating("POST /api/integrations/setup", s.handleIntegrationsSetup)
	s.api("GET /api/integrations/status", s.handleIntegrationsStatus)
	// Local-mode "Start your factory" provision action — creates
	// the synthetic tenant + materializes shipped defaults via the shared
	// bootstrap chain. Idempotent; no-op once a tenant exists.
	s.apiMutating("POST /api/setup/start", s.handleSetupStart)
	// DELETE on the collection = nuke all integration credentials.
	// Targeted clears (Jira only) get explicit subpaths.
	s.apiMutating("DELETE /api/integrations", s.handleIntegrationsClear)
	s.apiMutating("DELETE /api/integrations/jira", s.handleIntegrationsDeleteJira)

	// Logout-everywhere: must be authenticated to use it (you can only
	// nuke your own sessions).
	s.apiMutating("POST /api/auth/logout/all", s.handleLogoutAll)
	// /api/me is the session-protected identity endpoint. In local mode
	// the shim in withSession injects sentinel claims so the handler
	// sees a non-nil claims value; in multi mode the handler 401s when
	// no claims are seeded.
	s.api("GET /api/me", s.handleMe)
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
	th := &teamsHandler{tx: s.tx, az: s.az, allStores: s.allStores}
	s.api("GET /api/teams", th.handleTeamsList)
	s.apiMutating("POST /api/teams", th.handleTeamCreate)

	s.api("GET /api/queue", s.handleQueue)
	s.api("GET /api/tasks", s.handleTasks)
	s.api("GET /api/tasks/{id}", s.handleTaskGet)
	s.apiMutating("POST /api/tasks/{id}/swipe", s.handleSwipe)
	s.apiMutating("POST /api/tasks/{id}/snooze", s.handleSnooze)
	s.apiMutating("POST /api/tasks/{id}/undo", s.handleUndo)
	s.apiMutating("POST /api/tasks/{id}/requeue", s.handleRequeue)
	s.apiMutating("POST /api/tasks/{id}/advance", s.handleTaskAdvance)

	ag := &agentHandler{tx: s.tx, ws: s.ws, spawner: func() *delegate.Spawner { return s.spawner }}
	s.api("GET /api/agent/runs/{runID}", ag.handleAgentStatus)
	s.api("GET /api/agent/runs/{runID}/messages", ag.handleAgentMessages)
	s.apiMutating("POST /api/agent/runs/{runID}/cancel", ag.handleAgentCancel)
	s.apiMutating("POST /api/agent/runs/{runID}/message", ag.handleAgentMessage)
	s.apiMutating("POST /api/agent/runs/{runID}/interrupt", ag.handleAgentInterrupt)
	s.apiMutating("POST /api/agent/runs/{runID}/permissions/{requestID}", ag.handleAgentPermission)
	s.api("GET /api/agent/runs", ag.handleAgentRuns)

	// Projects (SKY-215). Pure CRUD over the projects table; the
	// Curator runtime that populates curator_session_id lands in
	// SKY-216 and per-project entity classification in SKY-220.
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
	// Project-creation backfill popup (SKY-220 PR B).
	bf := &backfillHandler{tx: s.tx, ws: s.ws}
	s.api("GET /api/projects/{id}/backfill-candidates", bf.handleBackfillCandidates)
	s.apiMutating("POST /api/projects/{id}/backfill", bf.handleBackfill)
	// Project entities panel (SKY-238).
	pe := &projectEntitiesHandler{tx: s.tx}
	s.api("GET /api/projects/{id}/entities", pe.handleProjectEntities)

	// Curator chat per project (SKY-216). The Curator package owns the
	// long-lived CC session lifecycle; these endpoints are the API
	// the Projects page (SKY-217) will hit.
	ch := &curatorHandler{db: s.db, tx: s.tx, ws: s.ws, runtime: func() *curator.Curator { return s.curator }}
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

	dh := &dashboardHandler{tx: s.tx, ghResolver: s.ghResolver}
	s.api("GET /api/dashboard/stats", dh.handleDashboardStats)
	s.api("GET /api/dashboard/prs", dh.handleDashboardPRs)
	s.api("GET /api/dashboard/prs/{number}/status", dh.handleDashboardPRStatus)
	s.apiMutating("POST /api/dashboard/prs/{number}/draft", dh.handleDashboardPRDraft)

	s.api("GET /api/brief", s.handleBrief)
	s.api("GET /api/preferences", s.handlePreferences)

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

	// SKY-264: team roster for the predicate editor. Fetched fresh on
	// every consumer mount (the FE dedups concurrent in-flight calls
	// within a render but doesn't hold a persistent cache — the roster
	// is mutable mid-session). One SELECT per call. /api/config — the
	// AuthGate boot endpoint — is mounted pre-auth above; per-user
	// identity that used to live on /api/config moved to /api/me.
	s.api("GET /api/team/members", s.handleTeamMembers)
	sk := &skillsHandler{db: s.db, prompts: s.prompts}
	s.apiMutating("POST /api/skills/import", sk.handleSkillsImport)
	s.api("GET /api/github/repos", s.handleGitHubRepos)
	se := &settingsHandler{tx: s.tx}
	s.apiMutating("POST /api/github/preflight-ssh", se.handleGitHubPreflightSSH)
	// URL-only host reachability (the wizard's URL sub-step) — no auth sent,
	// distinct from the creds stage (auth.ValidateGitHub / /api/jira/connect).
	s.apiMutating("POST /api/github/reachability", handleGitHubReachability)
	s.api("GET /api/repos", s.handleRepoProfiles)
	s.apiMutating("PATCH /api/repos/{owner}/{repo}", s.handleRepoUpdate)
	s.api("GET /api/repos/{owner}/{repo}/branches", s.handleRepoBranches)
	s.apiMutating("POST /api/jira/reachability", handleJiraReachability)
	s.apiMutating("POST /api/jira/connect", se.handleJiraConnect)
	s.api("GET /api/jira/statuses", se.handleJiraStatuses)
	s.api("GET /api/jira/stock", s.handleJiraStockGet)
	s.apiMutating("POST /api/jira/stock", s.handleJiraStockPost)

	rh := &reviewsHandler{tx: s.tx, ws: s.ws, agentRuns: s.agentRuns, ghResolver: s.ghResolver, spawner: func() *delegate.Spawner { return s.spawner }}
	s.api("GET /api/reviews/{id}", rh.handleReviewGet)
	s.apiMutating("PATCH /api/reviews/{id}", rh.handleReviewUpdate)
	s.api("GET /api/reviews/{id}/diff", rh.handleReviewDiff)
	s.apiMutating("POST /api/reviews/{id}/submit", rh.handleReviewSubmit)
	s.apiMutating("PUT /api/reviews/{id}/comments/{commentId}", rh.handleReviewCommentUpdate)
	s.apiMutating("DELETE /api/reviews/{id}/comments/{commentId}", rh.handleReviewCommentDelete)
	s.api("GET /api/agent/runs/{runID}/review", rh.handleRunReview)

	pp := &pendingPRsHandler{tx: s.tx, ws: s.ws, agentRuns: s.agentRuns, ghResolver: s.ghResolver, spawner: func() *delegate.Spawner { return s.spawner }}
	s.api("GET /api/pending-prs/{id}", pp.handlePendingPRGet)
	s.apiMutating("PATCH /api/pending-prs/{id}", pp.handlePendingPRUpdate)
	s.api("GET /api/pending-prs/{id}/diff", pp.handlePendingPRDiff)
	s.apiMutating("POST /api/pending-prs/{id}/submit", pp.handlePendingPRSubmit)
	s.api("GET /api/agent/runs/{runID}/pending-pr", pp.handleRunPendingPR)

	fh := &factoryHandler{tx: s.tx}
	s.api("GET /api/factory/snapshot", fh.handleFactorySnapshot)
	s.apiMutating("POST /api/factory/delegate", s.handleFactoryDelegate)

	ph := &promptsHandler{db: s.db, tx: s.tx}
	s.api("GET /api/event-types", ph.handleEventTypes)
	s.api("GET /api/event-schemas", handleEventSchemasList)
	s.api("GET /api/event-schemas/{event_type}", handleEventSchemaGet)
	// Unified event_handlers endpoints (SKY-259). Replace the former
	// /api/task-rules + /api/triggers split — kind is passed as ?kind=
	// on list, in the body on create, derived on update.
	eh := &eventHandlersHandler{tx: s.tx}
	s.api("GET /api/event-handlers", eh.handleEventHandlersList)
	s.apiMutating("POST /api/event-handlers", eh.handleEventHandlerCreate)
	s.apiMutating("PUT /api/event-handlers/reorder", eh.handleEventHandlerReorder)
	s.apiMutating("PATCH /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("PUT /api/event-handlers/{id}", eh.handleEventHandlerUpdate)
	s.apiMutating("DELETE /api/event-handlers/{id}", eh.handleEventHandlerDelete)
	s.apiMutating("POST /api/event-handlers/{id}/toggle", eh.handleEventHandlerToggle)
	s.apiMutating("POST /api/event-handlers/{id}/promote", eh.handleEventHandlerPromote)
	s.apiMutating("POST /api/event-handlers/{id}/retarget", eh.handleEventHandlerRetarget)
	s.api("GET /api/prompts", ph.handlePromptsList)
	s.apiMutating("POST /api/prompts", ph.handlePromptCreate)
	s.api("GET /api/prompts/{id}", ph.handlePromptGet)
	s.apiMutating("PUT /api/prompts/{id}", ph.handlePromptPut)
	s.apiMutating("DELETE /api/prompts/{id}", ph.handlePromptDelete)
	s.api("GET /api/prompts/{id}/stats", ph.handlePromptStats)
	bh := &blueprintsHandler{tx: s.tx, spawner: func() *delegate.Spawner { return s.spawner }}
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

	// Org template editor (SKY-381) — org-admin-gated, multi-mode only.
	// Mirrors the /api/prompts + /api/event-handlers families at org-template
	// scope (no team_id); each handler gates via requireOrgTemplate.
	ot := &orgTemplateHandler{tx: s.tx, az: s.az}
	s.api("GET /api/org-template/prompts", ot.handleOrgTemplatePromptsList)
	s.apiMutating("POST /api/org-template/prompts", ot.handleOrgTemplatePromptCreate)
	s.api("GET /api/org-template/prompts/{id}", ot.handleOrgTemplatePromptGet)
	s.apiMutating("PUT /api/org-template/prompts/{id}", ot.handleOrgTemplatePromptPut)
	s.apiMutating("DELETE /api/org-template/prompts/{id}", ot.handleOrgTemplatePromptDelete)
	s.api("GET /api/org-template/blueprints", ot.handleOrgTemplateBlueprintsList)
	s.apiMutating("POST /api/org-template/blueprints", ot.handleOrgTemplateBlueprintCreate)
	s.apiMutating("POST /api/org-template/blueprints/duplicate", ot.handleOrgTemplateBlueprintDuplicate)
	s.api("GET /api/org-template/blueprints/{id}", ot.handleOrgTemplateBlueprintGet)
	s.apiMutating("PUT /api/org-template/blueprints/{id}", ot.handleOrgTemplateBlueprintPut)
	s.apiMutating("DELETE /api/org-template/blueprints/{id}", ot.handleOrgTemplateBlueprintDelete)
	s.api("GET /api/org-template/blueprint-steps", ot.handleOrgTemplateBlueprintStepsAll)
	s.api("GET /api/org-template/blueprints/{id}/steps", ot.handleOrgTemplateBlueprintStepsGet)
	s.apiMutating("PUT /api/org-template/blueprints/{id}/steps", ot.handleOrgTemplateBlueprintStepsPut)
	s.apiMutating("POST /api/org-template/blueprints/{id}/merge", ot.handleOrgTemplateBlueprintMerge)
	s.apiMutating("POST /api/org-template/blueprints/{id}/split", ot.handleOrgTemplateBlueprintSplit)
	s.apiMutating("POST /api/org-template/blueprints/{id}/reconnect", ot.handleOrgTemplateBlueprintReconnect)
	s.api("GET /api/org-template/event-handlers", ot.handleOrgTemplateHandlersList)
	s.apiMutating("POST /api/org-template/event-handlers", ot.handleOrgTemplateHandlerCreate)
	s.apiMutating("PUT /api/org-template/event-handlers/reorder", ot.handleOrgTemplateHandlerReorder)
	s.apiMutating("PATCH /api/org-template/event-handlers/{id}", ot.handleOrgTemplateHandlerUpdate)
	s.apiMutating("PUT /api/org-template/event-handlers/{id}", ot.handleOrgTemplateHandlerUpdate)
	s.apiMutating("DELETE /api/org-template/event-handlers/{id}", ot.handleOrgTemplateHandlerDelete)
	s.apiMutating("POST /api/org-template/event-handlers/{id}/toggle", ot.handleOrgTemplateHandlerToggle)
	s.apiMutating("POST /api/org-template/event-handlers/{id}/promote", ot.handleOrgTemplateHandlerPromote)
	s.apiMutating("POST /api/org-template/event-handlers/{id}/retarget", ot.handleOrgTemplateHandlerRetarget)

	// GitHub App manifest registration. The launch endpoint serves a
	// script-free bounce page (carrying its own per-response CSP) that
	// POSTs the manifest cross-origin to the org's GitHub host; the
	// callback exchanges the temp code for App credentials. Both validate
	// org membership + admin role inside the handler via
	// r.PathValue("org_id"). Works in both local and multi mode.
	s.api("GET /api/orgs/{org_id}/github-app/register/launch", s.handleGitHubAppRegisterLaunch)
	s.api("GET /api/orgs/{org_id}/github-app/register/callback", s.handleGitHubAppRegisterCallback)
	// Bring-your-own-App import: the second way into App mode for orgs
	// that can't or shouldn't create the App themselves. Validates an App ID +
	// private key via an app-JWT GET /app, permission-preflights, and persists
	// through the same path the manifest callback uses (staging rule unchanged).
	// A JSON fetch from the SPA (not a top-level navigation like launch/callback),
	// so it rides apiMutating (CSRF). Org-admin (gated inside the handler).
	s.apiMutating("POST /api/orgs/{org_id}/github-app/import", s.handleGitHubAppImport)
	// Read-only status + install deep-link for the Workspace Settings panel.
	// Any org member (read), so requireOrgMember rather than requireOrgAdmin.
	s.api("GET /api/orgs/{org_id}/github-app", s.handleGitHubAppStatus)
	s.api("GET /api/orgs/{org_id}/github-app/install-url", s.handleGitHubAppInstallURL)
	// On-demand installation reconcile — the "UI panel refresh" half of D11
	// installation discovery (the poller cycle is the other). Admin-only (the
	// setup wizard's install step + the Settings App panel call it) and
	// mode-agnostic. Mutating: it reconciles the installation mirror via the
	// same API backfill the poller runs, so it rides apiMutating (CSRF).
	s.apiMutating("POST /api/orgs/{org_id}/github-app/installations/refresh", s.handleGitHubAppInstallationsRefresh)

	// GitHub access either/or transitions (TFAC-328). GitHub access is
	// strictly App XOR PAT per org; these commit the switches and surface the
	// inform-only reachability diffs. All org-admin (gated inside the handler).
	//   - cutover: commit a staged PAT→App switch (activate App + delete PAT).
	//   - switch-to-pat: full App teardown, validate + store the new PAT.
	//   - DELETE github-app: discard a staged (not-yet-live) App registration.
	//   - cutover-preflight / pat-preflight: inform-only reachability diffs.
	// The two commits + the discard mutate state (apiMutating, CSRF); the
	// cutover-preflight is a read (api); pat-preflight POSTs a token to probe
	// reach but stores nothing — still apiMutating for the same-origin guard.
	s.apiMutating("POST /api/orgs/{org_id}/github-app/cutover", s.handleGitHubAppCutover)
	s.apiMutating("POST /api/orgs/{org_id}/github-access/switch-to-pat", s.handleGitHubAccessSwitchToPAT)
	s.apiMutating("DELETE /api/orgs/{org_id}/github-app", s.handleGitHubAppDiscard)
	s.api("GET /api/orgs/{org_id}/github-app/cutover-preflight", s.handleGitHubAppCutoverPreflight)
	s.apiMutating("POST /api/orgs/{org_id}/github-access/pat-preflight", s.handleGitHubAccessPATPreflight)

	// "Connect GitHub" user-to-server OAuth — binds a host-verified GitHub
	// login to the signed-in user (identity, not access, not login).
	// start redirects to {github_base_url}/login/oauth/authorize;
	// callback exchanges the code, whoamis, and writes
	// user_github_identities(source='connect_oauth'). Both are GETs reached
	// via top-level navigation (the start from the gate page, the callback
	// from GitHub's redirect), so they ride s.api (withSession, no CSRF wrap)
	// and carry their own OAuth state-cookie CSRF defense. Any org member
	// binds their own identity. The identity-status read backs the gate.
	s.api("GET /api/orgs/{org_id}/github/connect/start", s.handleGitHubConnectStart)
	s.api("GET /api/orgs/{org_id}/github/connect/callback", s.handleGitHubConnectCallback)
	s.api("GET /api/orgs/{org_id}/identity/github", s.handleGitHubIdentityStatus)
	// Capture-and-discard per-user identity from a user-supplied PAT (PAT_2):
	// validate → whoami → write user_github_identities → drop the token. The
	// always-available fallback to Connect (and the only path when no App is
	// registered). Never stores the token.
	s.apiMutating("POST /api/orgs/{org_id}/identity/github/pat", s.handleGitHubIdentityPAT)

	// Per-user Jira access — the Jira sibling of the GitHub identity flow
	// (jira_connect.go). status reports connected from a STORED credential
	// (Jira's user level holds access, not just identity); the PAT path
	// validates the token, STORES it (per-user vault scope), and derives the
	// user's Jira identity. DC = paste-a-PAT; Cloud OAuth is a later ticket
	// (connect_available stays false). Any org member binds their own access.
	s.api("GET /api/orgs/{org_id}/identity/jira", s.handleJiraIdentityStatus)
	s.apiMutating("POST /api/orgs/{org_id}/identity/jira/pat", s.handleJiraIdentityPAT)

	// Per-org GitHub App webhook receiver. Pre-auth (GitHub has no
	// session) and identified by org_id from the path; the handler
	// verifies the HMAC signature against that org's stored webhook
	// secret before any side effect, so it's on the preAuthAllowlist.
	s.mux.Handle("POST /api/webhooks/github/{org_id}", http.HandlerFunc(s.handleGitHubWebhook))

	// Frontend: serve embedded SPA, with fallback to index.html for client-side routing
	s.mux.HandleFunc("/", s.handleFrontend)
}

// handleFrontend serves the embedded React SPA. Non-file requests fall back to index.html
// so that client-side routing works.
func (s *Server) handleFrontend(w http.ResponseWriter, r *http.Request) {
	if s.static == nil {
		http.Error(w, "frontend not built — run: cd frontend && npm run build", http.StatusNotFound)
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
		log.Printf("[server] inline script hash compute failed: %v (CSP will block inline scripts)", err)
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

// SetOnGitHubChanged registers a callback for GitHub config changes (creds, URL, repos).
// This triggers a full restart: invalidate profiles → stop all pollers → re-profile → restart.
// The orgID is the tenant whose creds changed — closure re-resolves via SecretStore.
//
// The registered callback is wrapped so the reachable-repo enumeration cache
// (SKY-409) is evicted for the org *before* the restart logic runs: a creds
// rotation, App install, or repo-set change can move which repos the org can
// reach, and a stale cached enumeration must never satisfy the next write.
//
// Handlers fire this callback in a goroutine (the restart is slow), so the
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

// SetEventBus wires the in-process event bus so the GitHub webhook
// receiver can publish verified deliveries. Wired post-construction in
// main.go after the bus is created.
func (s *Server) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

// SetInstallationRemovedHook registers a callback fired on a verified
// installation.deleted webhook (the resolver's token-cache invalidate).
// Optional — left nil until the credential resolver wires it.
func (s *Server) SetInstallationRemovedHook(fn func(orgID, installationID string)) {
	s.onInstallationRemoved = fn
}

// MarkJiraRestarted records the moment the Jira poller was restarted. Clears
// the last-poll timestamp so jiraPollReady reports false until a completion
// event arrives. Call this before kicking off a Jira poller restart.
func (s *Server) MarkJiraRestarted() {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	s.jiraRestartedAt = time.Now()
	s.jiraLastPollAt = time.Time{}
}

// MarkJiraPollComplete records a successful Jira poll cycle. Call from the
// event-bus subscriber on system:poll:completed when source == "jira".
// startedAt is the wall-clock time the poll cycle started; completions from
// poll goroutines that started before the most recent MarkJiraRestarted are
// ignored so an in-flight pre-restart poll can't incorrectly flip readiness
// back to true.
//
// A zero startedAt means the emitter didn't supply a start time (metadata
// field missing or the event came from a publisher unaware of the race
// guard). Accept those completions so a malformed/future event can't leave
// carry-over stuck on {status:"polling"} indefinitely — race protection
// degrades gracefully rather than silently failing open.
func (s *Server) MarkJiraPollComplete(startedAt time.Time) {
	s.jiraPollMu.Lock()
	defer s.jiraPollMu.Unlock()
	if !startedAt.IsZero() && startedAt.Before(s.jiraRestartedAt) {
		return
	}
	s.jiraLastPollAt = time.Now()
}

// jiraPollReady returns true when the poller has completed at least one cycle
// since the last restart. Used by /api/jira/stock to gate the list response.
func (s *Server) jiraPollReady() bool {
	s.jiraPollMu.RLock()
	defer s.jiraPollMu.RUnlock()
	return !s.jiraLastPollAt.IsZero() && s.jiraLastPollAt.After(s.jiraRestartedAt)
}

// --- Stub handlers (to be implemented) ---

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

// Prompt handlers are in prompts_handler.go
// Skill import handler is in skills_handler.go
