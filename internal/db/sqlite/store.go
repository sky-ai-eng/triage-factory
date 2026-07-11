// Package sqlite is the SQLite-backed implementation of the
// per-resource store interfaces declared in package db. Local-mode
// installs of triagefactory wire this implementation at startup
// (multi-mode wires internal/db/postgres). See the D2 spec
// at docs/specs/sky-246-d2-store-abstraction.html for the full
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
	// RepoStore / UsersStore / AgentStore so the Postgres impl can
	// route `...System` admin-pool variants distinctly. SQLite has
	// one connection — both args collapse to conn here.
	users := newUsersStore(conn, conn)
	secrets := newSecretStore()
	s.stores = db.Stores{
		Scores:         newScoreStore(conn),
		Prompts:        newPromptStore(conn, conn),
		Swipes:         newSwipeStore(conn),
		Dashboard:      newDashboardStore(conn),
		Secrets:        secrets,
		EventHandlers:  newEventHandlerStore(conn),
		Blueprints:     newBlueprintStore(conn, conn),
		Agents:         newAgentStore(conn, conn),
		TeamAgents:     newTeamAgentStore(conn),
		Users:          users,
		Tasks:          newTaskStore(conn, conn),
		Factory:        newFactoryReadStore(conn),
		AgentRuns:      newAgentRunStore(conn),
		Artifacts:      newArtifactStore(conn),
		Entities:       newEntityStore(conn, conn),
		Repos:          newRepoStore(conn, conn),
		PendingFirings: newPendingFiringsStore(conn),
		Projects:       newProjectStore(conn, conn),
		// Events wires both args to conn — SQLite has one connection
		// so the dual-pool constructor collapses, same as TaskStore.
		Events: newEventStore(conn, conn),
		// EventQueue holds the connection directly (it self-manages the
		// Enqueue transaction); single-worker drain in local mode.
		EventQueue: newEventQueueStore(conn),
		// RunQueue holds the connection directly; single-worker dispatcher
		// in local mode (no claim contention, so a plain claim suffices).
		RunQueue: newRunQueueStore(conn),
		// TaskMemory wires both args to conn — SQLite has one
		// connection so the dual-pool constructor collapses; the
		// `...System` variants forward to the non-System bodies.
		TaskMemory: newTaskMemoryStore(conn, conn),
		// RunWorktrees wires both args to conn — SQLite has one
		// connection so the dual-pool constructor collapses; the
		// `...System` variants forward to the non-System bodies.
		RunWorktrees: newRunWorktreeStore(conn, conn),
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
		// the one connection. ReplaceForTeam's repo_profiles reconcile
		// runs in the same tx as the team-row write here.
		TeamGitHubRepos: newTeamGitHubReposStore(conn, conn),
		// Curator: the goroutine wraps each turn in
		// Stores.Tx.SyntheticClaimsWithTx so the tx-bound variant
		// (composed inside the tx.go runTx body) is what handles
		// production writes. The non-tx variant wired here exists
		// for completeness — handler-side helpers stay on the
		// package-level *sql.DB calls until D9.
		Curator:    newCuratorStore(conn),
		GitHubApps: newGitHubAppsStore(conn, secrets),
		JiraApps:   newJiraAppsStore(conn),
		// OrgTemplate is a multi-mode concept; SQLite wires it so the
		// db-package bootstrap tests can run without Postgres. Local mode
		// never seeds or reads it.
		OrgTemplate: newOrgTemplateStore(conn),
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
		// RunSignals is Postgres-only (TFAC-585): this is a stub returning
		// ErrNotApplicableInLocal from every method — local mode is always
		// its own run's owner, so no code path may reach it.
		RunSignals: newRunSignalStore(),
		// RunPendingInput is dual-dialect (unlike RunSignals): local mode's
		// dispatcher claims its own resumed runs through the identical
		// queue path.
		RunPendingInput: newRunPendingInputStore(conn),
		// PollReadiness is admin-pool only in Postgres; SQLite collapses to
		// the one connection (N=1, no RLS). Org-scoped readiness gate for
		// /api/jira/stock + the one-shot "config took effect" announce
		// toast — see the poll_readiness migration. See TFAC-583.
		PollReadiness: newPollReadinessStore(conn),
		// Enterprise Edition SSO stubs attach via Ext (multi-mode stores live
		// in ee/sso/store; the sqlite stubs there return ErrNotApplicableInLocal).
		Ext: db.BuildStoreExtensions("sqlite", conn, conn),
		Tx:  s,
	}
	return s.stores
}
