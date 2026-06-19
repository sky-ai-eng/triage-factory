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
// Postgres routes every method through the app pool: the org_memberships_*
// RLS policies gate reads on org membership (org_memberships_select) and
// writes on org admin (org_memberships_update) or self-OR-admin
// (org_memberships_delete). The tf.guard_org_owners statement trigger is the
// sole authority on the last-owner invariant — UpdateRole / Remove surface
// its SQLSTATE 23514 as ErrLastOwnerGuard rather than pre-checking in Go.
//
// Multi-mode only in practice: the /org surface is hosted-only and its
// handlers 404 in local mode. The SQLite impl exists so the local store
// bundle satisfies the interface; its mutators return ErrNotApplicableInLocal.
type OrgMembershipsStore interface {
	// ListWithIdentity returns every member of orgID with their org role
	// and host-scoped identity readiness, ordered by display name then
	// user id for a stable roster. githubBaseURL / jiraBaseURL are the
	// org's configured hosts (read from org_settings by the caller);
	// identity is resolved per (user, host) the same way the per-user
	// GitHub/Jira lookups key, so a member's GitHubUsername / JiraAccountID
	// is nil when they hold no binding on the org's host. App pool
	// (org_memberships_select RLS gates by org membership).
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
