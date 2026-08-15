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
		return nil, fmt.Errorf("get org_github_apps: %w", err)
	}
	if registeredAt.Valid {
		a.RegisteredAt = registeredAt.Time
	}
	a.RegisteredByUserID = regBy.String
	a.BotUserID = botUserID.Int64 // NULL → 0 (bot user id unknown)
	return &a, nil
}

const selectGitHubAppCols = `
	SELECT org_id, app_id, slug, client_id,
	       client_secret_ref, pem_ref, webhook_secret_ref,
	       owner_type, registered_at, registered_by_user_id, active, bot_user_id
	  FROM org_github_apps
	 WHERE org_id = $1`

func (s *gitHubAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	app, err := scanGitHubApp(s.app.QueryRowContext(ctx, selectGitHubAppCols, orgID))
	return app, wrapAppPoolPermErr(err, "github_apps.GetForOrg")
}

func (s *gitHubAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	return scanGitHubApp(s.admin.QueryRowContext(ctx, selectGitHubAppCols, orgID))
}

func (s *gitHubAppsStore) CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) error {
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO org_github_apps (
			org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref,
			owner_type, registered_by_user_id, active, bot_user_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`,
		app.OrgID, app.AppID, app.Slug, app.ClientID,
		app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef,
		app.NormalizedOwnerType(), nullString(app.RegisteredByUserID), app.Active,
		nullBotUserID(app.BotUserID),
	)
	var pgErr *pgconn.PgError
	if err != nil && errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return &db.ErrGitHubAppExists{OrgID: app.OrgID}
	}
	if err != nil {
		return fmt.Errorf("insert org_github_apps: %w", err)
	}
	return nil
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

func (s *gitHubAppsStore) SetActive(ctx context.Context, orgID string, active bool) error {
	if !isValidUUID(orgID) {
		return nil
	}
	// App pool: the org_github_apps_update RLS policy gates this by org admin,
	// which is exactly the claims context the cutover handler runs under.
	_, err := s.app.ExecContext(ctx, `
		UPDATE org_github_apps SET active = $2 WHERE org_id = $1
	`, orgID, active)
	return wrapAppPoolPermErr(err, "github_apps.SetActive")
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
		SELECT installation_id, org_id, account_type, account_id, account_login,
		       installed_at, suspended_at, suspended_by
		  FROM org_github_app_installations
		 WHERE org_id = $1 AND removed_at IS NULL
		 ORDER BY account_login
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org_github_app_installations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			inst        domain.OrgGitHubAppInstallation
			accountID   sql.NullString
			suspendedAt sql.NullTime
			suspendedBy sql.NullString
		)
		if err := rows.Scan(
			&inst.InstallationID, &inst.OrgID, &inst.AccountType,
			&accountID, &inst.AccountLogin, &inst.InstalledAt,
			&suspendedAt, &suspendedBy,
		); err != nil {
			return nil, fmt.Errorf("scan org_github_app_installations: %w", err)
		}
		inst.AccountID = accountID.String // NULL → "" (account id not yet captured)
		inst.SuspendedAt = suspendedAt.Time
		inst.SuspendedBy = suspendedBy.String
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
func (s *gitHubAppsStore) UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) error {
	var installedAt sql.NullTime
	if !inst.InstalledAt.IsZero() {
		installedAt = sql.NullTime{Time: inst.InstalledAt, Valid: true}
	}
	_, err := s.admin.ExecContext(ctx, `
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_id, account_login, installed_at,
			 removed_at, suspended_at, suspended_by)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()), NULL, $7, $8)
		ON CONFLICT (org_id, installation_id) DO UPDATE SET
			account_type  = EXCLUDED.account_type,
			account_login = EXCLUDED.account_login,
			account_id    = COALESCE(EXCLUDED.account_id, org_github_app_installations.account_id),
			removed_at    = NULL,
			suspended_at  = EXCLUDED.suspended_at,
			suspended_by  = EXCLUDED.suspended_by
	`, inst.InstallationID, inst.OrgID, inst.AccountType, nullString(inst.AccountID), inst.AccountLogin, installedAt,
		nullTime(inst.SuspendedAt), nullString(inst.SuspendedBy))
	if err != nil {
		return fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	return nil
}

// SetInstallationSuspension stamps or clears the suspension columns on the
// admin pool, like every other write to this table. Zero suspendedAt clears
// both (an unsuspend leaves no residue); a non-zero one writes both, with an
// unnamed suspender landing as NULL rather than "".
func (s *gitHubAppsStore) SetInstallationSuspension(ctx context.Context, orgID, installationID string, suspendedAt time.Time, suspendedBy string) error {
	if !isValidUUID(orgID) {
		return nil
	}
	by := nullString(suspendedBy)
	if suspendedAt.IsZero() {
		by = nil // no suspender to record on a cleared row
	}
	_, err := s.admin.ExecContext(ctx, `
		UPDATE org_github_app_installations
		   SET suspended_at = $3, suspended_by = $4
		 WHERE org_id = $1 AND installation_id = $2
	`, orgID, installationID, nullTime(suspendedAt), by)
	if err != nil {
		return fmt.Errorf("set org_github_app_installations suspension: %w", err)
	}
	return nil
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

func (s *gitHubAppsStore) MarkInstallationRemoved(ctx context.Context, orgID, installationID string) error {
	_, err := s.admin.ExecContext(ctx, `
		UPDATE org_github_app_installations
		   SET removed_at = now()
		 WHERE org_id = $1 AND installation_id = $2 AND removed_at IS NULL
	`, orgID, installationID)
	if err != nil {
		return fmt.Errorf("mark org_github_app_installations removed: %w", err)
	}
	return nil
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
		SELECT a.app_id, a.pem_ref, COALESCE(s.github_base_url, '')
		  FROM org_github_apps a
		  LEFT JOIN org_settings s ON s.org_id = a.org_id
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
		func(i domain.OrgGitHubAppInstallation) error { return s.UpsertInstallation(ctx, i) },
		func(id string) error { return s.MarkInstallationRemoved(ctx, orgID, id) },
	)
}
