package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// JiraAppsStore owns the org_jira_apps table — per-org Atlassian OAuth (3LO)
// app registrations (the BYO-app override / local-supplied app). One row per
// org (org_id is the PK); orgs using the deployment app have no row. The
// client_secret is stored in the secret store (Vault in multi mode, the OS
// keychain in local) via SecretStore; this table holds only the ref name.
// Mirrors GitHubAppsStore, minus installations (an Atlassian OAuth app has
// none).
//
// # Pool split (Postgres)
//
//   - app: all methods. The org_jira_apps RLS policies gate reads by org
//     membership and writes by org admin, so the request handler's JWT
//     claims are the right identity context.
//   - admin: GetForOrgSystem only — the no-claims read the OAuth-app resolver
//     needs (credential resolution is a system operation, like
//     GitHubAppsStore.GetForOrgSystem).
//
// # Local mode (SQLite)
//
// One connection, no RLS — GetForOrgSystem == GetForOrg. The local-supplied
// BYO app is written here too (its secret lands in the keychain).
type JiraAppsStore interface {
	// GetForOrg returns the org's registered Atlassian OAuth app, or nil when
	// the org has no per-org override (it uses the deployment app in multi, or
	// has none in local). sql.ErrNoRows is folded into the nil return.
	GetForOrg(ctx context.Context, orgID string) (*domain.OrgJiraApp, error)

	// GetForOrgSystem mirrors GetForOrg but routes through the admin pool with
	// no JWT-claims dependency, for the credential resolver (a system caller
	// with a trusted orgID). Same nil-on-absent contract as GetForOrg. SQLite
	// collapses to the same query.
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgJiraApp, error)

	// UpsertForOrg inserts or replaces the org's Atlassian OAuth app row.
	// Unlike GitHubAppsStore.CreateForOrg (create-once-then-conflict), this is
	// an upsert: the settings card lets an admin enter OR replace the BYO app,
	// and an OAuth app has no staging/cutover lifecycle that a re-register would
	// disturb. registered_at is preserved across upserts; registered_by_user_id
	// is refreshed to the most recent registrant.
	//
	// Returns the persisted row, read off RETURNING on the write statement
	// itself rather than from a follow-up SELECT and projecting GetForOrg's
	// column list and scanner. registered_at is the point: the conflict arm
	// preserves the original, so on a re-register the row's value belongs to
	// the call that first registered the app, and app cannot describe it.
	UpsertForOrg(ctx context.Context, app domain.OrgJiraApp) (domain.OrgJiraApp, error)

	// DeleteForOrg hard-deletes the org's Atlassian OAuth app row. The "remove
	// BYO app" teardown. The row's client_secret lives in the secret store
	// under ClientSecretRef, which the caller still holds — so the handler
	// deletes that secret alongside this call (this method only touches the
	// relational row). A no-op (no error) on an org with no registration.
	//
	// Exempt from the returned-row rule: it is a delete.
	DeleteForOrg(ctx context.Context, orgID string) error
}
