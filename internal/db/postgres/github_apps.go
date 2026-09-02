package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// gitHubAppsStore holds two pools. app is the claims-checked request-handler
// path (GetForOrg / CreateForOrg / ListInstallationsForOrg — RLS gates reads
// by org membership, writes by org admin). admin is the system/background
// path: installation writes (tf_app is denied all writes to
// org_github_app_installations by RLS) and the no-claims reads the webhook
// receiver + backfill need. secrets reads the App PEM for backfill via the
// claims-free GetSystem door.
type gitHubAppsStore struct {
	app     queryer
	admin   queryer
	secrets db.SecretStore
}

func newGitHubAppsStore(app, admin queryer, secrets db.SecretStore) db.GitHubAppsStore {
	return &gitHubAppsStore{app: app, admin: admin, secrets: secrets}
}

var _ db.GitHubAppsStore = (*gitHubAppsStore)(nil)

// scanGitHubApp is shared by every org_github_apps reader AND writer — GetForOrg's
// SELECT and CreateForOrg / SetActive's RETURNING all scan through it — so it
// leaves a raw Scan failure unwrapped rather than baking in a caller-specific
// verb ("get"/"insert"/"set"). Each caller wraps with its own context;
// wrapping here would misname a write's failure as a read (see
// scanSQLiteGitHubApp for the sibling that already gets this right).
func scanGitHubApp(row interface{ Scan(...any) error }) (*domain.OrgGitHubApp, error) {
	var (
		a            domain.OrgGitHubApp
		regBy        sql.NullString
		registeredAt sql.NullTime
		botUserID    sql.NullInt64
	)
	err := row.Scan(
		&a.OrgID, &a.AppID, &a.Slug, &a.ClientID,
		&a.ClientSecretRef, &a.PEMRef, &a.WebhookSecretRef,
		&a.OwnerType, &registeredAt, &regBy, &a.Active, &botUserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if registeredAt.Valid {
		a.RegisteredAt = registeredAt.Time
	}
	a.RegisteredByUserID = regBy.String
	a.BotUserID = botUserID.Int64 // NULL → 0 (bot user id unknown)
	return &a, nil
}

// pgGitHubAppColumns is the canonical projection of an org_github_apps row, in
// the order scanGitHubApp reads them. GetForOrg SELECTs it and CreateForOrg /
// SetActive RETURN it, so the write shape cannot drift from the read shape.
const pgGitHubAppColumns = `org_id, app_id, slug, client_id,
	       client_secret_ref, pem_ref, webhook_secret_ref,
	       owner_type, registered_at, registered_by_user_id, active, bot_user_id`

const selectGitHubAppCols = `
	SELECT ` + pgGitHubAppColumns + `
	  FROM org_github_apps
	 WHERE org_id = $1`

func (s *gitHubAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	app, err := scanGitHubApp(s.app.QueryRowContext(ctx, selectGitHubAppCols, orgID))
	if err != nil {
		return nil, wrapAppPoolPermErr(fmt.Errorf("get org_github_apps: %w", err), "github_apps.GetForOrg")
	}
	return app, nil
}

func (s *gitHubAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	app, err := scanGitHubApp(s.admin.QueryRowContext(ctx, selectGitHubAppCols, orgID))
	if err != nil {
		return nil, fmt.Errorf("get org_github_apps: %w", err)
	}
	return app, nil
}

func (s *gitHubAppsStore) CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) (domain.OrgGitHubApp, error) {
	stored, err := scanGitHubApp(s.app.QueryRowContext(ctx, `
		INSERT INTO org_github_apps (
			org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref,
			owner_type, registered_by_user_id, active, bot_user_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+pgGitHubAppColumns,
		app.OrgID, app.AppID, app.Slug, app.ClientID,
		app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef,
		app.NormalizedOwnerType(), nullString(app.RegisteredByUserID), app.Active,
		nullBotUserID(app.BotUserID),
	))
	var pgErr *pgconn.PgError
	if err != nil && errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.OrgGitHubApp{}, &db.ErrGitHubAppExists{OrgID: app.OrgID}
	}
	if err != nil {
		return domain.OrgGitHubApp{}, fmt.Errorf("insert org_github_apps: %w", err)
	}
	// A plain INSERT ... RETURNING (no ON CONFLICT) either fails with a real
	// error, handled above, or succeeds with exactly one row — stored is never
	// nil here, unlike the UPDATE ... RETURNING in SetActive.
	return *stored, nil
}

// nullBotUserID maps a zero (unknown) bot user id to a SQL NULL so the column
// distinguishes "never fetched" from a real id; any non-zero id is written
// verbatim. On read both NULL and a stored 0 scan back to 0, but writing NULL
// keeps the column honest.
func nullBotUserID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *gitHubAppsStore) SetActive(ctx context.Context, orgID string, active bool) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	// App pool: the org_github_apps_update RLS policy gates this by org admin,
	// which is exactly the claims context the cutover handler runs under.
	app, err := scanGitHubApp(s.app.QueryRowContext(ctx, `
		UPDATE org_github_apps SET active = $2 WHERE org_id = $1
		RETURNING `+pgGitHubAppColumns, orgID, active))
	if err != nil {
		return nil, wrapAppPoolPermErr(fmt.Errorf("set org_github_apps active: %w", err), "github_apps.SetActive")
	}
	return app, nil
}

func (s *gitHubAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	if !isValidUUID(orgID) {
		return nil
	}
	// Registration row on the app pool (org_github_apps_delete RLS gates by org
	// admin). Installations on the admin pool: tf_app is denied every write to
	// org_github_app_installations by RLS, so the mirror is maintained
	// exclusively by system code. The two are not one transaction (the admin
	// half autonomous-commits outside any surrounding tx, the same shape
	// UpsertInstallation / MarkInstallationRemoved use); a lingering
	// installation row after the registration row is gone is a harmless orphan
	// (the resolver short-circuits on the absent App before reading it).
	if _, err := s.admin.ExecContext(ctx, `
		DELETE FROM org_github_app_installations WHERE org_id = $1
	`, orgID); err != nil {
		return fmt.Errorf("delete org_github_app_installations: %w", err)
	}
	if _, err := s.app.ExecContext(ctx, `
		DELETE FROM org_github_apps WHERE org_id = $1
	`, orgID); err != nil {
		return wrapAppPoolPermErr(err, "github_apps.DeleteForOrg")
	}
	return nil
}

func (s *gitHubAppsStore) ListInstallationsForOrg(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	insts, err := listInstallations(ctx, s.app, orgID)
	return insts, wrapAppPoolPermErr(err, "github_apps.ListInstallationsForOrg")
}

func (s *gitHubAppsStore) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return listInstallations(ctx, s.admin, orgID)
}

// InstallationOwnerSystem answers the bind ceremony's uniqueness question:
// does any org already hold this installation on this host? Admin pool, because
// the question spans orgs and no caller's claims could authorize it.
//
// LIMIT 1 is not an arbitrary narrowing. Policy is that a live installation
// belongs to at most one org, and the schema does not enforce it — there is no
// UNIQUE (github_host, installation_id) index, so the caller's lock is the only
// thing holding the invariant. Taking the first row is what makes the refusal
// fire on a set a bug has already made ambiguous, rather than erroring on it.
func (s *gitHubAppsStore) InstallationOwnerSystem(ctx context.Context, githubHost, installationID string) (string, error) {
	var orgID string
	err := s.admin.QueryRowContext(ctx, `
		SELECT org_id
		  FROM org_github_app_installations
		 WHERE github_host = $1 AND installation_id = $2 AND removed_at IS NULL
		 LIMIT 1
	`, db.EffectiveGitHubHost(githubHost), installationID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read installation owner: %w", err)
	}
	return orgID, nil
}

// pgGitHubAppInstallationColumns is the canonical projection of an
// org_github_app_installations row, in the order scanGitHubAppInstallation
// reads them — everything but removed_at, which the domain type omits (see
// MarkInstallationRemoved). listInstallations SELECTs it and
// UpsertInstallation / SetInstallationSuspension / MarkInstallationRemoved
// RETURN it, so the write shape cannot drift from the read shape.
const pgGitHubAppInstallationColumns = `installation_id, org_id, account_type, account_id, account_login,
	       github_host, installed_at, suspended_at, suspended_by,
	       repository_selection`

func scanGitHubAppInstallation(row interface{ Scan(...any) error }) (domain.OrgGitHubAppInstallation, error) {
	var (
		inst        domain.OrgGitHubAppInstallation
		accountID   sql.NullString
		suspendedAt sql.NullTime
		suspendedBy sql.NullString
		selection   sql.NullString
	)
	if err := row.Scan(
		&inst.InstallationID, &inst.OrgID, &inst.AccountType,
		&accountID, &inst.AccountLogin, &inst.GitHubHost, &inst.InstalledAt,
		&suspendedAt, &suspendedBy, &selection,
	); err != nil {
		return domain.OrgGitHubAppInstallation{}, err
	}
	inst.RepositorySelection = selection.String // NULL → "" (grant width not yet learned)
	inst.AccountID = accountID.String           // NULL → "" (account id not yet captured)
	inst.SuspendedAt = suspendedAt.Time
	inst.SuspendedBy = suspendedBy.String
	return inst, nil
}

// listInstallations reads the org's active installations on the given
// pool. ListInstallationsForOrg passes the app pool (claims-checked
// request path); ListInstallationsForOrgSystem passes the admin pool
// (claims-free resolver / poller path). The query is identical — only
// the RLS identity context differs.
func listInstallations(ctx context.Context, q queryer, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	out := make([]domain.OrgGitHubAppInstallation, 0)
	if !isValidUUID(orgID) {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT `+pgGitHubAppInstallationColumns+`
		  FROM org_github_app_installations
		 WHERE org_id = $1 AND removed_at IS NULL
		 ORDER BY account_login
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org_github_app_installations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		inst, err := scanGitHubAppInstallation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan org_github_app_installations: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// UpsertInstallation writes on the admin pool — the
// org_github_app_installations RLS policies deny every write to tf_app, so
// the mirror is maintained exclusively by system code (webhook handler +
// backfill). installed_at is set only on insert (COALESCE to now() when the
// caller passes a zero time) and left untouched on conflict so the original
// install time survives a re-observe; removed_at is cleared so a reinstall
// revives the row.
//
// The two account fields update on opposite rules, which is the point of
// having both. account_login is overwritten every time: it is what GitHub
// currently calls the account, so a rename must reach the mirror or the UI
// renders a name that no longer exists. account_id is COALESCEd, so a writer
// that doesn't know it (an older payload) fills nothing in and erases
// nothing — the backfill is opportunistic, and an id once learned is never
// unlearned by a subsequent write that happens to omit it.
//
// The suspension columns take the login's rule rather than the id's. Both
// callers see GitHub's whole answer for the installation — the reconcile lists
// it, the webhook is told about it — so a zero SuspendedAt is an assertion that
// the installation is not suspended, not an omission to preserve around. That
// is what makes the re-install case correct: the row a `created` delivery
// revives comes back unsuspended in the same statement that clears removed_at,
// so a re-install can never inherit the suspension of the installation it
// replaced.
//
// github_host is normalized on the way in and overwritten like the login: both
// writers derive it from the org's github_base_url, so an installation the
// current host reports is on the current host by construction. The normalize
// is what keeps the NOT NULL column out of the empty string — a struct built
// without a host is one whose org configured no base URL, which is github.com.
//
// repository_selection takes the account id's fill-in-only rule rather than the
// login's. Unlike the host and the suspension state, a writer here genuinely can
// not know it: the /app/installations listing reports it on every pass, but a
// caller that built the struct from something narrower did not look, and
// treating that silence as "unknown" would erase a width already learned. NULL
// therefore means "not established", never "no selection".
func (s *gitHubAppsStore) UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) (domain.OrgGitHubAppInstallation, error) {
	var installedAt sql.NullTime
	if !inst.InstalledAt.IsZero() {
		installedAt = sql.NullTime{Time: inst.InstalledAt, Valid: true}
	}
	selection, err := domain.NormalizeRepositorySelection(inst.RepositorySelection)
	if err != nil {
		return domain.OrgGitHubAppInstallation{}, fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	stored, err := scanGitHubAppInstallation(s.admin.QueryRowContext(ctx, `
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_id, account_login, github_host,
			 installed_at, removed_at, suspended_at, suspended_by, repository_selection)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), NULL, $8, $9, $10)
		ON CONFLICT (org_id, installation_id) DO UPDATE SET
			account_type  = EXCLUDED.account_type,
			account_login = EXCLUDED.account_login,
			account_id    = COALESCE(EXCLUDED.account_id, org_github_app_installations.account_id),
			github_host   = EXCLUDED.github_host,
			removed_at    = NULL,
			suspended_at  = EXCLUDED.suspended_at,
			suspended_by  = EXCLUDED.suspended_by,
			repository_selection = COALESCE(EXCLUDED.repository_selection,
			                                org_github_app_installations.repository_selection)
		RETURNING `+pgGitHubAppInstallationColumns,
		inst.InstallationID, inst.OrgID, inst.AccountType, nullString(inst.AccountID), inst.AccountLogin,
		db.EffectiveGitHubHost(inst.GitHubHost), installedAt,
		nullTime(inst.SuspendedAt), nullString(inst.SuspendedBy), nullString(selection)))
	if err != nil {
		return domain.OrgGitHubAppInstallation{}, fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	return stored, nil
}

// SetInstallationSuspension stamps or clears the suspension columns on the
// admin pool, like every other write to this table. Zero suspendedAt clears
// both (an unsuspend leaves no residue); a non-zero one writes both, with an
// unnamed suspender landing as NULL rather than "".
func (s *gitHubAppsStore) SetInstallationSuspension(ctx context.Context, orgID, installationID string, suspendedAt time.Time, suspendedBy string) (*domain.OrgGitHubAppInstallation, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	by := nullString(suspendedBy)
	if suspendedAt.IsZero() {
		by = nil // no suspender to record on a cleared row
	}
	stored, err := scanGitHubAppInstallation(s.admin.QueryRowContext(ctx, `
		UPDATE org_github_app_installations
		   SET suspended_at = $3, suspended_by = $4
		 WHERE org_id = $1 AND installation_id = $2
		RETURNING `+pgGitHubAppInstallationColumns,
		orgID, installationID, nullTime(suspendedAt), by))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("set org_github_app_installations suspension: %w", err)
	}
	return &stored, nil
}

// nullTime maps a zero time to a SQL NULL so an absent timestamp is stored as
// absent rather than as the zero instant, which reads back as a real (and very
// old) time.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// MarkInstallationRemoved soft-removes the installation and drops its grant
// mirror in one transaction. The mirror is a cache of what the installation
// could reach; an uninstalled installation reaches nothing, so keeping the rows
// would leave the org page able to report reach the App no longer has. The
// installation row itself survives as history, which is the asymmetry the
// mirror-as-cache / row-as-registry split exists for.
//
// Written here rather than left to the reconcile so a `deleted` webhook clears
// the grant at the moment it arrives instead of a poll interval later; the
// reconcile's own soft-remove arm gets the same clear for free. This is the one
// place outside ReachableReposStore that writes reachable_repositories, and it
// does so because the two writes are one fact.
func (s *gitHubAppsStore) MarkInstallationRemoved(ctx context.Context, orgID, installationID string) (*domain.OrgGitHubAppInstallation, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	var removed *domain.OrgGitHubAppInstallation
	err := inTx(ctx, s.admin, func(tx queryer) error {
		stored, err := scanGitHubAppInstallation(tx.QueryRowContext(ctx, `
			UPDATE org_github_app_installations
			   SET removed_at = now()
			 WHERE org_id = $1 AND installation_id = $2 AND removed_at IS NULL
			RETURNING `+pgGitHubAppInstallationColumns,
			orgID, installationID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already removed or never seen — no-op, nothing to cascade
		}
		if err != nil {
			return err
		}
		removed = &stored
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = $1 AND installation_id = $2
		`, orgID, installationID); err != nil {
			return err
		}
		// And the scope marker: an installation that reaches nothing must not keep
		// vouching that its reach was ever established. Narrowed to the App
		// classes because scope holds a host for the PAT tier, and this argument
		// is an installation id.
		classes, classArgs := appTierClassArgs(2)
		_, err = tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes
			 WHERE org_id = $1 AND scope = $2 AND credential_class IN (`+classes+`)
		`, append([]any{orgID, installationID}, classArgs...)...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("mark org_github_app_installations removed: %w", err)
	}
	return removed, nil
}

// activeInstallationIDs reads the org's live installation IDs on the admin
// pool — the system-context counterpart of ListInstallationsForOrg, used by
// the backfill reconcile (which has no JWT claims).
func (s *gitHubAppsStore) activeInstallationIDs(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT installation_id FROM org_github_app_installations
		 WHERE org_id = $1 AND removed_at IS NULL
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("read active installations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *gitHubAppsStore) BackfillInstallationsFromAPI(ctx context.Context, orgID string) error {
	if !isValidUUID(orgID) {
		return nil
	}
	var appID, pemRef, baseURL string
	// No active gate: a staged App (active=false, mid PAT→App switch) must
	// still discover its installations for the cutover preflight + install
	// verification. The poller gates its own backfill on active separately.
	err := s.admin.QueryRowContext(ctx, `
		SELECT a.app_id, a.pem_ref, COALESCE(es.base_url, '')
		  FROM org_github_apps a
		  LEFT JOIN org_event_sources es ON es.org_id = a.org_id AND es.kind = 'github'
		 WHERE a.org_id = $1
	`, orgID).Scan(&appID, &pemRef, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no registered App for this org — nothing to backfill
	}
	if err != nil {
		return fmt.Errorf("read org_github_apps for backfill: %w", err)
	}

	insts, err := db.DiscoverAppInstallations(ctx, s.secrets, orgID, appID, pemRef, baseURL)
	if err != nil {
		return err
	}
	active, err := s.activeInstallationIDs(ctx, orgID)
	if err != nil {
		return err
	}
	return db.ReconcileInstallations(insts, active,
		func(i domain.OrgGitHubAppInstallation) error { _, err := s.UpsertInstallation(ctx, i); return err },
		func(id string) error { _, err := s.MarkInstallationRemoved(ctx, orgID, id); return err },
	)
}

// RefreshManagedInstallations reconciles a workspace that rides the DEPLOYMENT
// App. See the GitHubAppsStore interface doc for the invariant it holds: the
// managed reconcile refreshes bound rows and creates none.
//
// The class and the org's GitHub base URL come off one read, as the sibling's
// registration + base URL do — same admin pool, same LEFT JOIN onto
// org_event_sources, so a settings row with no per-source override resolves to
// the public host rather than dropping the org.
func (s *gitHubAppsStore) RefreshManagedInstallations(ctx context.Context, orgID string, deployment githubapp.DeploymentApp) error {
	// An id that is not a uuid names no org, and an org this method cannot
	// identify is one it must not reconcile. Refused rather than skipped —
	// unlike the discovering sibling, which is invoked speculatively per poll
	// cycle, every caller of this one asked for a specific workspace.
	//
	// The refusal is what matters, not this guard: org_id is a uuid column, so
	// without it the settings read below fails on the cast instead, which is the
	// same outcome wearing a driver's error message. The SQLite twin needs no
	// equivalent for that reason — its org_id is TEXT, so an id naming nothing
	// simply matches no settings row and takes the refusal below. Both dialects
	// answer an unknown org the same way, which the conformance suite pins.
	if !isValidUUID(orgID) {
		return fmt.Errorf("refresh managed installations: %q is not an org id", orgID)
	}
	var class, baseURL string
	err := s.admin.QueryRowContext(ctx, `
		SELECT st.github_credential_class, COALESCE(es.base_url, '')
		  FROM org_settings st
		  LEFT JOIN org_event_sources es ON es.org_id = st.org_id AND es.kind = 'github'
		 WHERE st.org_id = $1
	`, orgID).Scan(&class, &baseURL)
	if errors.Is(err, sql.ErrNoRows) {
		// No settings row is no credential class, which is not the managed one.
		return fmt.Errorf("refresh managed installations: org %s has no settings row", orgID)
	}
	if err != nil {
		return fmt.Errorf("read org_settings for managed refresh: %w", err)
	}
	if domain.GitHubCredentialClass(class) != domain.GitHubCredentialClassManagedApp {
		return fmt.Errorf("refresh managed installations: org %s is on credential class %q", orgID, class)
	}

	active, err := s.activeInstallationIDs(ctx, orgID)
	if err != nil {
		return err
	}
	// Nothing bound is the ordinary state of a workspace that has not completed
	// a bind, and there is nothing a listing could tell us about it: no row to
	// refresh, and creating one is the thing this method may never do. Answered
	// without spending the API call.
	if len(active) == 0 {
		return nil
	}

	insts, err := db.RefreshBoundInstallations(ctx, deployment, orgID, baseURL, active)
	if err != nil {
		return err
	}
	return db.ReconcileInstallations(insts, active,
		func(i domain.OrgGitHubAppInstallation) error { _, err := s.UpsertInstallation(ctx, i); return err },
		func(id string) error { _, err := s.MarkInstallationRemoved(ctx, orgID, id); return err },
	)
}

// RefreshAllManagedInstallations is the deployment-wide managed refresh. See
// the GitHubAppsStore interface doc. Every read and write is on the admin
// pool: the read spans orgs, which no request's claims could authorize, and
// the installation mirror is admin-pool-only by RLS regardless.
func (s *gitHubAppsStore) RefreshAllManagedInstallations(ctx context.Context, deployment githubapp.DeploymentApp) error {
	sets, err := s.managedInstallationSets(ctx)
	if err != nil {
		return err
	}
	// No managed workspace holds a bound installation: no row to refresh, and
	// nothing a listing could add without discovering. Answered before the
	// deployment App is even consulted, so a deployment whose orgs all bring
	// their own App never minds that it has none.
	if len(sets) == 0 {
		return nil
	}
	return db.RefreshManagedInstallationSets(ctx, deployment, sets,
		func(i domain.OrgGitHubAppInstallation) error { _, err := s.UpsertInstallation(ctx, i); return err },
		func(orgID, id string) error { _, err := s.MarkInstallationRemoved(ctx, orgID, id); return err },
	)
}

// managedInstallationSets reads every managed workspace's live bound
// installation ids alongside the GitHub base URL the org lists against — the
// rows the cadence pass may refresh, grouped by org. An org with nothing bound
// contributes no set: there is no row for the pass to write to, which is the
// invariant stated as a query.
func (s *gitHubAppsStore) managedInstallationSets(ctx context.Context) ([]db.ManagedInstallationSet, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT st.org_id, COALESCE(es.base_url, ''), i.installation_id
		  FROM org_settings st
		  JOIN org_github_app_installations i ON i.org_id = st.org_id AND i.removed_at IS NULL
		  LEFT JOIN org_event_sources es ON es.org_id = st.org_id AND es.kind = 'github'
		 WHERE st.github_credential_class = $1
		 ORDER BY st.org_id, i.installation_id
	`, string(domain.GitHubCredentialClassManagedApp))
	if err != nil {
		return nil, fmt.Errorf("read managed installation sets: %w", err)
	}
	defer rows.Close()
	return db.ScanManagedInstallationSets(rows)
}
