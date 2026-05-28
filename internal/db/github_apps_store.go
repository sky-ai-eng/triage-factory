package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// GitHubAppsStore owns the org_github_apps table — per-org GitHub App
// registrations created through the manifest flow. One row per org
// (org_id is the PK); orgs using the deployment-default App have no
// row. Secrets (client_secret, PEM, webhook_secret) are stored in
// Vault via SecretStore; this table holds only the ref names.
//
// # Pool split (Postgres)
//
//   - app: all methods. The org_github_apps RLS policies gate reads
//     by org membership and writes by org admin, so the request
//     handler's JWT claims are the right identity context.
//
// # Local mode (SQLite)
//
// The manifest flow works in both modes. The SQLite impl reads/writes
// the org_github_apps table directly (the table exists in the SQLite
// baseline schema from SKY-348).
type GitHubAppsStore interface {
	// GetForOrg returns the org's registered GitHub App, or nil if
	// the org has no App registration (uses the deployment default
	// or PAT-borrow). sql.ErrNoRows is folded into the nil return.
	GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error)

	// GetForOrgSystem mirrors GetForOrg but routes through the admin
	// pool with no JWT-claims dependency, for system/background callers
	// that have only a trusted orgID — the webhook receiver (org_id from
	// the URL path) reading WebhookSecretRef, and the backfill loop.
	// Same nil-on-absent contract as GetForOrg. Same discipline as
	// GetForOrgSystem vs GetForOrg on AgentStore: request handlers use
	// the claims-checked GetForOrg.
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error)

	// CreateForOrg inserts a new org_github_apps row. Returns an
	// error wrapping ErrGitHubAppExists if the org already has a
	// registration (the PK constraint fires).
	CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) error

	// ListInstallationsForOrg returns the org's active App
	// installations (removed_at IS NULL), ordered by account_login.
	// Empty slice when the org has no App or no live installations.
	ListInstallationsForOrg(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error)

	// ListInstallationsForOrgSystem mirrors ListInstallationsForOrg but
	// routes through the admin pool in Postgres — no JWT claims required.
	// The credential resolver (internal/github) calls this to pick the
	// right installation for a target account, and it runs from both the
	// claims-bearing request path and the claims-free poller, so it must
	// not depend on request.jwt.claims. Same active-only (removed_at IS
	// NULL) contract and ordering as the app-pool variant. SQLite
	// collapses to the same query.
	ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error)

	// UpsertInstallation mirrors one installation into
	// org_github_app_installations. Idempotent on the (org_id,
	// installation_id) composite key; ON CONFLICT it refreshes the
	// account fields and clears removed_at, so re-observing (backfill) or
	// reinstalling (webhook) the same installation revives a
	// previously-removed row. installed_at is set on first insert and
	// preserved across upserts (a zero InstalledAt defaults to now()). In
	// Postgres this writes on the admin pool — tf_app is denied all
	// writes to this table by RLS.
	UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) error

	// MarkInstallationRemoved soft-deletes one installation by stamping
	// removed_at = now() (a no-op on an already-removed or absent row).
	// Scoped by orgID as well as installation_id: installation IDs are
	// unique only per GitHub host, so the org binding keeps a delete for
	// one tenant from touching another's same-numbered row.
	// domain.OrgGitHubAppInstallation has no RemovedAt field — readers
	// filter removed_at IS NULL, so the domain type stays active-only and
	// this operates at the SQL level. Admin pool in Postgres.
	MarkInstallationRemoved(ctx context.Context, orgID, installationID string) error

	// BackfillInstallationsFromAPI is the reconcile / system-of-record:
	// it mints an App JWT from the org's App PEM (read via
	// SecretStore.GetSystem), calls GET {apiBase}/app/installations,
	// upserts every installation returned, and soft-removes any active
	// row GitHub no longer reports (so a missed installation.deleted
	// webhook or an API-only deployment converges). v1 is per-org Apps
	// only, so every returned installation unambiguously belongs to
	// orgID. A no-op when the org has no registered App. Runs in all
	// modes; the invocation (poller cycle + UI refresh) is a separate
	// concern.
	BackfillInstallationsFromAPI(ctx context.Context, orgID string) error
}

// ErrGitHubAppExists is returned by CreateForOrg when the org already
// has a registered GitHub App. The handler maps this to 409 Conflict.
type ErrGitHubAppExists struct{ OrgID string }

func (e *ErrGitHubAppExists) Error() string {
	return "org " + e.OrgID + " already has a GitHub App registered"
}
