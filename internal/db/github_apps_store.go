package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
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
//
// CreateForOrg, SetActive, UpsertInstallation, SetInstallationSuspension and
// MarkInstallationRemoved hand back the row they persisted, read off
// RETURNING on the write statement itself rather than a follow-up SELECT,
// projecting GetForOrg's column list for the app row and
// ListInstallationsForOrg's for an installation. SetActive,
// SetInstallationSuspension and MarkInstallationRemoved each keep their
// documented no-op-on-absent-row contract: a miss is (nil, nil), not an
// error, matching GetForOrg's own nil-on-absent shape rather than an
// ErrNoSuchX sentinel. DeleteForOrg (a delete) and the three reconciles
// (BackfillInstallationsFromAPI / RefreshManagedInstallations /
// RefreshAllManagedInstallations — set reconciliation from a provider
// enumeration) stay exempt, each stated at the method.
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

	// CreateForOrg inserts a new org_github_apps row and returns it, read off
	// RETURNING on the insert itself. The row is written with app.Active
	// verbatim: a fresh setup (no org PAT) registers an active App; a
	// registration staged during a PAT→App switch (an org PAT is still live)
	// writes active=false so the PAT stays the live credential until an atomic
	// cutover (TFAC-328). This is a plain insert, not an upsert — it returns an
	// error wrapping ErrGitHubAppExists if the org already has a registration
	// (the PK constraint fires) rather than replacing it — a staged App
	// occupies the slot just as an active one does; DeleteForOrg frees it.
	CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) (domain.OrgGitHubApp, error)

	// SetActive flips the registration's active flag for orgID and returns the
	// updated row. The bit is the staged/live discriminator in the GitHub-access
	// either/or model: a staged App (active=false) is registered but not yet
	// minting tokens (resolver tier 1 skips it, github_ready ignores it) while
	// the org PAT stays live; the PAT→App cutover flips it true in the same
	// transaction it deletes the org PAT, so XOR holds after the commit. A
	// no-op on an org with no registration row: (nil, nil), not an error —
	// the RETURNING clause matching zero rows is exactly the unchecked-UPDATE
	// hole this signature closes, since a caller can no longer mistake a
	// silent zero-row flip for a successful one.
	SetActive(ctx context.Context, orgID string, active bool) (*domain.OrgGitHubApp, error)

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
	//
	// Exempt from the returned-row rule: it is a delete.
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

	// InstallationOwnerSystem reports which org holds installationID on
	// githubHost as a LIVE installation (removed_at IS NULL), or "" when none
	// does. It is the uniqueness gate of the bind ceremony: with one App key
	// serving many workspaces, an installation GitHub reports is not
	// self-evidently anybody's, and binding one that another workspace already
	// holds would hand that account's repositories to a second tenant.
	//
	// Scoped by host as well as id because an installation id is unique per
	// GitHub deployment and not universally — a self-host aggregating two GHES
	// instances can hold the same number twice, meaning two unrelated
	// installations.
	//
	// It returns the org id and nothing else, which is the whole answer a
	// caller may have: the refusal it feeds must never name the other
	// workspace. A removed row is not an owner — an uninstalled installation
	// reaches nothing, and re-binding it is an ordinary new bind.
	//
	// This read is a check, not a guarantee. Nothing in the schema stops a
	// second org writing the same (host, installation_id) between this call and
	// the caller's write, so a caller enforcing uniqueness has to serialize
	// itself on the installation's identity — a lock keyed by the ORG cannot,
	// since the two racers hold different org keys.
	//
	// System (claims-free) by construction: the question spans orgs, so no
	// caller's claims could authorize it. Admin pool in Postgres, like the
	// installation writes.
	InstallationOwnerSystem(ctx context.Context, githubHost, installationID string) (string, error)

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
	// GitHubHost takes the login's rule too, and is normalized on the way in
	// (EffectiveGitHubHost): both writers resolve it from the org's
	// github_base_url, which is the only thing it can be, so the value they
	// carry is always current and an empty one means "the org configured no
	// base URL" — the public host. That fold is what keeps the NOT NULL column
	// free of empty strings no matter which writer built the struct.
	//
	// The suspension fields are overwritten unconditionally, like the login
	// and unlike the id: both callers see GitHub's whole answer for the
	// installation (the reconcile lists it, the webhook is told about it), so
	// a zero SuspendedAt here means "GitHub says this installation is not
	// suspended", never "this writer didn't look". That is what makes a
	// re-install clear an inherited suspension in the same statement it clears
	// removed_at — the installation a user just re-installed is not the
	// suspended one they removed.
	//
	// Returns the persisted row, read off RETURNING on the upsert itself and
	// projecting ListInstallationsForOrg's column list and scanner. Always
	// produces one row — insert or conflict — so there is no miss case.
	UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) (domain.OrgGitHubAppInstallation, error)

	// SetInstallationSuspension records a suspension transition on one
	// installation and returns the updated row: a non-zero suspendedAt
	// suspends it (with suspendedBy, the suspending GitHub login, or "" when
	// the source named no one), a zero suspendedAt clears both columns. It is
	// the webhook's targeted write — installation.suspend / .unsuspend say
	// only that the state changed, so nothing else on the row is touched.
	//
	// Deliberately not scoped to live rows: a suspend that arrives after the
	// installation was removed writes the columns on the removed row, which no
	// reader surfaces (they all filter removed_at IS NULL) and no revival
	// depends on — RETURNING still hands back that row, since the row exists,
	// it's simply invisible to the active-only readers. A no-op (nil, nil,
	// not an error) on an installation the mirror has never seen — the next
	// reconcile mints the row with the suspension already on it. Admin pool
	// in Postgres, like the other installation writes.
	SetInstallationSuspension(ctx context.Context, orgID, installationID string, suspendedAt time.Time, suspendedBy string) (*domain.OrgGitHubAppInstallation, error)

	// MarkInstallationRemoved soft-deletes one installation by stamping
	// removed_at = now() (a no-op — (nil, nil) — on an already-removed or
	// absent row) and returns the row it just removed. Scoped by orgID as
	// well as installation_id: installation IDs are unique only per GitHub
	// host, so the org binding keeps a delete for one tenant from touching
	// another's same-numbered row. domain.OrgGitHubAppInstallation has no
	// RemovedAt field — readers filter removed_at IS NULL, so the domain type
	// stays active-only and this operates at the SQL level; the returned row
	// is therefore one no ListInstallationsForOrg call will ever find again,
	// which is what the row is — the caller's own answer to "which
	// installation did this remove", not a resource any other read surfaces.
	//
	// The installation row is the single row this method answers for. The
	// reachable_repositories and reachable_scopes deletes in the same
	// transaction are its cascade, not a second write this return value
	// speaks to — they're a reach-cache invalidation nothing reads as a
	// resource of its own. Admin pool in Postgres.
	MarkInstallationRemoved(ctx context.Context, orgID, installationID string) (*domain.OrgGitHubAppInstallation, error)

	// BackfillInstallationsFromAPI is the reconcile / system-of-record for a
	// workspace that brought its OWN App: it mints an App JWT from the org's
	// App PEM (read via SecretStore.GetSystem), calls GET
	// {apiBase}/app/installations, upserts every installation returned, and
	// soft-removes any active row GitHub no longer reports (so a missed
	// installation.deleted webhook or an API-only deployment converges). It
	// DISCOVERS, and may, because the org's own App key is the tenant boundary:
	// that key lists the installations of that App and no others, so every
	// installation it returns unambiguously belongs to orgID. A no-op when the
	// org has no registered App. Runs in all modes; the invocation (poller
	// cycle + UI refresh) is a separate concern.
	//
	// It is therefore structurally not the managed class's reconcile, and needs
	// no class check to stay out of it: a workspace riding the deployment App
	// holds no org_github_apps row and can hold none — that table is one row per
	// org with a UNIQUE app_id, so N orgs cannot each name the one shared App —
	// so this method finds nothing to mint a JWT from and returns. See
	// RefreshManagedInstallations for what such a workspace gets instead.
	//
	// Reconciles for any REGISTERED App regardless of its active flag: a
	// staged App (active=false, mid PAT→App switch) must still be able to
	// discover its installations so the cutover preflight + install
	// verification can run before the bit is flipped (TFAC-328). The poller
	// only invokes this for active Apps (its own gate), so widening here
	// doesn't make a staged App poll.
	//
	// Exempt from the returned-row rule: it reconciles a whole installation
	// set from a provider enumeration, so there is no single row a return
	// value could name.
	BackfillInstallationsFromAPI(ctx context.Context, orgID string) error

	// RefreshManagedInstallations is the same reconcile for a workspace on the
	// DEPLOYMENT App — the one App key that serves many workspaces — and the
	// difference is the whole point: for the managed class the reconcile
	// REFRESHES; it never DISCOVERS. It may update rows that already exist and
	// may never create one. Discovery belongs exclusively to the bind ceremony,
	// which is the only thing that can assert an installation is this
	// workspace's.
	//
	// Concretely it lists GET {apiBase}/app/installations under the deployment
	// key, keeps only the installations this org has already bound, and applies
	// that filtered set: account_login, account_id, the suspension pair and
	// repository_selection converge on the bound rows, and a bound installation
	// GitHub no longer reports is soft-removed, cascading to its reachable-repo
	// rows exactly as MarkInstallationRemoved does. Every other tenant's
	// installation in that listing is left entirely alone — not written and
	// removed, never written.
	//
	// deployment is a parameter rather than an ambient read because the
	// deployment App is operator environment config read once at boot and handed
	// to its consumers; two independent reads are two answers that can disagree.
	// An unconfigured one (the ordinary state of a local process, and of a
	// deployment whose orgs all bring their own App) is an error, not a silent
	// no-op — a managed org whose shared key has gone missing must fail where
	// someone can see it.
	//
	// It refuses an org that is not on the managed class, and that refusal is
	// load-bearing rather than defensive tidiness: run against a BYO org, this
	// would diff that org's bound installations against a listing produced by a
	// key that has never seen them and soft-remove every one.
	//
	// Exempt from the returned-row rule for the same reason as the method
	// above: a whole set reconciled from a provider enumeration names no row.
	RefreshManagedInstallations(ctx context.Context, orgID string, deployment githubapp.DeploymentApp) error

	// RefreshAllManagedInstallations is the cadence pass for the managed class:
	// the same refresh as RefreshManagedInstallations, for EVERY managed
	// workspace at once, from one listing per GitHub host rather than one per
	// org. Under a shared key the listing is the same whoever asks, so the
	// per-org shape spends one shared rate budget N times for N identical
	// answers; this reads every managed workspace's bound set in one query,
	// lists once, and fans the answer out to the orgs that bound each
	// installation (db.RefreshManagedInstallationSets holds the mechanics).
	//
	// Same invariants as the sibling, held by the same filter: bound rows
	// converge — account login and id, the suspension pair, repository_selection
	// — and a bound installation GitHub no longer reports is soft-removed with
	// its reachable-repo cascade; an installation no workspace bound is never
	// written; a failed listing changes nothing. A workspace on any other class
	// is not in the read at all, so nothing here can touch a BYO org's rows.
	//
	// Two scopes, two methods, and the split is deliberate: the sibling's
	// (ctx, orgID, deployment) signature is right for the Settings button — an
	// admin refreshing their own workspace — and is not a lever any tenant can
	// pull to make the deployment enumerate everything. This one has no org
	// parameter because it has no tenant caller: it runs on the poll cadence, on
	// the brain-lease holder, and nowhere a request can reach.
	//
	// A deployment with no managed workspace holding a bound installation is
	// answered without a request. One that has some and no configured deployment
	// App is an error, as for the sibling: a managed workspace whose shared key
	// has gone missing must fail where someone can see it. Claims-free by
	// construction (the read spans orgs); admin pool in Postgres.
	//
	// Exempt from the returned-row rule for the same reason as the two above: a
	// whole set reconciled from a provider enumeration names no row.
	RefreshAllManagedInstallations(ctx context.Context, deployment githubapp.DeploymentApp) error
}

// ErrGitHubAppExists is returned by CreateForOrg when the org already
// has a registered GitHub App. The handler maps this to 409 Conflict.
type ErrGitHubAppExists struct{ OrgID string }

func (e *ErrGitHubAppExists) Error() string {
	return "org " + e.OrgID + " already has a GitHub App registered"
}
