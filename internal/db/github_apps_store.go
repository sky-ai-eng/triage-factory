package db

import (
	"context"
	"time"

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
// baseline schema).
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

	// CreateForOrg inserts a new org_github_apps row. The row is written
	// with app.Active verbatim: a fresh setup (no org PAT) registers an
	// active App; a registration staged during a PAT→App switch (an org PAT
	// is still live) writes active=false so the PAT stays the live credential
	// until an atomic cutover (TFAC-328). Returns an error wrapping
	// ErrGitHubAppExists if the org already has a registration (the PK
	// constraint fires) — a staged App occupies the slot just as an active one
	// does; DeleteForOrg frees it.
	CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) error

	// SetActive flips the registration's active flag for orgID. The bit is the
	// staged/live discriminator in the GitHub-access either/or model: a staged
	// App (active=false) is registered but not yet minting tokens (resolver
	// tier 1 skips it, github_ready ignores it) while the org PAT stays live;
	// the PAT→App cutover flips it true in the same transaction it deletes the
	// org PAT, so XOR holds after the commit. A no-op (no error) on an org with
	// no registration row.
	SetActive(ctx context.Context, orgID string, active bool) error

	// DeleteForOrg hard-deletes the org's App registration row AND every
	// org_github_app_installations row for the org. It is the "switch to PAT"
	// teardown and the "discard a staged switch" exit (TFAC-328): a torn-down
	// App has nothing to mirror, and the installations mirror is rebuildable
	// from the API if the App is ever re-registered, so the rows are removed
	// rather than soft-deleted. The App's Vault secrets (client_secret, PEM,
	// webhook_secret) are NOT deleted here — their refs live on the row the
	// caller still holds, so the handler deletes them alongside this call.
	// A no-op (no error) on an org with no registration. In Postgres the
	// registration-row delete is an app-pool (RLS org-admin-gated) write while
	// the installations delete routes to the admin pool (tf_app is denied all
	// writes to org_github_app_installations); the two are not one transaction,
	// but a lingering installation row is a harmless orphan once the
	// registration row is gone (the resolver short-circuits on the absent App
	// before ever reading installations).
	DeleteForOrg(ctx context.Context, orgID string) error

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
	//
	// With BackfillInstallationsFromAPI this is one of exactly two writers of
	// AccountID, and the only two: the id is mirrored from GitHub or it is
	// not written at all. An empty AccountID leaves a stored id intact (the
	// backfill is opportunistic — a NULL fills in on the next write that
	// names the account, and is never re-emptied), while AccountLogin is
	// overwritten unconditionally so a rename reaches the mirror.
	//
	// The suspension fields are overwritten unconditionally, like the login
	// and unlike the id: both callers see GitHub's whole answer for the
	// installation (the reconcile lists it, the webhook is told about it), so
	// a zero SuspendedAt here means "GitHub says this installation is not
	// suspended", never "this writer didn't look". That is what makes a
	// re-install clear an inherited suspension in the same statement it clears
	// removed_at — the installation a user just re-installed is not the
	// suspended one they removed.
	UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) error

	// SetInstallationSuspension records a suspension transition on one
	// installation: a non-zero suspendedAt suspends it (with suspendedBy, the
	// suspending GitHub login, or "" when the source named no one), a zero
	// suspendedAt clears both columns. It is the webhook's targeted write —
	// installation.suspend / installation.unsuspend say only that the state
	// changed, so nothing else on the row is touched.
	//
	// Deliberately not scoped to live rows: a suspend that arrives after the
	// installation was removed writes the columns on the removed row, which no
	// reader surfaces (they all filter removed_at IS NULL) and no revival
	// depends on. A no-op (no error) on an installation the mirror has never
	// seen — the next reconcile mints the row with the suspension already on
	// it. Admin pool in Postgres, like the other installation writes.
	SetInstallationSuspension(ctx context.Context, orgID, installationID string, suspendedAt time.Time, suspendedBy string) error

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
	//
	// Reconciles for any REGISTERED App regardless of its active flag: a
	// staged App (active=false, mid PAT→App switch) must still be able to
	// discover its installations so the cutover preflight + install
	// verification can run before the bit is flipped (TFAC-328). The poller
	// only invokes this for active Apps (its own gate), so widening here
	// doesn't make a staged App poll.
	BackfillInstallationsFromAPI(ctx context.Context, orgID string) error
}

// ErrGitHubAppExists is returned by CreateForOrg when the org already
// has a registered GitHub App. The handler maps this to 409 Conflict.
type ErrGitHubAppExists struct{ OrgID string }

func (e *ErrGitHubAppExists) Error() string {
	return "org " + e.OrgID + " already has a GitHub App registered"
}
