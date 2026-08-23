package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WithTx runs fn inside a single Postgres transaction against the app
// pool (RLS-active). Before fn runs, it calls
//
//	SELECT set_config('request.jwt.claims', $1, true)
//
// so RLS policies + tf.current_user_id() / tf.current_org_id() helpers
// see the right (orgID, userID) for this tx. set_config(..., true)
// scopes the setting to the tx — it doesn't leak to other connections
// in the pool.
//
// Callers always pass orgID + userID explicitly. D7 will replace the
// explicit pass with extraction from a request-scoped context (e.g.
// httpx.ClaimsFrom(ctx)), but the WithTx shape stays the same.
//
// Closures that need to bypass RLS (system services) shouldn't use
// WithTx at all — they should call store methods directly on the
// admin-wired stores (db.Stores.Scores in wave 0; more in later
// waves). WithTx is purely for the request-handler atomicity boundary.
func (s *Store) WithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	return s.runClaimsBoundTx(ctx, orgID, userID, fn)
}

// SyntheticClaimsWithTx mirrors WithTx for callers that have an
// authoritative (orgID, userID) identity but no request context —
// delegate spawner goroutines, post-terminal handler cleanup, agent CLI
// subcommands.
//
// The Postgres body is identical to WithTx — same role elevation,
// same JWT-claims setup, same TxStores wiring. The only difference
// is the *intent* of the call site: WithTx callers extract the pair
// from request context, SyntheticClaimsWithTx callers construct it
// from a known row identity (the run's creator_user_id, etc.). Both
// run under tf_app, both honor RLS.
//
// userID must be a real users row id. Passing
// runmode.LocalDefaultUserID is rejected — that sentinel has no FK
// target in the multi-mode users table, and conversations.creator_user_id
// has an FK to users(id). Callers that lack a real user identity
// (event-triggered runs by schema CHECK, system services) should
// route through the admin pool via per-store `...System` methods
// instead.
func (s *Store) SyntheticClaimsWithTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	if userID == runmode.LocalDefaultUserID {
		return errors.New("postgres: SyntheticClaimsWithTx rejected runmode.LocalDefaultUserID — sentinel has no FK target in multi-mode users; route to admin pool via per-store ...System methods")
	}
	if userID == "" {
		return errors.New("postgres: SyntheticClaimsWithTx requires a non-empty userID; route through admin pool for callers that have no user identity")
	}
	return s.runClaimsBoundTx(ctx, orgID, userID, fn)
}

// runClaimsBoundTx is the shared body between WithTx and
// SyntheticClaimsWithTx. The only structural difference between the
// two public entry points is the source of the (orgID, userID) pair
// (request context vs caller-supplied) plus the SyntheticClaimsWithTx
// guardrails enforced at the public layer — once we're past those,
// the SQL is identical.
func (s *Store) runClaimsBoundTx(ctx context.Context, orgID, userID string, fn func(db.TxStores) error) error {
	// One span per claims-bound transaction — the handler-side atomicity
	// boundary every RLS-scoped write passes through. otelsql covers each
	// statement, but the gap between them (holding the connection, the
	// closure's own work, the commit) is invisible without this. userID
	// stays off: org.id already scopes the trace, and a per-user span
	// attaches an identity to every query a person runs.
	ctx, span := tracer.Start(ctx, "db.tx.claims_bound",
		trace.WithAttributes(telemetry.OrgID(orgID)))
	defer span.End()

	tx, err := s.app.BeginTx(ctx, nil)
	if err != nil {
		span.SetStatus(codes.Error, "begin")
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Elevate the role before doing anything else. The app pool
	// connects as `authenticator` (LOGIN, NOINHERIT) which has no
	// privileges by design — RLS policies expect `tf_app` to be
	// the active role, and the pgtest harness's WithUser helper
	// does the same elevation. Without this, every WithTx-bound
	// store call would fail at the role layer (not even RLS — just
	// "permission denied" because authenticator has no grants).
	// SET LOCAL scopes the role change to the tx, so the pool
	// connection returns to authenticator when the tx ends.
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE tf_app`); err != nil {
		span.SetStatus(codes.Error, "set role")
		return err
	}

	claims, err := json.Marshal(map[string]any{
		"sub":    userID,
		"org_id": orgID,
	})
	if err != nil {
		span.SetStatus(codes.Error, "marshal claims")
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, string(claims)); err != nil {
		span.SetStatus(codes.Error, "set claims")
		return err
	}

	if err := fn(s.txStoresFromTx(tx)); err != nil {
		// Rolled back via the defer. Not recorded as an exception: a
		// handler refusing a write with a validation error is normal, and
		// the error text can carry tenant data.
		span.SetStatus(codes.Error, "tx body")
		return err
	}
	if err := tx.Commit(); err != nil {
		span.SetStatus(codes.Error, "commit")
		return err
	}
	return nil
}

// txStoresFromTx returns the TxStores bundle wired against a single
// *sql.Tx. Shared between the WithTx and SyntheticClaimsWithTx code
// paths so wiring drift is impossible: both entrypoints get the
// exact same set of tx-bound stores, with the same admin-pool
// retention for Conversations (event-triggered Create routes around RLS
// even from inside a claims-set tx — see the Create comment).
func (s *Store) txStoresFromTx(tx *sql.Tx) db.TxStores {
	return db.TxStores{
		Scores:  newScoreStore(tx),
		Prompts: newTxPromptStore(tx),
		// Swipes keeps the real admin pool for its admin half even inside a
		// claims-set tx, same as Conversations below: UndoLastSwipe's guard
		// has to see swipe_events rows RLS hides from the requesting user.
		Swipes:    newSwipeStore(tx, s.admin),
		Dashboard: newDashboardStore(tx),
		// Secrets: app half is the claims-set tx (Put/Get/Delete +
		// per-user trio → org_secrets under tf_app + RLS); admin half
		// stays the real admin pool so GetSystem / GetUserSystem /
		// PutUserSystem route around RLS. secretKey carries the same
		// app-layer encryption key New was built with.
		Secrets:       s.buildSecrets(tx, s.admin),
		EventHandlers: newTxEventHandlerStore(tx),
		// Blueprints: composed half is tx; admin half stays the real
		// admin pool so event-triggered CreateRun + the `...System`
		// reads route around RLS. The admin writes commit
		// autonomously from the outer tx — same pool-routing
		// semantics as ConversationStore.Create.
		Blueprints:   newBlueprintStore(tx, s.admin),
		Agents:       newTxAgentStore(tx),
		TeamAgents:   newTxTeamAgentStore(tx),
		Users:        newUsersStore(tx, tx),
		Tasks:        newTaskStore(tx, s.admin),
		Factory:      newFactoryReadStore(tx),
		TeamActivity: newTeamActivityStore(tx),
		// Conversations: composed half is tx; admin half stays the
		// real admin pool so event-triggered Create can route
		// around RLS. The admin write commits autonomously from
		// the outer tx — see Create's pool-routing comment for
		// why that's the intended semantics.
		Conversations: newConversationStore(tx, s.admin),
		// Artifacts: app-side write routes through the tx so writes
		// compose with the surrounding claims tx (artifacts_* RLS scopes
		// by team_id like runs); admin half stays pinned to s.admin so
		// UpsertSystem inside WithTx routes outside the tx and commits
		// autonomously, the same shape Conversations / ConversationWorktrees use.
		Artifacts:      newArtifactStore(tx, s.admin),
		Entities:       newEntityStore(tx, tx),
		Repos:          newRepositoryStore(tx, tx),
		PendingFirings: newPendingFiringsStore(tx),
		// Events: app-side write routes through the tx; admin half
		// stays pinned to the real admin pool so RecordSystem /
		// GetMetadataSystem inside WithTx routes outside the tx —
		// those writes commit autonomously, same shape Conversations /
		// TaskMemory use for their admin-pool halves.
		Events: newEventStore(tx, s.admin),
		// TaskMemory: app-side write routes through the tx; admin
		// half stays pinned to the real admin pool so
		// UpsertAgentMemorySystem / GetMemoriesForEntitySystem inside
		// WithTx routes outside the tx — those writes commit
		// autonomously, same shape Events / Conversations use for their
		// admin-pool halves.
		TaskMemory: newTaskMemoryStore(tx, s.admin),
		// ConversationWorktrees: app-side write routes through the tx; admin
		// half stays pinned to s.admin so DeleteByPathSystem +
		// ListSystem inside WithTx route outside the tx — those
		// writes commit autonomously, same shape Events /
		// Conversations / TaskMemory use for their admin-pool halves.
		ConversationWorktrees: newConversationWorktreeStore(tx, s.admin),
		// Orgs: app-side writes route through the tx so settings
		// upserts compose with the surrounding claims tx; admin half
		// stays pinned to s.admin so ListActiveSystem +
		// GetSettingsSystem inside WithTx route outside the tx (those
		// reads are by-design cross-tenant / claims-less).
		Orgs: newOrgsStore(tx, s.admin),
		// OrgMemberships: app half is the claims-set tx so the RLS-gated
		// roster read + role/remove mutations compose with the surrounding
		// transaction; admin half stays pinned to s.admin so the cross-member
		// identity enrichment routes outside the tx (it bypasses the
		// self-only identity RLS, scoped to the org by a membership join).
		OrgMemberships: newOrgMembershipsStore(tx, s.admin),
		// Teams: app-side writes route through the tx so per-team
		// settings upserts compose with the surrounding claims tx;
		// admin half stays pinned to s.admin so GetDefaultForOrgSystem
		// + GetSettingsSystem inside WithTx route outside the tx — same
		// shape Orgs/Events/Conversations use for their admin-pool halves.
		Teams: newTeamsStore(tx, s.admin),
		// JiraStatusRules: app-side write routes through the tx so
		// ReplaceForTeam composes with the surrounding claims tx;
		// admin half stays pinned to s.admin so ListForTeamSystem
		// inside WithTx routes outside the tx.
		JiraStatusRules: newJiraStatusRulesStore(tx, s.admin),
		// TeamGitHubGroups: app-side writes (SetForTeam) route through
		// the tx so they compose with the surrounding claims tx; admin
		// half stays pinned to s.admin so the `...System` routing +
		// reconcile reads inside WithTx route outside the tx.
		TeamGitHubGroups: newTeamGitHubGroupsStore(tx, s.admin),
		// TeamGitHubRepos: app-side write (the team-row replace inside
		// ReplaceForTeam) routes through the tx so it composes with the
		// surrounding claims tx; admin half stays pinned to s.admin so
		// the org-wide union read + repositories reconcile (which must
		// see sibling teams' rows past RLS) route outside the tx, the
		// same autonomous-commit shape Events / TaskMemory use.
		TeamGitHubRepos: newTeamGitHubReposStore(tx, s.admin),
		// app half is the claims-set tx (GetForOrg / CreateForOrg);
		// admin half stays the real admin pool so installation writes +
		// GetForOrgSystem / backfill route outside the tx.
		GitHubApps: newGitHubAppsStore(tx, s.admin, s.buildSecrets(tx, s.admin)),
		// JiraApps: app half is the claims-set tx (GetForOrg / UpsertForOrg /
		// DeleteForOrg); admin half stays the real admin pool so the resolver's
		// GetForOrgSystem routes outside the tx.
		JiraApps: newJiraAppsStore(tx, s.admin),
		// ShippedDefaults: tx-bound so a misuse from inside WithTx fails
		// loudly (SeedShippedIntoTeam refuses to run there — admin-pool
		// bootstrap work). The bootstrap path reaches it through the non-tx
		// stores.ShippedDefaults instead.
		ShippedDefaults: newTxShippedDefaultsStore(tx, newTxEventHandlerStore(tx)),
		// Invites: app-side writes (Create/Revoke) route through the tx so
		// they compose with the surrounding claims tx; admin half stays
		// pinned to s.admin so the redeem reads (GetByTokenHashSystem +
		// IsOrgMemberSystem) inside WithTx route outside the tx — those are
		// by-design claims-less (the redeem actor has no membership).
		Invites: newInvitesStore(tx, s.admin),
		// SystemLLMRuns stays pinned to s.admin (system-written, no
		// JWT-claims writer) so an INSERT inside WithTx routes outside the
		// tx and commits autonomously — the same admin-pool shape Events /
		// TaskMemory use for their write-only halves.
		SystemLLMRuns: newSystemLLMRunStore(s.admin),
		// AccessChangeLog: app half is the tx-bound (app-pool) Record the
		// governance handlers compose with their surrounding claims tx, so the
		// audit row commits or rolls back atomically with the action
		// (access_change_log_all RLS gates the in-tx write by org); admin half
		// stays pinned to s.admin so RecordSystem inside WithTx routes outside the
		// tx — same autonomous-commit shape ExternalActions uses. See TFAC-471 /
		// TFAC-486.
		AccessChangeLog: newAccessChangeLogStore(tx, s.admin),
		// ExternalActions: app-side Record routes through the tx so the audit row
		// commits or rolls back atomically with the action it records (the server
		// approval flips, manual bot runs); admin half stays pinned to s.admin so
		// RecordSystem + ListByOrgSystem inside WithTx route outside the tx — same
		// autonomous-commit shape Artifacts / Events use for their admin halves.
		ExternalActions: newExternalActionStore(tx, s.admin),
		// Spend: app half is the claims-set tx (ListSpend / SpendByCategory run
		// under the surrounding claims, so the security_invoker view's base-table
		// RLS scopes them to the requesting user); admin half stays pinned to
		// s.admin so SpendByCategorySystem routes outside the tx and reads org-wide
		// (the TFAC-477 cap is claims-less by design). Read-only. See TFAC-472.
		Spend: newSpendStore(tx, s.admin),
		// Marketplace: app half is the claims-set tx (every request-facing
		// method is RLS-gated); admin half stays pinned to s.admin so
		// RecomputeStatsSystem (TFAC-540) routes outside the tx and bypasses
		// RLS for its cross-team aggregate, mirroring Spend/ExternalActions'
		// split.
		Marketplace: newMarketplaceStore(tx, s.admin),
		// ConversationPendingInput: bound to the claims tx (not s.admin) so a resume
		// wake's input write commits atomically with its status flip under the
		// resuming user's claims — the RLS policy admits it via the run's own
		// visibility. Consume (claim time) runs system-side off the top-level
		// store, never this tx-bound handle.
		ConversationPendingInput: newConversationPendingInputStore(tx),
		// Permissions: the app half is the tx, so the pending read runs under
		// the caller's claims in the same transaction that authorized the
		// conversation. The admin half stays pinned to s.admin — every write
		// is a delegate goroutine's, never a request's, and tf_app holds no
		// write grant on the table anyway.
		Permissions: newPermissionStore(tx, s.admin),
		// OrgEventSources: app half is the tx, so a member's read and an
		// admin's write both run under the caller's claims; the admin half
		// stays pinned to s.admin so the router's and poller's ...System read
		// routes around RLS the way every other claims-less read does.
		OrgEventSources: newOrgEventSourceStore(tx, s.admin),
		// ModelAvailability has no admin half: every write is a request-path
		// gesture, so the tx IS the whole store.
		ModelAvailability: newModelAvailabilityStore(tx),
		// Opaque extension bundles (the Enterprise Edition SSO stores) built
		// from the same (app=tx, admin=s.admin) handles, so their app/admin
		// pool split is identical to core's own stores — the login-time reads
		// (GetByProviderID / GetVerifiedByDomain) run on s.admin (claims-less),
		// the CRUD on the claims tx.
		Ext: db.BuildStoreExtensions("postgres", tx, s.admin),
	}
}
