// Package postgres is the Postgres-backed implementation of the
// per-resource store interfaces declared in package db. Multi-tenant
// installs of triagefactory wire this implementation at startup
// (local-mode wires internal/db/sqlite). See the D2 spec at
// docs/for-agents/specs/sky-246-d2-store-abstraction.html for the full design,
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

	"github.com/sky-ai-eng/triage-factory/internal/aead"
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

	// secretKey is the app-layer AES-256-GCM key the SecretStore
	// encrypts org/user integration secrets with (TFAC-402). Held on
	// the Store so txStoresFromTx can rebuild a tx-bound SecretStore
	// with the same key inside WithTx. nil on TF_ROLE=executor
	// (NewWithoutSecrets, TFAC-614) — buildSecrets returns the disabled
	// store in that case, so every tx-bound rebuild stays gated too, not
	// just the top-level Stores.Secrets field.
	secretKey *aead.Key

	stores db.Stores
}

// buildSecrets returns the real SecretStore bound to (app, admin) when this
// Store holds a key, or the disabled store (TFAC-614) when it doesn't. Every
// SecretStore construction in this package — the top-level bundle and both
// tx-bound rebuilds in tx.go — routes through this so an executor-role
// Store can never end up holding a real, if pointlessly zero-keyed,
// decrypting store via some internal call site that forgot to check.
func (s *Store) buildSecrets(app, admin queryer) db.SecretStore {
	if s.secretKey == nil {
		return newDisabledSecretStore()
	}
	return newSecretStore(app, admin, *s.secretKey)
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
//
// secretKey is the app-layer AES-256-GCM key the SecretStore encrypts
// org/user integration secrets with (TFAC-402); multi-mode boot loads it
// from TF_SECRET_ENCRYPTION_KEY in internal/app/stores.go.
func New(admin, app *sql.DB, secretKey aead.Key) db.Stores {
	return newStoreBundle(admin, app, &secretKey)
}

// NewWithoutSecrets builds the same Stores bundle as New, but wires
// db.ErrSecretStoreUnavailable's disabled SecretStore instead of a real
// decrypting one (TFAC-614). For TF_ROLE=executor only: an executor never
// holds TF_SECRET_ENCRYPTION_KEY at boot — all per-run credential material
// arrives pre-resolved via sealed claim_credentials bundles instead — so it
// must never construct a real, key-bearing SecretStore, not even one
// pointed at a throwaway key.
func NewWithoutSecrets(admin, app *sql.DB) db.Stores {
	return newStoreBundle(admin, app, nil)
}

// newStoreBundle is the shared body New and NewWithoutSecrets wire against;
// secretKey nil selects the disabled SecretStore (buildSecrets) throughout,
// including both tx-bound rebuilds in tx.go.
func newStoreBundle(admin, app *sql.DB, secretKey *aead.Key) db.Stores {
	s := &Store{admin: admin, app: app, secretKey: secretKey}
	// Built once and shared: GitHubApps.BackfillInstallationsFromAPI reads
	// the App PEM via the same SecretStore the bundle exposes (GetSystem,
	// admin pool).
	secrets := s.buildSecrets(app, admin)
	// Built once and shared with ShippedDefaults below: SeedShippedIntoTeam's
	// phase 3 (handlers) delegates to EventHandlers.Seed rather than
	// duplicating its SQL.
	eventHandlers := newEventHandlerStore(app, admin)
	s.stores = db.Stores{
		Scores: newScoreStore(admin),
		// PromptStore needs both pools: the ...System reads (GetSystem,
		// IncrementUsageSystem) run on the admin pool for claims-less
		// delegation goroutines, every other method on the app pool. The
		// impl picks per-method internally.
		Prompts: newPromptStore(app, admin),
		// Swipes needs both pools: UndoLastSwipe's guard reads swipe_events
		// rows the requesting user did not write, which RLS hides, so that
		// one method runs on the admin pool. Everything else stays on app.
		Swipes:    newSwipeStore(app, admin),
		Dashboard: newDashboardStore(app),
		// Secrets needs both pools. Put/Get/Delete + the per-user trio
		// run on the app pool against public.org_secrets — RLS gates the
		// org (and, for user rows, the user), so the caller must have set
		// request.jwt.claims first. GetSystem / GetUserSystem /
		// PutUserSystem run on the admin pool (supabase_admin bypasses
		// RLS) for background/system callers with no JWT. Values are
		// AES-256-GCM-encrypted app-side with secretKey (TFAC-402).
		Secrets: secrets,
		// EventHandlers needs both pools: Seed writes shipped rows
		// without JWT claims, but event_handlers_insert /
		// event_handlers_update RLS policies gate on either
		// creator_user_id = tf.current_user_id() or
		// tf.user_is_org_admin() on org-visible writes. The impl
		// routes Seed to admin (BYPASSRLS) and every CRUD method to
		// app — same pool-split pattern PromptStore + the predecessor
		// stores used.
		EventHandlers: eventHandlers,
		// Blueprints wires both pools. CreateRun routes internally on
		// trigger_type (event → admin with NULL creator, manual → app
		// with COALESCE fallback), mirroring ConversationStore.Create. The
		// `...System` variants on the read/write methods (ListSteps,
		// MarkRunStatus, ConversationsForBlueprint) give the blueprint orchestrator
		// goroutine an admin-pool route for its detached-context work.
		Blueprints: newBlueprintStore(app, admin),
		// Agents.Create routes through admin (bootstrap has no JWT
		// claims and the agents_insert policy gates on
		// tf.user_is_org_admin); every other method on app. Same
		// pool-split pattern as PromptStore + EventHandlerStore.
		Agents: newAgentStore(app, admin),
		// TeamAgents.AddForTeam routes through admin for the same
		// bootstrap reason; SetEnabled/Overrides/Remove/Get run on
		// app where RLS gates by team membership.
		TeamAgents: newTeamAgentStore(app, admin),
		// Users wires both pools: app for request-equivalent
		// reads/writes (RLS gated by tf.user_can_read_user() /
		// tf.user_can_update_user()), admin for the poller bootstrap's
		// GetGitHubLoginSystem read at startup. Row creation is an
		// auth-flow concern owned separately.
		Users: newUsersStore(app, admin),
		// Tasks wires both pools: app for request-equivalent
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
		// TeamActivity is request-equivalent (member-gated, like Dashboard),
		// not a system-level view — it wires app here so a caller reaching
		// this top-level bundle directly (bypassing WithTx and the
		// membership check the handler does) hits authenticator's lack of
		// grants instead of quietly reading past RLS on the admin pool. The
		// real, request-scoped access path is the tx-bound handle in
		// txStoresFromTx.
		TeamActivity: newTeamActivityStore(app),
		// TeamPRs needs both pools: the page runs on app (the tracked-set
		// semi-join is bounded by team_github_repos RLS), the member-login
		// lookup on admin (user_github_identities_select is self-only, so
		// under RLS a team's list would collapse to the caller's own).
		TeamPRs: newTeamPRStore(app, admin),
		// Conversations wires app — every consumer is request-
		// equivalent (server agent handler, delegate spawner
		// goroutine spawned from a handler, chains). System-service
		// reads of run state are routed through the admin-pooled
		// FactoryReadStore instead.
		// Conversations holds both pools. Manual-trigger Create + every
		// other method run on app (RLS-active). Event-triggered
		// Create routes to admin because the CHECK + RLS policy
		// pair makes that insert unreachable through tf_app — see
		// the impl's Create comment.
		Conversations: newConversationStore(app, admin),
		// Artifacts wires both pools: app for request-equivalent
		// writers (manual-run exec choke point under synthetic claims)
		// and readers (run-detail, C2), admin for UpsertSystem — the
		// event-triggered exec choke point has no creator user, so its
		// insert can't pass artifacts_insert's team-write check on the
		// app side (TFAC-459). artifacts_* RLS scopes by team_id like
		// runs; org_id stays in every clause as defense in depth.
		Artifacts: newArtifactStore(app, admin),
		// Entities wires both pools: app for request-
		// equivalent consumers (server panels, delegate context
		// loaders) and admin for the `...System` variants the tracker
		// uses. RLS policy entities_all gates
		// reads + writes on (org_id = tf.current_org_id() AND
		// tf.user_has_org_access) on the app side; admin bypasses
		// RLS, and org_id stays in every WHERE clause as defense
		// in depth.
		Entities: newEntityStore(app, admin),
		// Repos wires both pools: app for request-
		// equivalent consumers (repos/settings/projects handlers)
		// and admin for the `...System` variants the
		// poller bootstrap + startup clone-status writes use. RLS
		// policy repositories_all gates on (org_id = current_org_id()
		// AND user_has_org_access) on the app side; admin bypasses
		// RLS, and org_id stays in every WHERE clause as defense
		// in depth.
		Repos: newRepositoryStore(app, admin),
		// PendingFirings wires admin — the router has no per-user
		// identity (system service) and the drain sweeper runs as a
		// background goroutine, so impersonating any one user via
		// the app pool would be wrong. RLS still gates statements
		// via an EXISTS subquery against tasks; org_id defense-in-
		// depth fires in every WHERE/INSERT clause regardless.
		PendingFirings: newPendingFiringsStore(admin),
		// Events wires both pools: app for request-handler
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
		// ConversationQueue is admin-pool only: the dispatcher is a system worker with
		// no JWT-claims context. The claim uses FOR UPDATE SKIP LOCKED so a
		// future multi-worker dispatcher never double-claims a queued run.
		ConversationQueue: newConversationQueueStore(admin),
		// TaskMemory wires both pools: app for request-handler
		// equivalents (review/PR submit, swipe-discard cleanup,
		// factory + run-summary reads) and admin for the delegate
		// spawner's runAgent goroutine — the post-completion gate
		// teardown's UpsertAgentMemorySystem and the run-start
		// GetMemoriesForEntitySystem both fire without a JWT-claims
		// context. conversation_memory_all RLS gates the app side via an
		// EXISTS subquery against conversations; admin bypasses RLS, and
		// org_id stays in every WHERE clause as defense in depth.
		TaskMemory: newTaskMemoryStore(app, admin),
		// ConversationWorktrees wires both pools: app for cmd/exec workspace
		// callers (a separate cmd/exec auth pass owns the
		// synthetic-claims wrap) and admin for the delegate spawner
		// cleanup defers. org_id stays bound everywhere as defense
		// in depth.
		ConversationWorktrees: newConversationWorktreeStore(app, admin),
		// Orgs holds both pools: admin for ListActiveSystem +
		// GetSettingsSystem (background services iterating the active
		// org set / reading per-org settings without JWT claims) and
		// app for GetSettings + UpdateSettings (request-handler
		// reads/writes gated by org_settings_* RLS policies).
		Orgs: newOrgsStore(app, admin),
		// OrgMemberships holds both pools: app for the RLS-gated roster read +
		// the role/remove mutations, admin for the cross-member GitHub/Jira
		// identity enrichment the self-only identity RLS can't express (scoped
		// to the org by a membership join). See the store comment.
		OrgMemberships: newOrgMembershipsStore(app, admin),
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
		// repositories reconcile (org-wide union, commits autonomously).
		TeamGitHubRepos: newTeamGitHubReposStore(app, admin),
		// GitHubApps: app pool for request-handler reads/writes
		// (RLS-gated); admin pool for installation-mirror writes (tf_app
		// is denied all writes to org_github_app_installations) + the
		// no-claims reads the webhook receiver + backfill need; secrets
		// for the backfill's App-PEM GetSystem read.
		GitHubApps: newGitHubAppsStore(app, admin, secrets),
		// ReachableRepos: admin pool only. The reachable-repo cache carries the
		// same RLS posture as the installation rows its App half hangs off —
		// app-pool members read it, every app-pool write is denied — so the
		// refresh is the sole writer and there is no app-pool caller to wire.
		ReachableRepos: newReachableReposStore(admin),
		// GitHubDeliveries: admin pool only. The webhook receiver is pre-auth,
		// and github_webhook_deliveries has RLS enabled with no policy at all,
		// so there is no app-pool caller to wire.
		GitHubDeliveries: newGitHubDeliveryStore(admin),
		// GitHubPendingBinds: admin pool only. The consume finds its row by
		// nonce hash and learns the org from it, so there is no org for an RLS
		// policy to gate on before the read; github_pending_binds has RLS
		// enabled with no policy at all, and the nonce is the authorization.
		GitHubPendingBinds: newGitHubPendingBindStore(admin),
		// JiraApps: app pool for request-handler reads/writes (RLS-gated);
		// admin pool for the no-claims read the OAuth-app resolver needs.
		JiraApps: newJiraAppsStore(app, admin),
		// ShippedDefaults is admin-pool only (claims-less bootstrap work).
		// Phase 3 (handlers) reuses the eventHandlers store built above
		// instead of duplicating its Seed SQL.
		ShippedDefaults: newShippedDefaultsStore(admin, eventHandlers),
		// Invites needs both pools: app for the admin-facing create/list/
		// revoke (org_invites_{select,insert,update} RLS gates on org-admin),
		// admin for the redeem reads (GetByTokenHashSystem +
		// IsOrgMemberSystem), whose actor is a token-bearing outsider with no
		// membership. Same pool-split pattern as TeamsStore.
		Invites: newInvitesStore(app, admin),
		// SystemLLMRuns is admin-pool only: the writers (scorer,
		// repo-profiler) are boot-launched system
		// goroutines with no JWT-claims context, so an app-pool INSERT
		// under system_llm_runs_all RLS would be rejected. Same admin-only
		// shape as PendingFirings / EventQueue.
		SystemLLMRuns: newSystemLLMRunStore(admin),
		// AccessChangeLog holds both pools (like ExternalActions): app for Record
		// (governance handlers, composing inside their claims-bearing WithTx so the
		// access_change_log_all RLS policy gates the write + the audit view's read
		// by org) + ListByOrg, admin for RecordSystem (the SSO JIT auto-provision
		// seam, whose user has no claims/membership at grant time). See TFAC-471 /
		// TFAC-486.
		AccessChangeLog: newAccessChangeLogStore(app, admin),
		// ExternalActions holds both pools (like Artifacts): app for Record
		// (manual bot runs + server approval/board handlers, under claims) +
		// ListByTeam, admin for RecordSystem (event-triggered runs + the Jira
		// mirror, no claims) + ListByOrgSystem (the org-wide governance read).
		// Append-only audit log of org-credential external writes. See TFAC-483.
		ExternalActions: newExternalActionStore(app, admin),
		// Spend holds both pools: app for ListSpend + SpendByCategory (the
		// llm_spend view is security_invoker, so reading it under tf_app applies
		// the base tables' RLS — team-scoped runs + org-scoped system),
		// admin for SpendByCategorySystem (the org-wide aggregate the TFAC-477
		// safety cap reads from a claims-less Spawner.Delegate goroutine). Read-
		// only; owns no table. See TFAC-472 / TFAC-477.
		Spend: newSpendStore(app, admin),
		// AuthEvents is admin-pool only: auth_events is a system table (RLS
		// deny-by-default, REVOKEd from the app roles like user_identities) — its
		// writes + reads never carry user claims and org_id is frequently NULL, so
		// an app-pool statement would be rejected. Same admin-only shape as
		// SystemLLMRuns. SOC2 authentication audit log of record. See TFAC-76.
		AuthEvents: newAuthEventStore(admin),
		// StagedInjections is admin-pool only: both the producer (an eventbus
		// subscriber) and the consumer (a detached resume goroutine) run without
		// JWT claims, so an app-pool/RLS statement would see nothing. The table
		// still inherits runs' team-scoped RLS via its FK; org_id stays bound as
		// defense in depth. Durable "stage for next resume" queue. See TFAC-501.
		StagedInjections: newStagedInjectionStore(admin),
		// Marketplace wires both pools (TFAC-540): app for every
		// request-facing method (RLS-gated), admin for RecomputeStatsSystem —
		// the cross-team run-aggregation system job has no per-user identity
		// and its read spans every team's runs in the org, so it can't run
		// under any single caller's RLS view. See TFAC-535 / TFAC-540.
		Marketplace: newMarketplaceStore(app, admin),
		// Instances is admin-pool only: a fleet member isn't tenant data, so
		// there's no org to scope an app-pool policy on — same admin-only
		// shape as SystemLLMRuns/PendingFirings/EventQueue/ConversationQueue. Fleet
		// membership registry every TF process registers into at boot and
		// refreshes via periodic heartbeat.
		Instances: newInstanceStore(admin),
		// InstanceStats + Operators are admin-pool only, same posture as
		// Instances: fleet telemetry + the deployment-operator identity, never
		// read under a user's RLS context (TFAC-589).
		InstanceStats: newInstanceStatStore(admin),
		// SandboxStats is admin-pool only for the same reason InstanceStats
		// is: the per-sandbox resource series is executor telemetry the
		// sampler writes with no request identity in hand.
		SandboxStats: newSandboxStatStore(admin),
		Operators:    newOperatorStore(admin),
		// ConversationSignals is admin-pool only, same posture as Instances: the
		// cross-pod run-control outbox (TFAC-585), never read under a
		// user's RLS context.
		ConversationSignals: newConversationSignalStore(admin),
		// ConversationPendingInput on the top-level store is the admin-pool handle the
		// dispatcher's claim path uses for Consume (a goroutine with no request
		// context). Store is reached through the claims-tx handle
		// (TxStores.ConversationPendingInput, see tx.go) so the resume write commits
		// atomically with the status flip.
		ConversationPendingInput: newConversationPendingInputStore(admin),
		// Permissions is split-pool like Artifacts: the pending read runs on
		// the app pool under RLS (the policy composes through the
		// conversation, mirroring claims), every write on the admin pool —
		// tf_app holds no write grant and the writers are delegate goroutines
		// with no JWT-claims context.
		Permissions: newPermissionStore(app, admin),
		// OrgEventSources is split-pool: the request path reads and writes
		// through the app pool under RLS (member SELECT, admin write), and
		// the ...System read runs on the admin pool for the router and the
		// poller, neither of which carries request claims.
		OrgEventSources: newOrgEventSourceStore(app, admin),
		// ModelAvailability is app-pool only: member SELECT so the model list
		// can render the badge, org-admin write because a probe spends the
		// org's money. Nothing without request claims writes here — a
		// background prober would be the automatic re-probing this design
		// refuses — so there is no admin-pool arm to give it.
		ModelAvailability: newModelAvailabilityStore(app),
		// PollReadiness is admin-pool only: callers already hold an
		// authorized orgID (session claims or system context) by the time
		// they reach it, same admin-only shape as Instances. Org-scoped
		// readiness gate for /api/jira/stock + the one-shot "config took
		// effect" announce toast. See TFAC-583.
		PollReadiness: newPollReadinessStore(admin),
		// PlacementOverrides is admin-pool only, same posture as Instances:
		// the placement pin/replica overrides (TFAC-587) read for an already-
		// authorized, operator-gated orgID, never a browsable RLS surface.
		PlacementOverrides: newPlacementOverrideStore(admin),
		// ClaimCredentials is admin-pool only, same posture as Instances/
		// ConversationSignals: the sealed per-run credential bundle channel
		// (TFAC-614) never serves a request handler.
		ClaimCredentials: newClaimCredentialsStore(admin),
		// WorkspaceSnapshots is admin-pool only, same posture as
		// ClaimCredentials: the workspace-snapshot lifecycle record is written
		// by an executor's teardown and read by the resume path and the
		// retention reaper, never by a request handler.
		WorkspaceSnapshots: newWorkspaceSnapshotStore(admin),
		// Enterprise Edition SSO stores attach via Ext, built from the same
		// (app, admin) pool handles as core's stores.
		Ext: db.BuildStoreExtensions("postgres", app, admin),
		Tx:  s,
	}
	return s.stores
}

// The admin/app connection openers are NOT defined here — this
// package deliberately doesn't register the pgx stdlib driver as a
// side-effect import. internal/app/stores.go owns opening the admin and
// app *sql.DB handles (sql.Open("pgx", ...) against the admin/app DSNs)
// and passes them into New below. Tests construct *sql.DB via the
// pgtest harness, which registers the pgx driver itself.

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
//
// secretKey is the same app-layer AES-256-GCM key New takes; the
// SecretStore round-trip tests pass a real key so Put→Get works, and
// non-secret tests pass any key (unused). Tests share pgtest.SecretKey.
func NewForTx(tx *sql.Tx, secretKey aead.Key) db.TxStores {
	return db.TxStores{
		Scores:        newScoreStore(tx),
		Prompts:       newTxPromptStore(tx),
		Swipes:        newSwipeStore(tx, tx),
		Dashboard:     newDashboardStore(tx),
		Secrets:       newSecretStore(tx, tx, secretKey),
		EventHandlers: newTxEventHandlerStore(tx),
		Blueprints:    newBlueprintStore(tx, tx),
		Agents:        newTxAgentStore(tx),
		TeamAgents:    newTxTeamAgentStore(tx),
		Users:         newUsersStore(tx, tx),
		Tasks:         newTaskStore(tx, tx),
		Factory:       newFactoryReadStore(tx),
		TeamActivity:  newTeamActivityStore(tx),
		TeamPRs:       newTeamPRStore(tx, tx),
		// NewForTx is a test door — both pools collapse to the
		// supplied tx. Tests that exercise the admin-only branch
		// (event-triggered ConversationStore.Create, or any of the
		// `...System` methods that bypass RLS in
		// production) need the production WithTx wiring instead,
		// which gets the real admin pool via Store.admin.
		Conversations:         newConversationStore(tx, tx),
		Artifacts:             newArtifactStore(tx, tx),
		Entities:              newEntityStore(tx, tx),
		Repos:                 newRepositoryStore(tx, tx),
		PendingFirings:        newPendingFiringsStore(tx),
		Events:                newEventStore(tx, tx),
		TaskMemory:            newTaskMemoryStore(tx, tx),
		ConversationWorktrees: newConversationWorktreeStore(tx, tx),
		Orgs:                  newOrgsStore(tx, tx),
		OrgMemberships:        newOrgMembershipsStore(tx, tx),
		Teams:                 newTeamsStore(tx, tx),
		JiraStatusRules:       newJiraStatusRulesStore(tx, tx),
		TeamGitHubGroups:      newTeamGitHubGroupsStore(tx, tx),
		TeamGitHubRepos:       newTeamGitHubReposStore(tx, tx),
		// Both pools collapse to tx (test door). BackfillInstallationsFromAPI's
		// GetSystem would hit tf_app and be denied here — tests that exercise
		// it use New(admin, app, key) directly, same as the SecretStore tests.
		GitHubApps:               newGitHubAppsStore(tx, tx, newSecretStore(tx, tx, secretKey)),
		JiraApps:                 newJiraAppsStore(tx, tx),
		ShippedDefaults:          newTxShippedDefaultsStore(tx, newTxEventHandlerStore(tx)),
		Invites:                  newInvitesStore(tx, tx),
		SystemLLMRuns:            newSystemLLMRunStore(tx),
		AccessChangeLog:          newAccessChangeLogStore(tx, tx),
		ExternalActions:          newExternalActionStore(tx, tx),
		Spend:                    newSpendStore(tx, tx),
		AuthEvents:               newAuthEventStore(tx),
		Marketplace:              newMarketplaceStore(tx, tx),
		Instances:                newInstanceStore(tx),
		StagedInjections:         newStagedInjectionStore(tx),
		ConversationPendingInput: newConversationPendingInputStore(tx),
		OrgEventSources:          newOrgEventSourceStore(tx, tx),
		ModelAvailability:        newModelAvailabilityStore(tx),
		Ext:                      db.BuildStoreExtensions("postgres", tx, tx),
	}
}
