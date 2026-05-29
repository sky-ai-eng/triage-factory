package sqlite

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// WithTx runs fn inside a single SQLite transaction. orgID must equal
// runmode.LocalDefaultOrg in local mode — anything else indicates a
// caller that thinks it's in multi mode and would silently misbehave
// against SQLite (no org column to enforce isolation). userID is
// accepted for signature parity with the Postgres impl but otherwise
// ignored; SQLite has no auth concept.
//
// The closure receives a TxStores whose every field is wired against
// the *sql.Tx, so any nested call writes through the same transaction.
// Commit on nil error, rollback on any error — the deferred Rollback
// is a no-op after Commit.
func (s *Store) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return s.runTx(ctx, orgID, userID, fn)
}

// SyntheticClaimsWithTx mirrors WithTx for callers that have an
// authoritative (orgID, userID) identity but no request context. In
// local mode the assertion is the same as WithTx — orgID must equal
// runmode.LocalDefaultOrg, userID is ignored, no JWT-claims setup is
// needed because SQLite has no auth concept. Signature parity with
// the Postgres impl is the only reason this exists on SQLite at all.
// See SKY-296.
func (s *Store) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return s.runTx(ctx, orgID, userID, fn)
}

// runTx is the shared body between WithTx and SyntheticClaimsWithTx.
// Both entry points have identical behavior in SQLite — the
// distinction is purely semantic (request vs synthetic identity)
// and only matters in the Postgres impl where the two paths set
// JWT claims differently.
func (s *Store) runTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	_ = userID // accepted for signature parity; SQLite has no auth concept
	if orgID != runmode.LocalDefaultOrg {
		return fmt.Errorf("sqlite WithTx: orgID must be %q in local mode, got %q", runmode.LocalDefaultOrg, orgID)
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	users := newUsersStore(tx, tx)
	txStores := db.TxStores{
		Scores:           newScoreStore(tx),
		Prompts:          newPromptStore(tx, tx),
		Swipes:           newSwipeStore(tx),
		Dashboard:        newDashboardStore(tx),
		Secrets:          newSecretStore(),
		EventHandlers:    newEventHandlerStore(tx, users),
		Chains:           newChainStore(tx, tx),
		Agents:           newAgentStore(tx, tx),
		TeamAgents:       newTeamAgentStore(tx),
		Users:            users,
		Tasks:            newTaskStore(tx, tx),
		Factory:          newFactoryReadStore(tx),
		AgentRuns:        newAgentRunStore(tx),
		Entities:         newEntityStore(tx, tx),
		Reviews:          newReviewStore(tx, tx),
		PendingPRs:       newPendingPRStore(tx, tx),
		Repos:            newRepoStore(tx, tx),
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
		Curator:          newCuratorStore(tx),
		GitHubApps:       newGitHubAppsStore(tx, newSecretStore()),
	}
	if err := fn(txStores); err != nil {
		return err
	}
	return tx.Commit()
}
