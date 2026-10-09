package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// LinearAppsStore owns the org_linear_apps table — the org's own Linear OAuth
// app, the Linear sibling of JiraAppsStore. One row per org; an org using the
// deployment app has none. The client_secret lives in the secret store under
// the row's ClientSecretRef; this table holds only the ref name.
//
// # Pool split (Postgres)
//
//   - app: GetForOrg / UpsertForOrg / DeleteForOrg. RLS gates reads by org
//     membership and writes by org admin.
//   - admin: GetForOrgSystem, the no-claims read the OAuth-app resolver makes.
//
// # Local mode (SQLite)
//
// One connection, no RLS: GetForOrgSystem == GetForOrg.
type LinearAppsStore interface {
	// GetForOrg returns the org's registered Linear OAuth app, or nil when it
	// has none.
	GetForOrg(ctx context.Context, orgID string) (*domain.OrgLinearApp, error)

	// GetForOrgSystem is GetForOrg on the admin pool, for the OAuth-app
	// resolver. Same nil-on-absent contract.
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearApp, error)

	// UpsertForOrg inserts or replaces the org's app row and returns the row
	// it persisted. registered_at is preserved across upserts, so on a
	// replace the returned row disagrees with the struct passed in;
	// registered_by_user_id is refreshed to the latest registrant.
	UpsertForOrg(ctx context.Context, app domain.OrgLinearApp) (domain.OrgLinearApp, error)

	// DeleteForOrg removes the org's app row; a no-op when there is none. The
	// caller deletes the secret the row names.
	//
	// Exempt from the returned-row rule: it is a delete.
	DeleteForOrg(ctx context.Context, orgID string) error
}

// ErrWorkspaceInstalledElsewhere is LinearInstallsStore.UpsertSystem refused
// because another org holds a live install of the same Linear workspace.
var ErrWorkspaceInstalledElsewhere = errors.New("db: linear workspace is installed in another org")

// LinearInstallsStore owns the org_linear_installs table — the Linear
// workspace that installed the org's resolved OAuth app as an app user. One
// row per org; a live row holds its workspace against every other org.
//
// Every method is a ...System method on the admin pool in Postgres. The
// install callback and the disconnect write it there: the uniqueness check
// spans every org's rows, which no claims transaction can see, and tf_app is
// refused every write by RLS. Inside WithTx the store stays on the admin pool,
// so a write commits on its own rather than with the transaction; on SQLite it
// is bound to the transaction like every other store.
type LinearInstallsStore interface {
	// GetForOrgSystem returns the org's install row, live or removed, or nil
	// when the org never installed.
	GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgLinearInstall, error)

	// UpsertSystem writes inst as the org's live install, replacing any row the
	// org has, live or removed, and returns the row it persisted. InstalledAt
	// is stored as given. A workspace another org holds live is
	// ErrWorkspaceInstalledElsewhere, and nothing is written.
	UpsertSystem(ctx context.Context, inst domain.OrgLinearInstall) (domain.OrgLinearInstall, error)

	// MarkRemovedSystem stamps the org's live install removed for reason (a
	// domain.LinearInstallRemoved* value) and returns the removed row; nil
	// when the org has no live install.
	MarkRemovedSystem(ctx context.Context, orgID, reason string) (*domain.OrgLinearInstall, error)
}
