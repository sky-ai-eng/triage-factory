package db

import (
	"context"
	"errors"
)

// Stores bundles every per-resource store interface plus the
// transaction runner. Constructed once at startup by either
// internal/db/sqlite.New (local mode) or internal/db/postgres.New
// (multi mode); fields are populated wave by wave.
//
// NEVER pass Stores to a handler. Handlers depend only on the
// specific interfaces they consume (db.ScoreStore, db.TaskStore, …).
// The bundle exists for main.go wiring and for the WithTx wrapper —
// nothing else. See docs/specs/sky-246-d2-store-abstraction.html §5.
type Stores struct {
	// Scores is the first store to land on the D2 wave 0 pilot.
	// Subsequent waves add the remaining 21 fields here.
	Scores ScoreStore

	// Prompts owns prompts + system_prompt_versions. SeedOrUpdate is
	// routed to the admin pool in Postgres (sidecar writes are
	// REVOKE'd from tf_app); every other method runs on the app pool.
	Prompts PromptStore

	// Swipes owns the swipe_events audit log + the task-status
	// transitions that follow each swipe.
	Swipes SwipeStore

	// Dashboard is a read-only projection over entities + their
	// snapshot_json blobs. Owns no table.
	Dashboard DashboardStore

	// Secrets is the per-org secret bag. Postgres impl wraps the
	// public.vault_* SECURITY DEFINER functions (RLS-gated per org);
	// SQLite impl delegates to internal/auth's keychain helpers so
	// callers see the same Put/Get/Delete shape in either mode.
	// orgID is required and enforced — vault wrapper rejects on a
	// claim/arg mismatch in multi, sqlite asserts LocalDefaultOrgID.
	Secrets SecretStore

	// EventHandlers owns the unified event_handlers table:
	// rules + triggers as one primitive with a kind discriminator.
	// Rules create unclaimed tasks (human triage); triggers also fire
	// an auto-delegation prompt. The router reads via GetEnabledForEvent
	// on every routed event; handlers do full CRUD per kind.
	EventHandlers EventHandlerStore

	// Blueprints owns blueprints + blueprint_steps + blueprint_runs. Read by
	// the blueprint HTTP handlers; written by the delegate spawner.
	Blueprints BlueprintStore

	// Agents owns the agents table — the org's workload identity.
	// One row per org. Bootstrap-only Create (admin pool in Postgres);
	// reads + admin-gated updates run on the app pool.
	Agents AgentStore

	// TeamAgents owns team_agents — per-team membership for the
	// agent + per-team config overrides. Bootstrap-only AddForTeam
	// (admin pool in Postgres); SetEnabled/SetOverrides/Remove run
	// on the app pool and gate on team membership via RLS.
	TeamAgents TeamAgentStore

	// Users owns the users table — non-secret identity facts like
	// display_name and the Jira binding — plus the host-scoped GitHub
	// identity bindings in user_github_identities (which
	// replaced the users.github_username column). The keychain
	// holds the PAT; the rows hold everything else. The GitHub login
	// backs the predicate-matcher allowlists, keyed on (user, host).
	Users UsersStore

	// Tasks owns the tasks table — lifecycle, claims, dedup,
	// swipe-triggered transitions, plus the run-history queries
	// powering the auto-delegate breaker. App pool in Postgres
	// (RLS-active) since the queue + per-task surface is request-
	// driven; the AI scorer reads tasks via the admin-pooled
	// ScoreStore.
	Tasks TaskStore

	// Factory is the read-only projection that backs the
	// /api/factory/snapshot handler. Every method is org-scoped via
	// an explicit orgID parameter (the handler reads from request
	// claims and forwards it). Admin-pool in Postgres for the
	// non-tx wiring on this bundle; production callers always run
	// inside WithTx and get the tx-bound, claims-set Factory via
	// TxStores instead.
	Factory FactoryReadStore

	// AgentRuns owns runs + run_messages — agent run lifecycle and
	// transcript. App pool in Postgres; every consumer is
	// request-equivalent or runs in a delegate goroutine launched from
	// a request handler.
	AgentRuns AgentRunStore

	// Artifacts owns the artifacts table — the durable, run-attributed,
	// polymorphic record of everything a run produces externally (branch,
	// PR, review, issue, comment). Deduped per (org_id, dedup_key) so all
	// of TFAC-454's capture writers UPSERT to one row. App pool in
	// Postgres (team-scoped RLS via team_id, like runs); consumers are the
	// exec choke point + reconciliation (writers) and run-detail / C2
	// (readers). See TFAC-455.
	Artifacts ArtifactStore

	// Entities owns the entities table — the long-lived source
	// objects (PR, Jira issue) every event/task/run hangs off. App
	// pool in Postgres; consumers are the tracker, projectclassify,
	// delegate context loaders, the scorer, and the server panels.
	Entities EntityStore

	// Repos owns repo_profiles — the user-configured GitHub repos
	// plus their cached AI profile and clone-attempt state. App pool
	// in Postgres; consumers are the repos handler, settings, the
	// curator, the projects handler, the poller manager, the
	// profiler, and the workspace CLI tests. Every method accepts
	// repoID as "owner/repo" — Postgres splits to (owner, repo) and
	// queries by the natural key UNIQUE(org_id, owner, repo).
	Repos RepoStore

	// PendingFirings owns the pending_firings table — the FIFO queue
	// of intent-to-auto-delegate rows the router enqueues when an
	// entity already has an active auto run. Admin pool in Postgres
	// (the router has no per-user identity; system service).
	PendingFirings PendingFiringsStore

	// Projects owns the projects table — user-curated work groupings
	// (Linear/Jira project mirrors with pinned repos and the curator
	// session that maintains the project's knowledge dir). App pool
	// in Postgres; consumers are the projects handler, curator,
	// backfill, project_entities, projectclassify runner, and the
	// projectbundle import/export paths.
	Projects ProjectStore

	// Events owns the events audit log — append-only event rows the
	// router records and the factory/delegate paths read. Holds both
	// pools: app for request-handler equivalents (stock
	// carry-over, factory drag-to-delegate) and admin for background
	// goroutines without JWT-claims context (router RecordSystem +
	// re-derive, delegate post-run metadata enrichment).
	Events EventStore

	// EventQueue owns the event_queue table — the durable router queue.
	// Enqueue writes the events audit row + a queue row atomically at
	// ingest (transactional outbox); the drain worker
	// claims pending rows, routes them, and marks them done. A
	// system-service store (admin pool in Postgres): the ingestor and
	// worker run as background goroutines with no per-user identity.
	EventQueue EventQueueStore

	// RunQueue owns the run queue — the work list the delegation dispatcher
	// drains to drive blueprints through their steps (sibling of EventQueue).
	// A blueprint step is enqueued as a runs row in status='queued'; a worker
	// claims it, runs the agent, and the reactor advances the blueprint_run.
	// A system-service store (admin pool in Postgres): the dispatcher runs as
	// a background worker with no per-user identity.
	RunQueue RunQueueStore

	// TaskMemory owns the run_memory table — per-run agent narrative
	// + human verdict, read back by the delegate spawner to
	// materialize prior context into fresh worktrees. Holds both
	// pools: app for request-handler equivalents (review/PR submit,
	// swipe-discard cleanup, factory/run-summary reads) and admin for
	// the spawner's runAgent goroutine (post-completion upsert + run-
	// start materializer, both without a JWT-claims context).
	TaskMemory TaskMemoryStore

	// RunWorktrees owns the run_worktrees table — one row per
	// (run_id, repo_id) lazy worktree reservation a Jira-style run
	// accumulates as the agent materializes repos via `workspace
	// add`. Holds both pools: app for the cmd/exec workspace CLI
	// (its synthetic-claims wrap is owned by a separate cmd/exec
	// auth pass) and admin for the spawner's runAgent + chain
	// orchestrator cleanup defers (no JWT-claims context).
	RunWorktrees RunWorktreeStore

	// Orgs owns the orgs table — the tenancy root. Background
	// services (poller, tracker, projectclassify, repoprofile)
	// iterate active orgs through this store at the top of each
	// cycle instead of hardcoding the runmode sentinel. Admin pool
	// in Postgres — every caller is a boot-launched goroutine
	// without JWT-claims context, and the iteration is by definition
	// a cross-org system-service read.
	Orgs OrgsStore

	// OrgMemberships owns the org_memberships table — the (user, org,
	// role) roster the org People surface (TFAC-417) lists and mutates.
	// Postgres holds both pools: the app pool for the RLS-gated roster read
	// (reads gate on org membership) and the role/remove writes (org admin,
	// or self-delete), with the tf.guard_org_owners trigger as the authority
	// on the last-owner invariant; the admin pool for the cross-member
	// GitHub/Jira identity enrichment, which the self-only identity-table RLS
	// can't express (scoped back to the org by a membership join). Multi-mode
	// only in practice; the SQLite impl is a stub satisfying the interface.
	OrgMemberships OrgMembershipsStore

	// Teams owns the teams table — the membership unit inside an
	// org. Request handlers synthesizing tasks / projects / prompts
	// resolve `team_id` for the requesting org via
	// GetDefaultForOrgSystem instead of hardcoding the local-mode
	// sentinel team. Admin pool in Postgres — see the TeamsStore
	// interface comment for the pool-split rationale.
	Teams TeamsStore

	// JiraStatusRules owns the jira_project_status_rules table —
	// one row per (team_id, project_key) carrying the team's per-
	// project pickup/in_progress/done configuration. App pool in
	// Postgres for the request-handler reads/writes (RLS gates by
	// team membership / team admin); admin pool for the boot-time
	// `...System` reads the poller manager + scorer make. Bulk-
	// replace semantics on ReplaceForTeam match config.Save() today.
	JiraStatusRules JiraStatusRulesStore

	// TeamGitHubGroups owns the team_github_groups table — the GitHub
	// twin of jira_project_status_rules, mapping fully-qualified GitHub
	// teams (org login + team slug) to TF teams for review-request
	// routing. App pool in Postgres for the request-handler reads/writes
	// (RLS gates by team membership / team admin); admin pool for the
	// `...System` routing + reconcile reads the router/poller make
	// without JWT claims. Replace-set semantics on SetForTeam.
	TeamGitHubGroups TeamGitHubGroupsStore

	// TeamGitHubRepos owns the team_github_repos table — the per-team
	// GitHub repo *tracking* selection and source of truth for which
	// repos a team cares about (the tracking-scope twin of
	// jira_project_status_rules, distinct from TeamGitHubGroups which is
	// review routing). App pool in Postgres for the request-handler
	// reads/writes (RLS gates by team membership / team admin); admin
	// pool for the `...System` router-gate reads + the repo_profiles
	// reconcile that ReplaceForTeam runs (repo_profiles is now the
	// org-wide UNION of every team's rows, a derived cache).
	TeamGitHubRepos TeamGitHubReposStore

	// Curator owns the curator-runtime tables (curator_requests,
	// curator_messages, curator_pending_context). App pool in
	// Postgres — the per-project goroutine wraps each turn's
	// writes in Tx.SyntheticClaimsWithTx with the requesting
	// user's identity (creator_user_id read from the request row
	// at dequeue). RLS policies gate every row on the
	// (org_id, creator_user_id) pair.
	Curator CuratorStore

	// GitHubApps owns the org_github_apps table — per-org GitHub
	// App registrations created through the manifest flow. App pool
	// in Postgres (RLS gates reads by org membership, writes by org
	// admin). SQLite is also wired in local mode and reads/writes
	// org_github_apps for the same manifest-flow path.
	GitHubApps GitHubAppsStore

	// JiraApps owns the org_jira_apps table — per-org Atlassian OAuth (3LO)
	// app registrations (the BYO-app override / local-supplied app). App pool
	// in Postgres (RLS gates reads by org membership, writes by org admin);
	// admin pool for the resolver's no-claims GetForOrgSystem. SQLite is wired
	// in local mode for the local-supplied BYO app.
	JiraApps JiraAppsStore

	// Invites owns the org_invites table — TF-owned, link-based org
	// invitations (TFAC-416). App pool for the admin-facing create/list/
	// revoke (RLS gates on org-admin); admin pool for the redeem reads
	// (GetByTokenHashSystem + IsOrgMemberSystem), whose actor is a
	// token-bearing outsider with no membership. Multi-mode only; the
	// SQLite impl is a stub returning ErrNotApplicableInLocal.
	Invites InvitesStore

	// OrgTemplate owns org_template_prompts + org_template_handlers — the
	// per-org, org-admin-editable template BootstrapNewOrg/NewTeam copy into
	// each new team's prompts + event_handlers. App pool for the
	// editor CRUD (org_template_*_all RLS gates on tf.user_is_org_admin);
	// admin pool for SeedFromShipped (org-create seed) + MaterializeIntoTeam
	// (per-team copy, which also writes the team's prompts/event_handlers/
	// system_prompt_versions). Multi-mode only; the SQLite impl exists so the
	// bootstrap tests can run without Postgres.
	OrgTemplate OrgTemplateStore

	// SystemLLMRuns owns the system_llm_runs table — per-call cost +
	// token accounting for the headless LLM jobs (scorer, repo-profiler,
	// project-classifier). Admin pool in Postgres: every writer is a
	// boot-launched background goroutine with no JWT-claims context
	// (system-written, org-scoped — same shape as PendingFirings). The
	// org-scoped RLS policy gates the app-pool reads the llm_spend view
	// makes (db.SpendStore, read by internal/server/usage_handler.go).
	// See TFAC-451.
	SystemLLMRuns SystemLLMRunStore

	// AccessChangeLog owns the access_change_log table — the small, low-volume
	// audit log of governance actions with no external entity (org/team
	// membership & role grants/changes/revokes, credential bind/rotate). App
	// pool in Postgres: every Record composes inside the claims-bearing WithTx
	// that runs the audited action, gated by an org-scoped RLS policy; the
	// org-admin audit view (internal/server/usage_access_log.go) reads
	// through the same pool. SQLite is N=1 and unscoped. See TFAC-471.
	AccessChangeLog AccessChangeLogStore

	// ExternalActions owns the external_actions table — the append-only audit
	// log of record: one row per external write TF performs under an ORG-scoped
	// credential (the org GitHub App / org Jira service account). Holds both
	// pools in Postgres: app for Record (the manual bot runs + server
	// approval/board handlers, under claims, org-scoped RLS) and ListByTeam,
	// admin for RecordSystem (event-triggered runs + the Jira mirror, no claims)
	// and ListByOrgSystem (the org-wide governance read). Append-only — both
	// Record paths ON CONFLICT DO NOTHING. SQLite is N=1 and unscoped. See
	// TFAC-483.
	ExternalActions ExternalActionStore

	// Spend is the read-only aggregation over the llm_spend view — the unified
	// shape that UNION-ALLs runs + curator_requests + system_llm_runs onto the
	// category axis (TFAC-472). App pool in Postgres: the view is
	// security_invoker, so the base tables' RLS scopes the read under the
	// querying user (a team member sees their team's runs, not another team's;
	// system/curator at org scope). Owns no table; the spine the dashboards +
	// safety cap (TFAC-449) read from.
	Spend SpendStore

	// AuthEvents owns the auth_events table — the SOC2 authentication audit log
	// of record: durable, append-only capture of every authentication / session
	// outcome (login, logout, refresh failure, JWT-verify failure, SSO
	// enforcement, break-glass). The authentication sibling of AccessChangeLog
	// (the authorization-CHANGE log). Admin-pool-only / system table in Postgres:
	// writes + reads never carry user claims and org_id is frequently NULL, so an
	// org-scoped RLS policy can't gate it — it is denied to the app roles like
	// public.user_identities, and the superuser pool does all I/O (same shape as
	// SystemLLMRuns). SQLite is N=1, parity-only (local mode has no login). See
	// TFAC-76.
	AuthEvents AuthEventStore

	// StagedInjections owns the staged_agent_injections table — the durable,
	// producer-agnostic "stage for next resume" agent-injection queue (TFAC-501,
	// the generic terminal half of TFAC-493's feedback seam). Admin-pool-only
	// in Postgres: both the producer (an eventbus subscriber) and the consumer
	// (a detached resume goroutine) run without JWT claims; the table inherits
	// runs' team-scoped RLS. SQLite is N=1. Written/read by the delegate
	// spawner's staged-injection API.
	StagedInjections StagedInjectionStore

	// Marketplace owns the within-org prompt marketplace tables
	// (marketplace_listings + _versions + _events, marketplace_votes,
	// marketplace_installs — TFAC-535). App pool only in Postgres: every
	// method is request-facing (browse/publish/vote/install as the acting
	// user), gated end to end by RLS. No "...System" variant.
	Marketplace MarketplaceStore

	// Instances owns the instances table — the fleet membership registry
	// every TF process registers into at boot and refreshes via periodic
	// heartbeat. Admin-pool-only in Postgres: no org_id (a fleet member
	// isn't tenant data), so there's no app-pool counterpart and no
	// "...System" suffix, same shape as RunQueueStore/EventQueueStore.
	// SQLite is N=1: one row, epoch bumping per restart.
	Instances InstanceStore

	// The SSO stores (sso_connections / sso_domains / sso_break_glass) live in
	// the Enterprise Edition (ee/sso/store) and attach via the Ext slot below —
	// core holds no SSO symbols.

	// Ext carries opaque store bundles built by registered
	// StoreExtension factories (see storeext.go) — the seam an
	// out-of-core extension (the Enterprise Edition) uses to attach its
	// own pool-split stores without core naming their types. Populated by
	// the backend builders; read through Extension. Exported so the
	// postgres/sqlite packages can set it in their struct literals; nil in
	// a community build with no extension registered.
	Ext map[string]any

	// Tx is the transaction runner — handlers that need atomic
	// multi-store writes call Tx.WithTx and receive a TxStores with
	// every field tx-bound. Postgres impl also sets the JWT claims
	// that RLS policies + tf.current_user_id() / tf.current_org_id()
	// read from.
	Tx TxRunner
}

// TxStores mirrors Stores but each field is bound to a single
// *sql.Tx so the closure body inside WithTx runs every operation
// in the same transaction. Fields are added as their parent stores
// land in successive waves.
type TxStores struct {
	Scores           ScoreStore
	Prompts          PromptStore
	Swipes           SwipeStore
	Dashboard        DashboardStore
	Secrets          SecretStore
	EventHandlers    EventHandlerStore
	Blueprints       BlueprintStore
	Agents           AgentStore
	TeamAgents       TeamAgentStore
	Users            UsersStore
	Tasks            TaskStore
	Factory          FactoryReadStore
	AgentRuns        AgentRunStore
	Artifacts        ArtifactStore
	Entities         EntityStore
	Repos            RepoStore
	PendingFirings   PendingFiringsStore
	Projects         ProjectStore
	Events           EventStore
	TaskMemory       TaskMemoryStore
	RunWorktrees     RunWorktreeStore
	Orgs             OrgsStore
	OrgMemberships   OrgMembershipsStore
	Teams            TeamsStore
	JiraStatusRules  JiraStatusRulesStore
	TeamGitHubGroups TeamGitHubGroupsStore
	TeamGitHubRepos  TeamGitHubReposStore
	Curator          CuratorStore
	GitHubApps       GitHubAppsStore
	JiraApps         JiraAppsStore
	OrgTemplate      OrgTemplateStore
	Invites          InvitesStore
	SystemLLMRuns    SystemLLMRunStore
	AccessChangeLog  AccessChangeLogStore
	ExternalActions  ExternalActionStore
	Spend            SpendStore
	AuthEvents       AuthEventStore
	StagedInjections StagedInjectionStore
	Marketplace      MarketplaceStore
	Instances        InstanceStore

	// Ext carries opaque store bundles built by registered
	// StoreExtension factories (see storeext.go), tx-bound to the same
	// *sql.Tx as every other field here. Read through Extension; nil in a
	// community build with no extension registered.
	Ext map[string]any
}

// Extension returns the opaque store bundle registered under key (see
// storeext.go), or nil when no extension registered it. The registering
// package wraps this in a typed accessor that performs the assertion.
func (s Stores) Extension(key string) any { return s.Ext[key] }

// Extension returns the tx-bound opaque store bundle registered under
// key, or nil when no extension registered it.
func (t TxStores) Extension(key string) any { return t.Ext[key] }

// TxRunner runs fn inside a single database transaction. Postgres
// impl additionally calls
//
//	SELECT set_config('request.jwt.claims', $1, true)
//
// before fn so RLS policies see the right (orgID, userID) claims for
// this transaction. set_config(..., true) scopes to the tx and does
// not leak to other pool connections. SQLite impl ignores orgID /
// userID beyond asserting orgID == runmode.LocalDefaultOrgID.
//
// Callers always pass orgID + userID explicitly — D7 will replace
// the explicit pass with extraction from a request-scoped context,
// but the WithTx shape stays the same.
//
// SyntheticClaimsWithTx mirrors WithTx for callers that have an
// authoritative (orgID, userID) identity but no request context —
// delegate-spawner goroutines, curator-message processing, post-
// terminal handler cleanup, agent CLI subcommands. The only
// structural difference from WithTx is the source of the claims
// values: request context vs caller-supplied. The Postgres impl
// shares its body with WithTx via a private helper; the SQLite
// impl asserts orgID == runmode.LocalDefaultOrgID and ignores
// userID (no auth concept in local mode).
//
// userID is required and must reference a real users row in
// Postgres — runs.creator_user_id has an FK to users(id). Callers
// that don't have a real user (event-triggered run completion,
// system services) should route to admin pool via the per-store
// `...System` methods instead. Passing runmode.LocalDefaultUserID
// in production multi-mode is rejected with a clear error because
// that sentinel has no FK target in the multi-mode users table.
type TxRunner interface {
	WithTx(ctx context.Context, orgID, userID string, fn func(TxStores) error) error
	SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(TxStores) error) error
}

// ErrNotApplicableInLocal is returned by SQLite impls of multi-only
// store methods (SessionStore.Insert, MembershipStore.Add, …). The
// auth path is gated behind runmode.ModeMulti, so this should never
// reach a production user; the error is the safety net for code that
// escapes that gate.
var ErrNotApplicableInLocal = errors.New("db: operation not applicable in local mode")
