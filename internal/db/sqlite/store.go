// Package sqlite is the SQLite-backed implementation of the
// per-resource store interfaces declared in package db. Local-mode
// installs of triagefactory wire this implementation at startup
// (multi-mode wires internal/db/postgres). See the SKY-246 D2 spec
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
	// SKY-296 introduced two-pool constructors on EntityStore /
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
		EventHandlers:  newEventHandlerStore(conn, users),
		Chains:         newChainStore(conn, conn),
		Agents:         newAgentStore(conn, conn),
		TeamAgents:     newTeamAgentStore(conn),
		Users:          users,
		Tasks:          newTaskStore(conn, conn),
		Factory:        newFactoryReadStore(conn),
		AgentRuns:      newAgentRunStore(conn),
		Entities:       newEntityStore(conn, conn),
		Reviews:        newReviewStore(conn, conn),
		PendingPRs:     newPendingPRStore(conn, conn),
		Repos:          newRepoStore(conn, conn),
		PendingFirings: newPendingFiringsStore(conn),
		Projects:       newProjectStore(conn, conn),
		// Events wires both args to conn — SQLite has one connection
		// so the dual-pool constructor collapses, same as TaskStore.
		Events: newEventStore(conn, conn),
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
		// Curator: the goroutine wraps each turn in
		// Stores.Tx.SyntheticClaimsWithTx so the tx-bound variant
		// (composed inside the tx.go runTx body) is what handles
		// production writes. The non-tx variant wired here exists
		// for completeness — handler-side helpers stay on the
		// package-level *sql.DB calls until D9.
		Curator:    newCuratorStore(conn),
		GitHubApps: newGitHubAppsStore(conn, secrets),
		Tx:         s,
	}
	return s.stores
}
