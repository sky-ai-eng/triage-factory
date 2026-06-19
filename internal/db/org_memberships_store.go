package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrOrgMemberNotFound is returned by OrgMembershipsStore.UpdateRole /
// Remove when no membership row matches (org_id, user_id) — so a stale or
// bogus user_id surfaces as a 404 rather than a silent no-op. Distinct from
// an RLS denial: the handlers gate org-admin (or self) before calling, so a
// zero-row result past that gate means the target simply isn't in the org.
var ErrOrgMemberNotFound = errors.New("db: org member not found")

// ErrLastOwnerGuard is returned by OrgMembershipsStore.UpdateRole / Remove
// when the write would leave the org with no 'owner' role. The authority is
// the tf.guard_org_owners statement trigger (Postgres baseline), which
// raises SQLSTATE 23514; the store translates that into this sentinel so the
// handler can answer 409 without importing the pg driver. The invariant is
// never re-implemented in Go — the DB is the single source of truth.
var ErrLastOwnerGuard = errors.New("db: each org must retain at least one owner")

// OrgMembershipsStore owns the org_memberships table — the (user_id, org_id,
// role) rows that place a human in an org with an org-level role
// (owner | admin | member). The org People roster (TFAC-417) reads the full
// set via ListWithIdentity; the role-change / remove affordances mutate it
// via UpdateRole / Remove.
//
// Postgres uses both pools. The app pool carries the RLS-gated work: the
// org_memberships_* policies gate the roster read on org membership
// (org_memberships_select) and the writes on org admin (org_memberships_update)
// or self-OR-admin (org_memberships_delete), and the tf.guard_org_owners
// statement trigger is the sole authority on the last-owner invariant —
// UpdateRole / Remove surface its SQLSTATE 23514 as ErrLastOwnerGuard rather
// than pre-checking in Go. The admin pool carries ONLY ListWithIdentity's
// cross-member GitHub/Jira identity enrichment: the user_*_identities select
// policies are self-only, so a member can't read a peer's identity under RLS;
// the enrichment bypasses that but stays scoped to the org by a membership
// join, so it surfaces only the same members the RLS roster already returned.
//
// Multi-mode only in practice: the /org surface is hosted-only and its
// handlers 404 in local mode. The SQLite impl exists so the local store
// bundle satisfies the interface; its mutators return ErrNotApplicableInLocal.
type OrgMembershipsStore interface {
	// ListWithIdentity returns every member of orgID with their org role
	// and host-scoped identity readiness, ordered by display name then
	// user id for a stable roster. githubBaseURL / jiraBaseURL are the
	// org's configured hosts (raw, read from org_settings by the caller) —
	// the impl resolves them to the host identities are actually keyed under
	// (EffectiveGitHubHost: an unset github_base_url resolves to github.com,
	// where most identities live; Jira has no default host, so an unset one
	// matches nothing, which is correct). A member's GitHubUsername /
	// JiraAccountID is nil when they hold no binding on that host. The roster
	// reads under the app pool (org_memberships_select RLS); the identity
	// enrichment reads under the admin pool (see the type doc).
	ListWithIdentity(ctx context.Context, orgID, githubBaseURL, jiraBaseURL string) ([]domain.OrgMember, error)

	// UpdateRole sets userID's org role in orgID to role
	// ("owner" | "admin" | "member"). App pool: org_memberships_update RLS
	// gates the write to org admins. Returns ErrLastOwnerGuard when the
	// change would drop the org's last owner (the guard trigger fires), and
	// ErrOrgMemberNotFound when no row matches. Callers validate role
	// against the allowed set before calling, so an invalid enum never
	// reaches the column.
	UpdateRole(ctx context.Context, orgID, userID, role string) error

	// Remove deletes userID's membership in orgID. App pool:
	// org_memberships_delete RLS permits the caller to remove themselves
	// (self-leave, any role) or, as an org admin, anyone. Returns
	// ErrLastOwnerGuard when removing the org's last owner (guard trigger),
	// and ErrOrgMemberNotFound when no row matches.
	Remove(ctx context.Context, orgID, userID string) error
}
