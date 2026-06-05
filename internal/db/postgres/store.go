// Package postgres is the Postgres-backed implementation of the
// per-resource store interfaces declared in package db. Multi-tenant
// installs of triagefactory wire this implementation at startup
// (local-mode wires internal/db/sqlite). See the SKY-246 D2 spec at
// docs/specs/sky-246-d2-store-abstraction.html for the full design,
// and the D3 schema at internal/db/migrations-postgres/.
//
// # Two-connection design
//
// New(admin, app) takes two *sql.DB handles. They serve different
// roles:
//
//   - admin: superuser / supabase_admin pool. RLS is bypassed for
//     this role. Used by (a) deploy-time operations like migrations
//     and system-prompt seeding, and (b) server-side system services
//     that need to read/write across users in an org without
//     impersonating each one (the AI scorer is the canonical
//     example — it has no user identity but must operate on every
//     queued task in the org).
//
//   - app: authenticator → tf_app role. RLS-active. Used by request
//     handlers; the TxRunner sets request.jwt.claims so policies
//     see (orgID, userID).
//
// Per-resource stores choose which queryer they wire against based
// on whether they serve request handlers or system services.
package postgres

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Store holds the two Postgres connection pools + the bundle of
// resource-store implementations wired against them. New returns the
// assembled db.Stores bundle for application wiring; downstream
// consumers such as handlers should depend on the specific store
// interfaces they need rather than the whole bundle.
type Store struct {
	admin *sql.DB
	app   *sql.DB

	stores db.Stores
}

// New wires a db.Stores bundle backed by Postgres. Wave 0 ships only
// ScoreStore + the TxRunner; subsequent waves populate the remaining
// 21 fields on the bundle.
//
// admin is the superuser pool (RLS bypassed); app is the tf_app
// authenticator pool (RLS-active). ScoreStore wires against admin
// because the scorer is a system service operating across all users
// in each org. Request-handler stores (added in later waves) wire
// against app and rely on WithTx to set per-request JWT claims.
func New(admin, app *sql.DB) db.Stores {
	s := &Store{admin: admin, app: app}
	// Built once and shared: GitHubApps.BackfillInstallationsFromAPI reads
	// the App PEM via the same SecretStore the bundle exposes (GetSystem,
	// admin pool).
	secrets := newSecretStore(app, admin)
	s.stores = db.Stores{
		Scores: newScoreStore(admin),
		// PromptStore needs both pools: SeedOrUpdate writes to
		// system_prompt_versions (REVOKE'd from tf_app — admin only),
		// every other method runs on the app pool. The impl picks
		// per-method internally.
		Prompts:   newPromptStore(app, admin),
		Swipes:    newSwipeStore(app),
		Dashboard: newDashboardStore(app),
		// Secrets needs both pools. Put/Get/Delete wrap the
		// public.vault_* SECURITY DEFINER functions GRANTed to tf_app
		// (app pool) — the caller must have set request.jwt.claims so
		// the wrapper's p_org_id == tf.current_org_id() gate passes.
		// GetSystem wraps vault_get_org_secret_system on the admin pool
		// (supabase_admin) for background/system callers with no JWT;
		// tf_app has no EXECUTE on that function.
		Secrets: secrets,
		// EventHandlers needs both pools: Seed writes shipped rows
		// without JWT claims, but event_handlers_insert /
		// event_handlers_update RLS policies gate on either
		// creator_user_id = tf.current_user_id() or
		// tf.user_is_org_admin() on org-visible writes. The impl
		// routes Seed to admin (BYPASSRLS) and every CRUD method to
		// app — same pool-split pattern PromptStore + the predecessor
		// stores used.
		EventHandlers: newEventHandlerStore(app, admin),
		// Blueprints wires both pools. CreateRun routes internally on
		// trigger_type (event → admin with NULL creator, manual → app
		// with COALESCE fallback), mirroring AgentRunStore.Create. The
		// `...System` variants on the read/write methods (ListSteps,
		// MarkRunStatus, RunsForBlueprint) give the blueprint orchestrator
		// goroutine an admin-pool route for its detached-context work.
		Blueprints: newBlueprintStore(app, admin),
		// Agents.Create routes through admin (bootstrap has no JWT
		// claims and the agents_insert policy gates on
		// tf.user_is_org_admin); every other method on app. Same
		// pool-split pattern as PromptStore + TaskRuleStore.
		Agents: newAgentStore(app, admin),
		// TeamAgents.AddForTeam routes through admin for the same
		// bootstrap reason; SetEnabled/Overrides/Remove/Get run on
		// app where RLS gates by team membership.
		TeamAgents: newTeamAgentStore(app, admin),
		// Users wires both pools (SKY-296): app for request-equivalent
		// reads/writes (RLS gated by tf.user_can_read_user() /
		// tf.user_can_update_user()), admin for the poller bootstrap's
		// GetGitHubLoginSystem read at startup. Row creation is an
		// auth-flow concern owned by SKY-251.
		Users: newUsersStore(app, admin),
		// Tasks wires both pools (SKY-297): app for request-equivalent
		// consumers (server tasks handler, router, delegate) and admin
		// for the tracker's stale-review reconciliation read via
		// FindActiveByEntityAndTypeSystem. The AI scorer still uses
		// the admin-pooled ScoreStore for its system-service reads
		// rather than going through TaskStore.
		Tasks: newTaskStore(app, admin),
		// Factory wires admin — the snapshot is a system-level view
		// (no per-user identity, must see every in-flight run
		// regardless of creator).
		Factory: newFactoryReadStore(admin),
		// AgentRuns wires app — every consumer is request-
		// equivalent (server agent handler, delegate spawner
		// goroutine spawned from a handler, chains). System-service
		// reads of run state are routed through the admin-pooled
		// FactoryReadStore instead.
		// AgentRuns holds both pools. Manual-trigger Create + every
		// other method run on app (RLS-active). Event-triggered
		// Create routes to admin because the CHECK + RLS policy
		// pair makes that insert unreachable through tf_app — see
		// the impl's Create comment.
		AgentRuns: newAgentRunStore(app, admin),
		// Entities wires both pools (SKY-296): app for request-
		// equivalent consumers (server panels, delegate context
		// loaders) and admin for the `...System` variants the tracker
		// + project classifier use. RLS policy entities_all gates
		// reads + writes on (org_id = tf.current_org_id() AND
		// tf.user_has_org_access) on the app side; admin bypasses
		// RLS, and org_id stays in every WHERE clause as defense
		// in depth.
		Entities: newEntityStore(app, admin),
		// Reviews wires both pools: app for request-equivalent
		// consumers (reviews handler, swipe-dismiss, agent submit-
		// review via cmd/exec/gh), admin for ByRunIDSystem — the
		// delegate spawner's processCompletion reads pending reviews
		// from a goroutine that has detached from the request
		// context. RLS policies pending_reviews_all +
		// pending_review_comments_all gate the app side; admin
		// bypasses RLS, and org_id stays in every WHERE clause as
		// defense in depth.
		Reviews: newReviewStore(app, admin),
		// PendingPRs wires both pools: app for request-equivalent
		// consumers (pending_prs handler, swipe-dismiss cleanup,
		// agent gh-create-pr tool via cmd/exec), admin for
		// ByRunIDSystem — same goroutine-detached read path as
		// ReviewStore.ByRunIDSystem. RLS policy pending_prs_all gates
		// the app side via the runs subquery; admin bypasses RLS,
		// and org_id stays in every WHERE clause as defense in depth.
		PendingPRs: newPendingPRStore(app, admin),
		// Repos wires both pools (SKY-296): app for request-
		// equivalent consumers (repos/settings/projects handlers,
		// curator) and admin for the `...System` variants the
		// poller bootstrap + startup clone-status writes use. RLS
		// policy repo_profiles_all gates on (org_id = current_org_id()
		// AND user_has_org_access) on the app side; admin bypasses
		// RLS, and org_id stays in every WHERE clause as defense
		// in depth.
		Repos: newRepoStore(app, admin),
		// PendingFirings wires admin — the router has no per-user
		// identity (system service) and the drain sweeper runs as a
		// background goroutine, so impersonating any one user via
		// the app pool would be wrong. RLS still gates statements
		// via an EXISTS subquery against tasks; org_id defense-in-
		// depth fires in every WHERE/INSERT clause regardless.
		PendingFirings: newPendingFiringsStore(admin),
		// Projects wires both pools (SKY-297): app for request-equivalent
		// consumers and admin for ListSystem, the project classifier's
		// cross-org read. projects_* RLS policies gate the app side
		// by visibility + team membership; admin bypasses RLS, and
		// org_id stays in every WHERE clause as defense in depth.
		Projects: newProjectStore(app, admin),
		// Events wires both pools (SKY-305): app for request-handler
		// equivalents (stock carry-over, factory drag-to-delegate) and
		// admin for background goroutines without JWT-claims context
		// (router RecordSystem + re-derive, delegate post-run metadata
		// enrichment). events_all RLS gates the app side; admin
		// bypasses, org_id is bound everywhere as defense in depth.
		Events: newEventStore(app, admin),
		// EventQueue is admin-pool only: the ingestor and
		// drain worker are system services with no JWT-claims context.
		// The store self-manages the Enqueue transaction, so it holds
		// the admin *sql.DB directly. event_queue_all RLS is
		// defense-in-depth (admin bypasses it).
		EventQueue: newEventQueueStore(admin),
		// RunQueue is admin-pool only: the dispatcher is a system worker with
		// no JWT-claims context. The claim uses FOR UPDATE SKIP LOCKED so a
		// future multi-worker dispatcher never double-claims a queued run.
		RunQueue: newRunQueueStore(admin),
		// TaskMemory wires both pools: app for request-handler
		// equivalents (review/PR submit, swipe-discard cleanup,
		// factory + run-summary reads) and admin for the delegate
		// spawner's runAgent goroutine — the post-completion gate
		// teardown's UpsertAgentMemorySystem and the run-start
		// GetMemoriesForEntitySystem both fire without a JWT-claims
		// context. run_memory_all RLS gates the app side via an
		// EXISTS subquery against runs; admin bypasses RLS, and
		// org_id stays in every WHERE clause as defense in depth.
		TaskMemory: newTaskMemoryStore(app, admin),
		// RunWorktrees wires both pools: app for cmd/exec workspace
		// callers (a separate cmd/exec auth pass owns the
		// synthetic-claims wrap) and admin for the delegate spawner
		// cleanup defers. org_id stays bound everywhere as defense
		// in depth.
		RunWorktrees: newRunWorktreeStore(app, admin),
		// Orgs holds both pools: admin for ListActiveSystem +
		// GetSettingsSystem (background services iterating the active
		// org set / reading per-org settings without JWT claims) and
		// app for GetSettings + UpdateSettings (request-handler
		// reads/writes gated by org_settings_* RLS policies).
		Orgs: newOrgsStore(app, admin),
		// Teams holds both pools: admin for GetDefaultForOrgSystem +
		// GetSettingsSystem (boot-time pollers/scorer/delegation
		// without JWT claims) and app for GetSettings +
		// UpdateSettings (request-handler reads/writes gated by
		// team_settings_* RLS policies).
		Teams: newTeamsStore(app, admin),
		// JiraStatusRules holds both pools: app for ListForTeam +
		// ReplaceForTeam (request-handler reads/writes gated by
		// jira_rules_* RLS policies) and admin for ListForTeamSystem
		// (poller manager + scorer reads at boot/poll-tick without
		// JWT claims).
		JiraStatusRules: newJiraStatusRulesStore(app, admin),
		// TeamGitHubGroups holds both pools: app for ListForTeam +
		// SetForTeam + TeamsForGroup (request-handler reads/writes gated
		// by team_github_groups_* RLS policies) and admin for
		// ListForTeamSystem + TeamsForGroupSystem + PruneMissingSystem
		// (router/poller routing + GitHub-team-deletion reconcile
		// without JWT claims).
		TeamGitHubGroups: newTeamGitHubGroupsStore(app, admin),
		// TeamGitHubRepos holds both pools: app for ListForTeam + the
		// team-row write inside ReplaceForTeam (RLS gates by team admin)
		// and admin for ListForTeamSystem + ListForOrgSystem +
		// TracksRepoSystem (router gate, no JWT claims) + the
		// repo_profiles reconcile (org-wide union, commits autonomously).
		TeamGitHubRepos: newTeamGitHubReposStore(app, admin),
		// Curator wires the app pool. The per-project goroutine
		// wraps each turn's writes in Tx.SyntheticClaimsWithTx
		// under the requesting user's identity; the tx-bound
		// variant composed inside tx.go's txStoresFromTx body is
		// what actually services those calls. The handler-side
		// CreateRequest / GetRequest reads (where the request has
		// a user identity but not yet via the D9 context plumb)
		// also route through this app-pool wiring.
		// Curator holds both pools: app for per-turn writes (claims-
		// bound via SyntheticClaimsWithTx, RLS gates on the
		// (org_id, creator_user_id) pair), admin for the boot-time
		// CancelOrphanedNonTerminalRequests sweep that runs before
		// any JWT-claims context exists.
		Curator: newCuratorStore(app, admin),
		// GitHubApps: app pool for request-handler reads/writes
		// (RLS-gated); admin pool for installation-mirror writes (tf_app
		// is denied all writes to org_github_app_installations) + the
		// no-claims reads the webhook receiver + backfill need; secrets
		// for the backfill's App-PEM GetSystem read.
		GitHubApps: newGitHubAppsStore(app, admin, secrets),
		// OrgTemplate needs both pools: the editor CRUD runs on app
		// (org_template_*_all RLS gates on tf.user_is_org_admin), while
		// SeedFromShipped + MaterializeIntoTeam run on admin (claims-less
		// bootstrap; MaterializeIntoTeam also writes the team's
		// prompts/event_handlers/system_prompt_versions). The impl picks
		// per-method internally — same split as PromptStore.
		OrgTemplate: newOrgTemplateStore(app, admin),
		Tx:          s,
	}
	return s.stores
}

// Connection openers (OpenAdmin, OpenApp) are NOT defined here in
// wave 0. main.go fatals before reaching them; introducing them now
// would require registering the pgx stdlib driver inside this
// package (a side-effect import) without any caller exercising it.
// SKY-251 (D7) owns the multi-mode startup wiring and will add the
// openers alongside the config + DSN plumbing that actually consumes
// them. Tests construct *sql.DB via the pgtest harness, which
// registers the pgx driver itself.

// NewForTx returns a db.TxStores wired against one *sql.Tx — the
// same shape WithTx produces internally for its closure body,
// exposed so tests can drive store methods against a claims-set
// transaction without going through a WithTx callback. The most
// prominent caller is the SecretStore test, where the vault
// wrapper refuses calls without a matching JWT claim.
//
// Returns db.TxStores (not db.Stores) deliberately: db.Stores
// carries a TxRunner, and a Stores{Tx: nil} would panic on
// stores.Tx.WithTx(...). TxStores has no Tx field, so misuse is
// a compile error rather than a runtime crash. Production code
// reaches the same wiring via Store.WithTx; this helper is the
// test-side door into it.
func NewForTx(tx *sql.Tx) db.TxStores {
	return db.TxStores{
		Scores:        newScoreStore(tx),
		Prompts:       newTxPromptStore(tx),
		Swipes:        newSwipeStore(tx),
		Dashboard:     newDashboardStore(tx),
		Secrets:       newSecretStore(tx, tx),
		EventHandlers: newTxEventHandlerStore(tx),
		Blueprints:    newBlueprintStore(tx, tx),
		Agents:        newTxAgentStore(tx),
		TeamAgents:    newTxTeamAgentStore(tx),
		Users:         newUsersStore(tx, tx),
		Tasks:         newTaskStore(tx, tx),
		Factory:       newFactoryReadStore(tx),
		// NewForTx is a test door — both pools collapse to the
		// supplied tx. Tests that exercise the admin-only branch
		// (event-triggered AgentRunStore.Create, or any of the
		// SKY-296 `...System` methods that bypass RLS in
		// production) need the production WithTx wiring instead,
		// which gets the real admin pool via Store.admin.
		AgentRuns:        newAgentRunStore(tx, tx),
		Entities:         newEntityStore(tx, tx),
		Repos:            newRepoStore(tx, tx),
		Reviews:          newReviewStore(tx, tx),
		PendingPRs:       newPendingPRStore(tx, tx),
		PendingFirings:   newPendingFiringsStore(tx),
		Projects:         newProjectStore(tx, tx),
		Events:           newEventStore(tx, tx),
		TaskMemory:       newTaskMemoryStore(tx, tx),
		RunWorktrees:     newRunWorktreeStore(tx, tx),
		Orgs:             newOrgsStore(tx, tx),
		Teams:            newTeamsStore(tx, tx),
		JiraStatusRules:  newJiraStatusRulesStore(tx, tx),
		TeamGitHubGroups: newTeamGitHubGroupsStore(tx, tx),
		TeamGitHubRepos:  newTeamGitHubReposStore(tx, tx),
		Curator:          newCuratorStore(tx, tx),
		// Both pools collapse to tx (test door). BackfillInstallationsFromAPI's
		// GetSystem would hit tf_app and be denied here — tests that exercise
		// it use New(admin, app) directly, same as the SecretStore tests.
		GitHubApps:  newGitHubAppsStore(tx, tx, newSecretStore(tx, tx)),
		OrgTemplate: newTxOrgTemplateStore(tx),
	}
}
