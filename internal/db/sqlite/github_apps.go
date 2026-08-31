package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// gitHubAppsStore is the SQLite (local-mode) impl. One connection — no RLS,
// no pool split — so the admin/app distinction the Postgres impl draws
// collapses here; GetForOrgSystem == GetForOrg and installation writes go to
// the same conn. secrets reads the App PEM for backfill (the keychain has no
// claims, so GetSystem == Get locally too).
type gitHubAppsStore struct {
	q       queryer
	secrets db.SecretStore
}

func newGitHubAppsStore(q queryer, secrets db.SecretStore) db.GitHubAppsStore {
	return &gitHubAppsStore{q: q, secrets: secrets}
}

var _ db.GitHubAppsStore = (*gitHubAppsStore)(nil)

// sqliteGitHubAppColumns is the canonical projection of an org_github_apps
// row, in the order scanSQLiteGitHubApp reads them. GetForOrg SELECTs it and
// CreateForOrg / SetActive RETURN it, so the write shape cannot drift from
// the read shape.
const sqliteGitHubAppColumns = `org_id, app_id, slug, client_id,
	       client_secret_ref, pem_ref, webhook_secret_ref,
	       owner_type, registered_at, registered_by_user_id, active, bot_user_id`

func scanSQLiteGitHubApp(scan func(...any) error) (domain.OrgGitHubApp, error) {
	var (
		a         domain.OrgGitHubApp
		regBy     sql.NullString
		botUserID sql.NullInt64
	)
	if err := scan(
		&a.OrgID, &a.AppID, &a.Slug, &a.ClientID,
		&a.ClientSecretRef, &a.PEMRef, &a.WebhookSecretRef,
		&a.OwnerType, &a.RegisteredAt, &regBy, &a.Active, &botUserID,
	); err != nil {
		return a, err
	}
	a.RegisteredByUserID = regBy.String
	a.BotUserID = botUserID.Int64 // NULL → 0 (bot user id unknown)
	return a, nil
}

func (s *gitHubAppsStore) GetForOrg(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	a, err := scanSQLiteGitHubApp(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteGitHubAppColumns+`
		  FROM org_github_apps
		 WHERE org_id = ?
	`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get org_github_apps: %w", err)
	}
	return &a, nil
}

// GetForOrgSystem is identical to GetForOrg in local mode — single conn,
// no RLS, no claims — but exists so system callers (webhook handler,
// backfill) use the same interface shape they would in multi mode.
func (s *gitHubAppsStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.OrgGitHubApp, error) {
	return s.GetForOrg(ctx, orgID)
}

func (s *gitHubAppsStore) CreateForOrg(ctx context.Context, app domain.OrgGitHubApp) (domain.OrgGitHubApp, error) {
	stored, err := scanSQLiteGitHubApp(s.q.QueryRowContext(ctx, `
		INSERT INTO org_github_apps (
			org_id, app_id, slug, client_id,
			client_secret_ref, pem_ref, webhook_secret_ref,
			owner_type, registered_by_user_id, active, bot_user_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING `+sqliteGitHubAppColumns,
		app.OrgID, app.AppID, app.Slug, app.ClientID,
		app.ClientSecretRef, app.PEMRef, app.WebhookSecretRef,
		app.NormalizedOwnerType(), nullStringValue(app.RegisteredByUserID), app.Active,
		nullBotUserID(app.BotUserID),
	).Scan)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return domain.OrgGitHubApp{}, &db.ErrGitHubAppExists{OrgID: app.OrgID}
	}
	if err != nil {
		return domain.OrgGitHubApp{}, fmt.Errorf("insert org_github_apps: %w", err)
	}
	return stored, nil
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
	a, err := scanSQLiteGitHubApp(s.q.QueryRowContext(ctx, `
		UPDATE org_github_apps SET active = ? WHERE org_id = ?
		RETURNING `+sqliteGitHubAppColumns,
		active, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("set org_github_apps active: %w", err)
	}
	return &a, nil
}

func (s *gitHubAppsStore) DeleteForOrg(ctx context.Context, orgID string) error {
	// Installations first, then the registration row. SQLite is one
	// connection (inside the caller's tx when run through TxStores), so both
	// land atomically — no cross-pool split like Postgres.
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM org_github_app_installations WHERE org_id = ?
	`, orgID); err != nil {
		return fmt.Errorf("delete org_github_app_installations: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM org_github_apps WHERE org_id = ?
	`, orgID); err != nil {
		return fmt.Errorf("delete org_github_apps: %w", err)
	}
	return nil
}

// sqliteGitHubAppInstallationColumns is the canonical projection of an
// org_github_app_installations row, in the order scanSQLiteGitHubAppInstallation
// reads them — everything but removed_at, which the domain type omits (see
// MarkInstallationRemoved). ListInstallationsForOrg SELECTs it and
// UpsertInstallation / SetInstallationSuspension / MarkInstallationRemoved
// RETURN it, so the write shape cannot drift from the read shape.
const sqliteGitHubAppInstallationColumns = `installation_id, org_id, account_type, account_id, account_login,
	       github_host, installed_at, suspended_at, suspended_by,
	       repository_selection`

func scanSQLiteGitHubAppInstallation(scan func(...any) error) (domain.OrgGitHubAppInstallation, error) {
	var (
		inst        domain.OrgGitHubAppInstallation
		accountID   sql.NullString
		suspendedAt sql.NullTime
		suspendedBy sql.NullString
		selection   sql.NullString
	)
	if err := scan(
		&inst.InstallationID, &inst.OrgID, &inst.AccountType,
		&accountID, &inst.AccountLogin, &inst.GitHubHost, &inst.InstalledAt,
		&suspendedAt, &suspendedBy, &selection,
	); err != nil {
		return domain.OrgGitHubAppInstallation{}, err
	}
	inst.AccountID = accountID.String // NULL → "" (account id not yet captured)
	inst.SuspendedAt = suspendedAt.Time
	inst.SuspendedBy = suspendedBy.String
	inst.RepositorySelection = selection.String // NULL → "" (grant width not yet learned)
	return inst, nil
}

func (s *gitHubAppsStore) ListInstallationsForOrg(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteGitHubAppInstallationColumns+`
		  FROM org_github_app_installations
		 WHERE org_id = ? AND removed_at IS NULL
		 ORDER BY account_login
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org_github_app_installations: %w", err)
	}
	defer rows.Close()

	out := make([]domain.OrgGitHubAppInstallation, 0)
	for rows.Next() {
		inst, err := scanSQLiteGitHubAppInstallation(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan org_github_app_installations: %w", err)
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// ListInstallationsForOrgSystem is identical to ListInstallationsForOrg in
// local mode — single conn, no RLS, no claims — but exists so the
// credential resolver uses the same interface shape it would in multi mode.
func (s *gitHubAppsStore) ListInstallationsForOrgSystem(ctx context.Context, orgID string) ([]domain.OrgGitHubAppInstallation, error) {
	return s.ListInstallationsForOrg(ctx, orgID)
}

// UpsertInstallation mirrors one installation, idempotent on installation_id.
// installed_at is set only on insert (defaulting to CURRENT_TIMESTAMP for a
// zero InstalledAt) and preserved on conflict; removed_at is cleared so a
// reinstall revives the row. account_login is overwritten (a rename must
// reach the mirror) while account_id is only ever filled in, never blanked —
// see the Postgres impl for why. The suspension columns follow the login's
// rule, not the id's: both callers see GitHub's whole answer, so they land
// verbatim, and a re-install clears an inherited suspension in the same
// statement it clears removed_at. github_host takes the login's rule too, and
// is normalized on the way in so the NOT NULL column can never hold an empty
// string. repository_selection takes the account id's rule — see the Postgres
// impl.
func (s *gitHubAppsStore) UpsertInstallation(ctx context.Context, inst domain.OrgGitHubAppInstallation) (domain.OrgGitHubAppInstallation, error) {
	var installedAt sql.NullTime
	if !inst.InstalledAt.IsZero() {
		installedAt = sql.NullTime{Time: inst.InstalledAt, Valid: true}
	}
	selection, err := domain.NormalizeRepositorySelection(inst.RepositorySelection)
	if err != nil {
		return domain.OrgGitHubAppInstallation{}, fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	stored, err := scanSQLiteGitHubAppInstallation(s.q.QueryRowContext(ctx, `
		INSERT INTO org_github_app_installations
			(installation_id, org_id, account_type, account_id, account_login, github_host,
			 installed_at, removed_at, suspended_at, suspended_by, repository_selection)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), NULL, ?, ?, ?)
		ON CONFLICT(org_id, installation_id) DO UPDATE SET
			account_type  = excluded.account_type,
			account_login = excluded.account_login,
			account_id    = COALESCE(excluded.account_id, org_github_app_installations.account_id),
			github_host   = excluded.github_host,
			removed_at    = NULL,
			suspended_at  = excluded.suspended_at,
			suspended_by  = excluded.suspended_by,
			repository_selection = COALESCE(excluded.repository_selection,
			                                org_github_app_installations.repository_selection)
		RETURNING `+sqliteGitHubAppInstallationColumns,
		inst.InstallationID, inst.OrgID, inst.AccountType, nullStringValue(inst.AccountID), inst.AccountLogin,
		db.EffectiveGitHubHost(inst.GitHubHost), installedAt,
		nullTimeValue(inst.SuspendedAt), nullStringValue(inst.SuspendedBy), nullStringValue(selection)).Scan)
	if err != nil {
		return domain.OrgGitHubAppInstallation{}, fmt.Errorf("upsert org_github_app_installations: %w", err)
	}
	return stored, nil
}

// SetInstallationSuspension stamps or clears the suspension columns. Zero
// suspendedAt clears both (an unsuspend leaves no residue); a non-zero one
// writes both, with an unnamed suspender landing as NULL rather than "".
func (s *gitHubAppsStore) SetInstallationSuspension(ctx context.Context, orgID, installationID string, suspendedAt time.Time, suspendedBy string) (*domain.OrgGitHubAppInstallation, error) {
	by := nullStringValue(suspendedBy)
	if suspendedAt.IsZero() {
		by = nil // no suspender to record on a cleared row
	}
	stored, err := scanSQLiteGitHubAppInstallation(s.q.QueryRowContext(ctx, `
		UPDATE org_github_app_installations
		   SET suspended_at = ?, suspended_by = ?
		 WHERE org_id = ? AND installation_id = ?
		RETURNING `+sqliteGitHubAppInstallationColumns,
		nullTimeValue(suspendedAt), by, orgID, installationID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("set org_github_app_installations suspension: %w", err)
	}
	return &stored, nil
}

// nullTimeValue maps a zero time to a SQL NULL so an absent timestamp is
// stored as absent rather than as the zero instant, which reads back as a real
// (and very old) time.
func nullTimeValue(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
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
	var removed *domain.OrgGitHubAppInstallation
	err := inTx(ctx, s.q, func(tx queryer) error {
		stored, err := scanSQLiteGitHubAppInstallation(tx.QueryRowContext(ctx, `
			UPDATE org_github_app_installations
			   SET removed_at = CURRENT_TIMESTAMP
			 WHERE org_id = ? AND installation_id = ? AND removed_at IS NULL
			RETURNING `+sqliteGitHubAppInstallationColumns,
			orgID, installationID).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already removed or never seen — no-op, nothing to cascade
		}
		if err != nil {
			return err
		}
		removed = &stored
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = ? AND installation_id = ?
		`, orgID, installationID); err != nil {
			return err
		}
		// And the scope marker: an installation that reaches nothing must not keep
		// vouching that its reach was ever established. Narrowed to the App
		// classes because scope holds a host for the PAT tier, and this argument
		// is an installation id.
		classes, classArgs := appTierClassArgs()
		_, err = tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes
			 WHERE org_id = ? AND scope = ? AND credential_class IN (`+classes+`)
		`, append([]any{orgID, installationID}, classArgs...)...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("mark org_github_app_installations removed: %w", err)
	}
	return removed, nil
}

func (s *gitHubAppsStore) activeInstallationIDs(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT installation_id FROM org_github_app_installations
		 WHERE org_id = ? AND removed_at IS NULL
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
	var appID, pemRef, baseURL string
	// No active gate: a staged App (active=0, mid PAT→App switch) must still
	// discover its installations for the cutover preflight + install
	// verification. The poller gates its own backfill on active separately.
	err := s.q.QueryRowContext(ctx, `
		SELECT a.app_id, a.pem_ref, COALESCE(es.base_url, '')
		  FROM org_github_apps a
		  LEFT JOIN org_event_sources es ON es.org_id = a.org_id AND es.kind = 'github'
		 WHERE a.org_id = ?
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
