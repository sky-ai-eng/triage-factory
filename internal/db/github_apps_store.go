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

	// CreateForOrg inserts a new org_github_apps row. Returns an
	// error wrapping ErrGitHubAppExists if the org already has a
	// registration (the PK constraint fires).
	CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) error
}

// ErrGitHubAppExists is returned by CreateForOrg when the org already
// has a registered GitHub App. The handler maps this to 409 Conflict.
type ErrGitHubAppExists struct{ OrgID string }

func (e *ErrGitHubAppExists) Error() string {
	return "org " + e.OrgID + " already has a GitHub App registered"
}
