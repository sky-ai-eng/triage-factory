package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// deploymentCommitSignerStore is the Postgres impl of
// db.DeploymentCommitSignerStore. Admin pool only: every caller is a control
// pod.
type deploymentCommitSignerStore struct{ q queryer }

func newDeploymentCommitSignerStore(q queryer) db.DeploymentCommitSignerStore {
	return &deploymentCommitSignerStore{q: q}
}

var _ db.DeploymentCommitSignerStore = (*deploymentCommitSignerStore)(nil)

func (s *deploymentCommitSignerStore) GetSystem(ctx context.Context, host, publicKey string) (*domain.DeploymentCommitSigner, error) {
	row, err := db.ScanDeploymentCommitSigner(s.q.QueryRowContext(ctx,
		`SELECT `+db.DeploymentCommitSignerColumns+` FROM deployment_commit_signer WHERE host = $1 AND public_key = $2`,
		db.NormalizeGitHubHost(host), publicKey).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *deploymentCommitSignerStore) RegisterSystem(ctx context.Context, host, publicKey, login string) (domain.DeploymentCommitSigner, error) {
	host = db.NormalizeGitHubHost(host)
	if err := db.ValidateDeploymentSignerKey(host, publicKey, login); err != nil {
		return domain.DeploymentCommitSigner{}, err
	}
	// DO NOTHING returns no row for a key that already has one, and the row
	// that persisted is then the existing one, which the read below returns:
	// every control pod registers its key at boot, and the first one's row,
	// with whatever the checks have learned since, is the one that stands.
	row, err := db.ScanDeploymentCommitSigner(s.q.QueryRowContext(ctx, `
		INSERT INTO deployment_commit_signer (host, public_key, login)
		VALUES ($1, $2, $3)
		ON CONFLICT (host, public_key) DO NOTHING
		RETURNING `+db.DeploymentCommitSignerColumns,
		host, publicKey, login).Scan)
	if !errors.Is(err, sql.ErrNoRows) {
		return row, err
	}
	existing, err := s.GetSystem(ctx, host, publicKey)
	if err != nil {
		return domain.DeploymentCommitSigner{}, err
	}
	if existing == nil {
		return domain.DeploymentCommitSigner{}, db.ErrNoSuchDeploymentCommitSigner
	}
	return *existing, nil
}

func (s *deploymentCommitSignerStore) RecordKeyCheckSystem(ctx context.Context, before domain.DeploymentCommitSigner, check db.DeploymentSignerKeyCheck) (*domain.DeploymentCommitSigner, error) {
	status, reason := db.DeploymentKeyCheckTransition(before.Status, before.StatusReason, check.KeyFound)
	var userID, login any
	if check.GitHubUserID > 0 {
		userID, login = check.GitHubUserID, check.Login
	}
	// $3 NULL is an account the check could not find, which keeps what the
	// row holds. The WHERE is the compare half of a compare-and-set on the
	// status the check read, and refuses an account other than the one the
	// row already follows.
	row, err := db.ScanDeploymentCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE deployment_commit_signer SET
			github_user_id = COALESCE($3::bigint, github_user_id),
			login          = COALESCE($4::text, login),
			name           = CASE WHEN $3::bigint IS NULL THEN name ELSE $5 END,
			status_since   = CASE WHEN status = $6 THEN status_since ELSE now() END,
			status         = $6,
			status_reason  = $7,
			checked_at     = now()
		 WHERE host = $1 AND public_key = $2
		   AND status = $8 AND COALESCE(status_reason, '') = $9
		   AND ($3::bigint IS NULL OR github_user_id IS NULL OR github_user_id = $3::bigint)
		RETURNING `+db.DeploymentCommitSignerColumns,
		db.NormalizeGitHubHost(before.Host), before.PublicKey, userID, login, nullIfEmpty(check.Name),
		string(status), nullIfEmpty(reason), string(before.Status), before.StatusReason).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *deploymentCommitSignerStore) ActivateSystem(ctx context.Context, host, publicKey string, userID int64, login, name string) (domain.DeploymentCommitSigner, error) {
	if userID <= 0 || login == "" {
		return domain.DeploymentCommitSigner{}, db.ErrNoSuchDeploymentCommitSigner
	}
	row, err := db.ScanDeploymentCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE deployment_commit_signer SET
			github_user_id = $3,
			login          = $4,
			name           = $5,
			status_since   = CASE WHEN status = $6 THEN status_since ELSE now() END,
			status         = $6,
			status_reason  = NULL,
			checked_at     = now()
		 WHERE host = $1 AND public_key = $2 AND (github_user_id IS NULL OR github_user_id = $3)
		RETURNING `+db.DeploymentCommitSignerColumns,
		db.NormalizeGitHubHost(host), publicKey, userID, login, nullIfEmpty(name),
		string(domain.CommitSignerActive)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DeploymentCommitSigner{}, db.ErrNoSuchDeploymentCommitSigner
	}
	return row, err
}

func (s *deploymentCommitSignerStore) BreakSystem(ctx context.Context, host, publicKey, reason string) (*domain.DeploymentCommitSigner, error) {
	if err := db.ValidateBreakReason(reason); err != nil {
		return nil, err
	}
	row, err := db.ScanDeploymentCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE deployment_commit_signer SET
			status_since  = CASE WHEN status = $4 THEN status_since ELSE now() END,
			status        = $4,
			status_reason = $3
		 WHERE host = $1 AND public_key = $2
		   AND (status = $5 OR (status = $4 AND status_reason = $6 AND $3::text <> $7::text))
		RETURNING `+db.DeploymentCommitSignerColumns,
		db.NormalizeGitHubHost(host), publicKey, reason, string(domain.CommitSignerBroken), string(domain.CommitSignerActive),
		domain.CommitSignerReasonKeyMissing, domain.CommitVerificationUnknownKey).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *deploymentCommitSignerStore) NewestFailedVerificationSystem(ctx context.Context, host string, userID int64) (*domain.CommitSignature, error) {
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		SELECT `+db.CommitSignatureColumns+` FROM commit_signatures
		 WHERE state = $1 AND signer = $2 AND signer_host = $3 AND signer_user_id = $4
		   AND verification_reason IS NOT NULL AND verification_reason <> $5
		 ORDER BY signed_at DESC, id DESC
		 LIMIT 1`,
		string(domain.CommitSignatureSigned), string(domain.CommitSigningDeployment),
		db.NormalizeGitHubHost(host), userID, domain.CommitVerificationValid).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *deploymentCommitSignerStore) UsageSystem(ctx context.Context, since, until time.Time) (db.DeploymentSignerUsage, error) {
	// One statement, so both cuts count the same signatures: the hour set
	// leaves org_id NULL and the org set leaves the hour NULL.
	rows, err := s.q.QueryContext(ctx, `
		SELECT date_trunc('hour', signed_at, 'UTC'), org_id, count(*) FROM commit_signatures
		 WHERE state = $1 AND signer = $2
		   AND ($3::timestamptz IS NULL OR signed_at >= $3::timestamptz)
		   AND ($4::timestamptz IS NULL OR signed_at < $4::timestamptz)
		 GROUP BY GROUPING SETS ((date_trunc('hour', signed_at, 'UTC')), (org_id))`,
		string(domain.CommitSignatureSigned), string(domain.CommitSigningDeployment), nullTime(since), nullTime(until))
	if err != nil {
		return db.DeploymentSignerUsage{}, err
	}
	defer rows.Close()
	var out db.DeploymentSignerUsage
	for rows.Next() {
		var (
			hour  sql.NullTime
			orgID sql.NullString
			n     int
		)
		if err := rows.Scan(&hour, &orgID, &n); err != nil {
			return db.DeploymentSignerUsage{}, err
		}
		if hour.Valid {
			out.Total += n
			out.ByHour = append(out.ByHour, db.HourCount{Hour: hour.Time.UTC(), Count: n})
		} else {
			out.ByOrg = append(out.ByOrg, db.OrgCount{OrgID: orgID.String, Count: n})
		}
	}
	if err := rows.Err(); err != nil {
		return db.DeploymentSignerUsage{}, err
	}
	db.SortDeploymentSignerUsage(&out)
	return out, nil
}
