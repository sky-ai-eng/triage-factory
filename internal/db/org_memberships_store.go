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

// ErrLastOwnerGuard is returned by OrgMembershipsStore.UpdateRole / Remove /
// TransferOwnership when the write would leave the org with no 'owner' role.
// The authority is the tf.guard_org_owners statement trigger (Postgres
// baseline), which raises SQLSTATE 23514; the store translates that into this
// sentinel so the handler can answer 409 without importing the pg driver. The
// invariant is never re-implemented in Go — the DB is the single source of
// truth.
var ErrLastOwnerGuard = errors.New("db: each org must retain at least one owner")

// ErrNotOrgOwner is returned by OrgMembershipsStore.TransferOwnership when the
// caller isn't the org's current owner. The authority is the
// tf.guard_org_owner_transfer trigger (Postgres baseline), which raises
// SQLSTATE 42501 on an UPDATE of orgs.owner_user_id by anyone other than the
// present owner; the store translates that into this sentinel so the handler
// can answer 403 without importing the pg driver. The transfer handler also
// front-gates on tf.user_owns_org, so this is the defense-in-depth backstop
// for a between-check-and-write ownership race.
var ErrNotOrgOwner = errors.New("db: only the current org owner can transfer ownership")

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
// Multi-mode only in practice: the /org surface is multi-only and its
// handlers 404 in local mode. The SQLite impl exists so the local store
// bundle satisfies the interface; its mutators return ErrNotApplicableInLocal.
type OrgMembershipsStore interface {
	// ListWithIdentity returns one page of orgID's members with their org
	// role and host-scoped identity readiness plus the unpaged total,
	// ordered by display name then user id — a total order, so the pages
	// partition the roster rather than repeating its head. githubBaseURL /
	// jiraBaseURL are the org's configured hosts (raw, read from
	// org_settings by the caller) —
	// the impl resolves them to the host identities are actually keyed under
	// (EffectiveGitHubHost: an unset github_base_url resolves to github.com,
	// where most identities live; Jira has no default host, so an unset one
	// matches nothing, which is correct). A member's GitHubUsername /
	// JiraAccountID is nil when they hold no binding on that host. The roster
	// reads under the app pool (org_memberships_select RLS); the identity
	// enrichment reads under the admin pool (see the type doc).
	ListWithIdentity(ctx context.Context, orgID, githubBaseURL, jiraBaseURL string, opts ListOpts) ([]domain.OrgMember, int, error)

	// RoleFor returns userID's org_role in orgID ("owner" | "admin" |
	// "member"), or "" when they hold no membership row. App pool: the
	// org_memberships_select policy already scopes the read to orgs the
	// caller belongs to, so a non-member's probe of someone else's org
	// answers "" rather than a role. The canonical org read reports it as
	// the caller's own relationship to the org.
	RoleFor(ctx context.Context, orgID, userID string) (string, error)

	// UpdateRole sets userID's org role in orgID to role
	// ("owner" | "admin" | "member") and returns the prior role (so the
	// governance audit log can record the old→new transition). App pool:
	// org_memberships_update RLS gates the write to org admins. Returns
	// ErrLastOwnerGuard when the change would drop the org's last owner (the
	// guard trigger fires), and ErrOrgMemberNotFound when no row matches.
	// Callers validate role against the allowed set before calling, so an
	// invalid enum never reaches the column.
	UpdateRole(ctx context.Context, orgID, userID, role string) (oldRole string, err error)

	// Remove deletes userID's membership in orgID. App pool:
	// org_memberships_delete RLS permits the caller to remove themselves
	// (self-leave, any role) or, as an org admin, anyone. Returns
	// ErrLastOwnerGuard when removing the org's last owner (guard trigger),
	// and ErrOrgMemberNotFound when no row matches.
	//
	// Exempt from the returned-row rule: it is a delete.
	Remove(ctx context.Context, orgID, userID string) error

	// TransferOwnership hands org ownership from currentOwnerID to newOwnerID
	// in a single transaction, in the order the tf.guard_org_owner_transfer
	// trigger demands: (1) promote newOwnerID to 'owner' in org_memberships,
	// (2) repoint orgs.owner_user_id to newOwnerID, (3) demote currentOwnerID
	// to 'admin'. The BEFORE-UPDATE trigger on orgs refuses to repoint
	// owner_user_id to anyone who doesn't already hold role='owner', so the
	// promote MUST land first; it also requires the caller to be the present
	// owner, so this MUST run under the current owner's claims (the handler
	// calls it inside WithTx as the owner, on the app pool — not the admin
	// pool, whose claims-less connection would make tf.current_user_id() NULL).
	//
	// Returns ErrNotOrgOwner when the caller isn't the current owner (the
	// trigger's SQLSTATE 42501), ErrLastOwnerGuard when an owner-count
	// invariant trips (23514, from either guard), and ErrOrgMemberNotFound
	// when newOwnerID isn't a member (the promote matches no row). App pool;
	// multi-mode only.
	TransferOwnership(ctx context.Context, orgID, currentOwnerID, newOwnerID string) error
}
