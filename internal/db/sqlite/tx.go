package sqlite

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WithTx runs fn inside a single SQLite transaction. orgID must equal
// runmode.LocalDefaultOrgID in local mode — anything else indicates a
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
// runmode.LocalDefaultOrgID, userID is ignored, no JWT-claims setup is
// needed because SQLite has no auth concept. Signature parity with
// the Postgres impl is the only reason this exists on SQLite at all.
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
	// The Postgres twin's span, same name and attribute, so a local-mode
	// trace has the same shape as a multi-mode one. org.id is the local
	// sentinel here — constant, but present, which is what keeps the two
	// shapes identical.
	ctx, span := tracer.Start(ctx, "db.tx.claims_bound",
		trace.WithAttributes(telemetry.OrgID(orgID)))
	defer span.End()

	if orgID != runmode.LocalDefaultOrgID {
		span.SetStatus(codes.Error, "org mismatch")
		return fmt.Errorf("sqlite WithTx: orgID must be %q in local mode, got %q", runmode.LocalDefaultOrgID, orgID)
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		span.SetStatus(codes.Error, "begin")
		return err
	}
	defer func() { _ = tx.Rollback() }()

	users := newUsersStore(tx, tx)
	txStores := db.TxStores{
		Scores:                   newScoreStore(tx),
		Prompts:                  newPromptStore(tx),
		Swipes:                   newSwipeStore(tx),
		Dashboard:                newDashboardStore(tx),
		Secrets:                  newSecretStore(),
		EventHandlers:            newEventHandlerStore(tx),
		Blueprints:               newBlueprintStore(tx),
		Agents:                   newAgentStore(tx, tx),
		TeamAgents:               newTeamAgentStore(tx),
		Users:                    users,
		Tasks:                    newTaskStore(tx, tx),
		Factory:                  newFactoryReadStore(tx),
		TeamActivity:             newTeamActivityStore(tx),
		TeamPRs:                  newTeamPRStore(tx),
		Conversations:            newConversationStore(tx),
		Artifacts:                newArtifactStore(tx),
		Entities:                 newEntityStore(tx, tx),
		Repos:                    newRepositoryStore(tx, tx),
		PendingFirings:           newPendingFiringsStore(tx),
		Events:                   newEventStore(tx, tx),
		TaskMemory:               newTaskMemoryStore(tx, tx),
		ConversationWorktrees:    newConversationWorktreeStore(tx, tx),
		Orgs:                     newOrgsStore(tx, tx),
		OrgMemberships:           newOrgMembershipsStore(),
		Teams:                    newTeamsStore(tx, tx),
		JiraStatusRules:          newJiraStatusRulesStore(tx, tx),
		TeamGitHubGroups:         newTeamGitHubGroupsStore(tx, tx),
		TeamGitHubRepos:          newTeamGitHubReposStore(tx, tx),
		GitHubApps:               newGitHubAppsStore(tx, newSecretStore()),
		JiraApps:                 newJiraAppsStore(tx),
		ShippedDefaults:          newTxShippedDefaultsStore(tx, newEventHandlerStore(tx)),
		Invites:                  newInvitesStore(tx, tx),
		SystemLLMRuns:            newSystemLLMRunStore(tx),
		AccessChangeLog:          newAccessChangeLogStore(tx),
		ExternalActions:          newExternalActionStore(tx),
		Spend:                    newSpendStore(tx),
		Marketplace:              newMarketplaceStore(tx, tx),
		ConversationPendingInput: newConversationPendingInputStore(tx),
		Permissions:              newPermissionStore(tx),
		OrgEventSources:          newOrgEventSourceStore(tx),
		ModelAvailability:        newModelAvailabilityStore(tx),
		Ext:                      db.BuildStoreExtensions("sqlite", tx, tx),
	}
	if err := fn(txStores); err != nil {
		// See the Postgres twin: rolled back via the defer, and not
		// recorded as an exception.
		span.SetStatus(codes.Error, "tx body")
		return err
	}
	if err := tx.Commit(); err != nil {
		span.SetStatus(codes.Error, "commit")
		return err
	}
	return nil
}
