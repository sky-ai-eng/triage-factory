// Package sqlite is the SQLite-backed implementation of the
// per-resource store interfaces declared in package db. Local-mode
// installs of triagefactory wire this implementation at startup
// (multi-mode wires internal/db/postgres). See the D2 spec
// at docs/for-agents/specs/sky-246-d2-store-abstraction.html for the full
// design.
package sqlite

import (
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Store holds the SQLite connection + the bundle of resource-store
// implementations wired against it. New returns the assembled
// db.Stores bundle for application startup wiring; handlers should
// depend only on the specific store interfaces they need.
type Store struct {
	conn *sql.DB

	stores db.Stores
}

// New wires a db.Stores bundle backed by SQLite. Wave 0 ships only
// ScoreStore + the TxRunner; subsequent waves populate the remaining
// 21 fields on the bundle.
func New(conn *sql.DB) db.Stores {
	s := &Store{conn: conn}
	// Two-pool constructors exist on EntityStore /
	// RepositoryStore / UsersStore / AgentStore so the Postgres impl can
	// route `...System` admin-pool variants distinctly. SQLite has
	// one connection — both args collapse to conn here.
	users := newUsersStore(conn, conn)
	secrets := newSecretStore()
	// Built once and shared with ShippedDefaults below: SeedShippedIntoTeam's
	// phase 3 (handlers) delegates to EventHandlers.Seed rather than
	// duplicating its SQL.
	eventHandlers := newEventHandlerStore(conn)
	s.stores = db.Stores{
		Scores:         newScoreStore(conn),
		Prompts:        newPromptStore(conn),
		Swipes:         newSwipeStore(conn),
		Dashboard:      newDashboardStore(conn),
		Secrets:        secrets,
		EventHandlers:  eventHandlers,
		Blueprints:     newBlueprintStore(conn),
		Agents:         newAgentStore(conn, conn),
		TeamAgents:     newTeamAgentStore(conn),
		Users:          users,
		Tasks:          newTaskStore(conn, conn),
		Factory:        newFactoryReadStore(conn),
		TeamActivity:   newTeamActivityStore(conn),
		TeamPRs:        newTeamPRStore(conn),
		Conversations:  newConversationStore(conn),
		Artifacts:      newArtifactStore(conn),
		Entities:       newEntityStore(conn, conn),
		Repos:          newRepositoryStore(conn, conn),
		PendingFirings: newPendingFiringsStore(conn),
		// Events wires both args to conn — SQLite has one connection
		// so the dual-pool constructor collapses, same as TaskStore.
		Events: newEventStore(conn, conn),
		// EventQueue holds the connection directly (it self-manages the
		// Enqueue transaction); single-worker drain in local mode.
		EventQueue: newEventQueueStore(conn),
		// ConversationQueue holds the connection directly; single-worker dispatcher
		// in local mode (no claim contention, so a plain claim suffices).
		ConversationQueue: newConversationQueueStore(conn),
		// TaskMemory wires both args to conn — SQLite has one
		// connection so the dual-pool constructor collapses; the
		// `...System` variants forward to the non-System bodies.
		TaskMemory: newTaskMemoryStore(conn, conn),
		// ConversationWorktrees wires both args to conn — SQLite has one
		// connection so the dual-pool constructor collapses; the
		// `...System` variants forward to the non-System bodies.
		ConversationWorktrees: newConversationWorktreeStore(conn, conn),
		// Orgs is dual-pool in Postgres; SQLite collapses to the one
		// connection. Callers are background services iterating the
		// active org set, settings reads/writes from request handlers,
		// and `...System` settings reads from boot-time goroutines.
		Orgs: newOrgsStore(conn, conn),
		// OrgMemberships is a hosted-only surface; the SQLite impl is a
		// stub satisfying the interface (its methods return
		// ErrNotApplicableInLocal — the /org handlers 404 in local mode).
		OrgMemberships: newOrgMembershipsStore(),
		// Teams is dual-pool in Postgres; SQLite collapses to the one
		// connection. Covers default-team lookup for request handlers
		// + per-team settings reads/writes.
		Teams: newTeamsStore(conn, conn),
		// JiraStatusRules is dual-pool in Postgres; SQLite collapses
		// to the one connection. Bulk-replace semantics match the
		// existing config.Save() flow.
		JiraStatusRules: newJiraStatusRulesStore(conn, conn),
		// TeamGitHubGroups is dual-pool in Postgres; SQLite collapses
		// to the one connection. Replace-set semantics on SetForTeam;
		// the `...System` variants forward to the non-System bodies.
		TeamGitHubGroups: newTeamGitHubGroupsStore(conn, conn),
		// TeamGitHubRepos is dual-pool in Postgres; SQLite collapses to
		// the one connection. ReplaceForTeam's repositories reconcile
		// runs in the same tx as the team-row write here.
		TeamGitHubRepos: newTeamGitHubReposStore(conn, conn),
		GitHubApps:      newGitHubAppsStore(conn, secrets),
		// ReachableRepos: the reachable-repo cache. Admin-pool-only in
		// Postgres; one connection here. Wired in local mode too — the refresh
		// that maintains it is by-pull in both modes, which is the only shape
		// that works where no webhook ever arrives.
		ReachableRepos: newReachableReposStore(conn),
		// GitHubDeliveries is admin-pool-only in Postgres; SQLite collapses to
		// the one connection. Wired in local mode too — a local install GitHub
		// can reach runs the same pre-auth receiver.
		GitHubDeliveries: newGitHubDeliveryStore(conn),
		JiraApps:         newJiraAppsStore(conn),
		// ShippedDefaults is what BootstrapNewOrg/BootstrapNewTeam call.
		// Phase 3 (handlers) reuses the eventHandlers store built above
		// instead of duplicating its Seed SQL.
		ShippedDefaults: newShippedDefaultsStore(conn, eventHandlers),
		// Invites is multi-mode only; the SQLite stub returns
		// ErrNotApplicableInLocal. Wired so the bundle is complete in both
		// modes — local never mounts the invite routes.
		Invites: newInvitesStore(conn, conn),
		// SystemLLMRuns is admin-pool only in Postgres; SQLite collapses to
		// the one connection (N=1, no RLS).
		SystemLLMRuns: newSystemLLMRunStore(conn),
		// AccessChangeLog is app-pool in Postgres; SQLite collapses to the one
		// connection (N=1, no RLS). See TFAC-471.
		AccessChangeLog: newAccessChangeLogStore(conn),
		// ExternalActions holds both pools in Postgres; SQLite collapses to the
		// one connection (N=1, no RLS). Append-only audit log of org-credential
		// external writes. See TFAC-483.
		ExternalActions: newExternalActionStore(conn),
		// Spend is app-pool (RLS-scoped) in Postgres; SQLite collapses to the one
		// connection (N=1, no RLS). Read-only view over llm_spend. See TFAC-472.
		Spend: newSpendStore(conn),
		// AuthEvents is admin-pool only in Postgres; SQLite collapses to the one
		// connection (N=1, no RLS). SOC2 authentication audit log; parity-only in
		// local mode (no login flow). See TFAC-76.
		AuthEvents: newAuthEventStore(conn),
		// StagedInjections is admin-pool only in Postgres; SQLite collapses to the one
		// connection (N=1, no RLS). Durable "stage for next resume"
		// agent-injection queue. See TFAC-501.
		StagedInjections: newStagedInjectionStore(conn),
		// Marketplace is multi-mode only (TFAC-535): every marketplace_* table
		// lives in the Postgres baseline only. This is a stub returning
		// ErrNotApplicableInLocal from every method — wired so the bundle is
		// complete in both modes; local never mounts the marketplace routes.
		Marketplace: newMarketplaceStore(conn, conn),
		// Instances is admin-pool only in Postgres; SQLite collapses to the
		// one connection (N=1, no RLS). Fleet membership registry — one
		// row, epoch bumping per restart.
		Instances: newInstanceStore(conn),
		// InstanceStats + Operators collapse to the one connection (N=1). The
		// sampler still writes stats; operators is effectively unused (the
		// single local user is implicitly the operator). See TFAC-589.
		InstanceStats: newInstanceStatStore(conn),
		// SandboxStats records nothing in practice here: local mode never
		// sandboxes, so no jail exists to sample. Wired so the bundle is
		// complete and the dual-dialect contract holds in one conformance
		// suite, not because a local run has a cgroup.
		SandboxStats: newSandboxStatStore(conn),
		Operators:    newOperatorStore(conn),
		// ConversationSignals is Postgres-only (TFAC-585): this is a stub returning
		// ErrNotApplicableInLocal from every method — local mode is always
		// its own run's owner, so no code path may reach it.
		ConversationSignals: newConversationSignalStore(),
		// ConversationPendingInput is dual-dialect (unlike ConversationSignals): local mode's
		// dispatcher claims its own resumed runs through the identical
		// queue path.
		ConversationPendingInput: newConversationPendingInputStore(conn),
		// Permissions is split-pool in Postgres; SQLite collapses to the one
		// connection (N=1, no RLS). This is the arm production uses — the
		// browser permission round-trip is reached only by an unsandboxed
		// local SDK run.
		Permissions: newPermissionStore(conn),
		// OrgEventSources is split-pool in Postgres; SQLite collapses to the
		// one connection (N=1, no RLS) and asserts the local org instead.
		OrgEventSources: newOrgEventSourceStore(conn),
		// ModelAvailability is app-pool-only in Postgres; SQLite collapses to
		// the one connection (N=1, no RLS) and asserts the local org instead.
		// Never written in local mode — nothing probes there — but present so
		// the contract is stated once, in the conformance suite.
		ModelAvailability: newModelAvailabilityStore(conn),
		// PollReadiness is admin-pool only in Postgres; SQLite collapses to
		// the one connection (N=1, no RLS). Org-scoped readiness gate for
		// /api/jira/stock + the one-shot "config took effect" announce
		// toast — see the poll_readiness migration. See TFAC-583.
		PollReadiness: newPollReadinessStore(conn),
		// PlacementOverrides collapses to the one connection (N=1). Inert in
		// local mode — the placement hash always returns self — but present
		// for store-interface + conformance symmetry. See TFAC-587.
		PlacementOverrides: newPlacementOverrideStore(conn),
		// ClaimCredentials is admin-pool only in Postgres; SQLite collapses
		// to the one connection. Never populated in local mode (forced
		// role=all, the bundle path is executor-role-only) — exists for
		// store-interface + conformance-test symmetry. See TFAC-614.
		ClaimCredentials: newClaimCredentialsStore(conn),
		// WorkspaceSnapshots collapses to the one connection (N=1, no RLS).
		// Live in local mode, unlike ClaimCredentials: the single process parks
		// and snapshots through the same writer, so the lifecycle row is
		// recorded here exactly as it is on an executor.
		WorkspaceSnapshots: newWorkspaceSnapshotStore(conn),
		// Enterprise Edition SSO stubs attach via Ext (multi-mode stores live
		// in ee/sso/store; the sqlite stubs there return ErrNotApplicableInLocal).
		Ext: db.BuildStoreExtensions("sqlite", conn, conn),
		Tx:  s,
	}
	return s.stores
}
