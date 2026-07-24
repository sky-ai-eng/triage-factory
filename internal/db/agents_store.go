package db

import (
	"context"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

//go:generate go run github.com/vektra/mockery/v2 --name=AgentStore --output=./mocks --case=underscore --with-expecter

// AgentStore owns the agents table — the org's workload identity.
// One row per org (UNIQUE(org_id) in Postgres; idempotent INSERT on a
// deterministic id in SQLite where org_id has no column). Distinct
// from the Conversation domain in internal/domain/agent.go: an Agent is
// "who acts," an Conversation is one execution by that actor.
//
// Audiences:
//
//   - Startup / org-create bootstrap (internal/db/bootstrap.go) —
//     Create with admin-pool routing in Postgres because at org-create
//     time no org_memberships row exists yet for the founder and the
//     agents_insert RLS policy would refuse.
//   - Future admin UI (D14) — Update + SetGitHubPATUser via
//     the app pool, admin-gated by RLS.
//   - D-Claims + delegate spawner — GetForOrg on every run
//     dispatch to pick the credential source.
//
// GitHub App identity is no longer modelled on agents — per-org
// registration + installation tracking lives in the org_github_apps /
// org_github_app_installations tables. A single text column on agents
// couldn't model the 1:N installation fan-out per org. The credential
// resolver reads those tables directly.
//
// # Pool split (Postgres)
//
//   - app pool — tf_app, RLS-active. GetForOrg, Update, SetGitHubPATUser.
//     agents_select gates reads by org access;
//     agents_insert/agents_update/agents_delete each gate writes by
//     tf.user_is_org_admin(org_id).
//   - admin pool — supabase_admin, BYPASSRLS. Create only. Justified
//     because boot-time bootstrap has no JWT claims and the founder's
//     org_memberships row is being inserted in the same transaction
//     as the agents row — at the agents-insert moment, the founder
//     isn't yet an admin per RLS's lookup.
//
// SQLite has one connection; assertLocalOrg pins orgID to LocalDefaultOrgID.
type AgentStore interface {
	// GetForOrg returns the org's agent row, or (nil, nil) if not yet
	// bootstrapped. Callers reading credentials handle the nil case
	// gracefully (degrade to keychain lookup in local mode, surface
	// "set up your bot" in multi mode).
	GetForOrg(ctx context.Context, orgID string) (*domain.Agent, error)

	// Create inserts the org's single agent row. Idempotent on
	// (org_id): a duplicate call returns the existing row's id
	// without error so bootstrap is safe to re-run after partial-
	// failure recovery. The Postgres impl routes this through the
	// admin pool (BYPASSRLS) because the only legitimate caller is
	// bootstrap; user-initiated agent creation isn't a v1 surface.
	//
	// Agent.ID on the input is IGNORED — the impl always uses
	// BootstrapAgentID(orgID). Both backends honor this contract so
	// GetForOrg can rely on the deterministic id (SQLite has no
	// UNIQUE(org_id) constraint, so a caller-supplied custom id
	// would create rows that GetForOrg's deterministic lookup could
	// never reach). The returned id is the deterministic derivation.
	Create(ctx context.Context, orgID string, a domain.Agent) (id string, err error)

	// Update changes the agent's mutable metadata: display name,
	// default model, default autonomy threshold, Jira service account.
	// The credential FK uses SetGitHubPATUser at a smaller surface
	// rather than letting Update touch it. Admin-only in Postgres via
	// RLS. No-op on invalid UUID in Postgres (matches SQLite TEXT-keyed
	// semantics).
	Update(ctx context.Context, orgID string, a domain.Agent) error

	// SetGitHubPATUser sets the PAT-borrow user FK. Used by local
	// install (where userID is the sentinel local user) and by
	// multi-mode small-org fallback. Admin-only in Postgres.
	SetGitHubPATUser(ctx context.Context, orgID, agentID, userID string) error

	// SetGitHubOrgLogin records the org credential's OWN GitHub login —
	// the login the bot/org PAT authenticates as, distinct from
	// SetGitHubPATUser (the user who pasted the PAT). Written by the
	// org-PAT setup/rebind writers (internal/server/credentials.go and
	// the other integrations.Save sites) so the credential resolver's
	// OrgIdentityFor PAT tier can stamp the org commit-author identity
	// (TFAC-452). Empty login clears it. Admin-only in Postgres, same
	// pool + RLS as SetGitHubPATUser. App-tier orgs never write here —
	// the App bot login (<slug>[bot]) resolves live from the App
	// registration, so this is the PAT path only.
	SetGitHubOrgLogin(ctx context.Context, orgID, agentID, login string) error

	// GetForOrgSystem mirrors GetForOrg but routes through the admin
	// pool in Postgres. Used by system / claims-free callers that have
	// no request JWT to drive RLS — e.g. the bootstrap chain
	// (BootstrapTeamAgent), the event router's auto-trigger gate +
	// agent-claim stamping (internal/routing), the delegate spawner's
	// actor stamping, and the credential resolver's PAT-borrow lookup.
	// Same admin/app split convention as the other stores in
	// this wave.
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.Agent, error)
}

// bootstrapAgentNamespace is the fixed UUID v4 used as the seed
// namespace for deterministic agent-id derivation per orgID. Hardcoded
// once so re-runs of bootstrap on the same orgID land on the same
// agents.id across both backends — SQLite stores it as TEXT, Postgres
// as UUID, the comparison is byte-identical.
var bootstrapAgentNamespace = uuid.MustParse("e1f7c4a3-9d62-4f1b-b8a5-6c3d2e9f1a7b")

// BootstrapAgentID returns the deterministic UUID for the org's bot
// row. Two regimes:
//
//   - Local mode (orgID == runmode.LocalDefaultOrgID): returns the
//     sentinel runmode.LocalDefaultAgentID directly. The SQLite
//     migration (202605120003_local_tenancy.sql) backfills the
//     pre-existing agents row to this id at upgrade time, so both
//     fresh installs and upgrades resolve to the same value here.
//   - Multi mode (any other orgID): UUIDv5 derivation from the
//     namespace + orgID. Same orgID → same UUID across installs.
//
// Two regimes rather than one because local mode's orgID is a
// sentinel UUID and applying UUIDv5 to it would produce a derived
// value that has no recognizable shape in logs. Using the
// LocalDefaultAgentID sentinel keeps every local-mode agent row's id
// readable as "the local bot" at a glance.
func BootstrapAgentID(orgID string) string {
	if orgID == runmode.LocalDefaultOrgID {
		return runmode.LocalDefaultAgentID
	}
	return uuid.NewSHA1(bootstrapAgentNamespace, []byte(orgID)).String()
}
