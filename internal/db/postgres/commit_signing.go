package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgCommitSignerStore is the Postgres impl of db.OrgCommitSignerStore.
// Split-pool: q is the app pool (RLS — member SELECT, org-admin write), admin
// serves GetSystem for the identity resolution that runs with no request
// claims.
type orgCommitSignerStore struct {
	q     queryer
	admin queryer
}

func newOrgCommitSignerStore(q, admin queryer) db.OrgCommitSignerStore {
	return &orgCommitSignerStore{q: q, admin: admin}
}

var _ db.OrgCommitSignerStore = (*orgCommitSignerStore)(nil)

func (s *orgCommitSignerStore) Get(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error) {
	return getOrgCommitSigner(ctx, s.q, orgID)
}

func (s *orgCommitSignerStore) GetSystem(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error) {
	return getOrgCommitSigner(ctx, s.admin, orgID)
}

func getOrgCommitSigner(ctx context.Context, q queryer, orgID string) (*domain.OrgCommitSigner, error) {
	row, err := db.ScanOrgCommitSigner(q.QueryRowContext(ctx,
		`SELECT `+db.OrgCommitSignerColumns+` FROM org_commit_signers WHERE org_id = $1`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *orgCommitSignerStore) Create(ctx context.Context, signer domain.OrgCommitSigner) (domain.OrgCommitSigner, error) {
	host := db.NormalizeGitHubHost(signer.Host)
	if err := db.ValidateNewOrgCommitSigner(signer, host); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	// DO NOTHING rather than an error-code match: the one conflict this
	// table can have is the org's own row, so zero rows back is exactly
	// "the org already has a signer".
	row, err := db.ScanOrgCommitSigner(s.q.QueryRowContext(ctx, `
		INSERT INTO org_commit_signers (org_id, host, github_user_id, login, name, email_override, public_key, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (org_id) DO NOTHING
		RETURNING `+db.OrgCommitSignerColumns,
		signer.OrgID, host, signer.GitHubUserID, signer.Login, nullIfEmpty(signer.Name),
		nullIfEmpty(signer.EmailOverride), signer.PublicKey, string(domain.CommitSignerPending),
		nullIfEmpty(signer.CreatedBy)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.OrgCommitSigner{}, db.ErrOrgCommitSignerExists
	}
	return row, err
}

func (s *orgCommitSignerStore) SetEmailOverride(ctx context.Context, orgID, email string) (domain.OrgCommitSigner, error) {
	return db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET email_override = $2
		 WHERE org_id = $1
		RETURNING `+db.OrgCommitSignerColumns, orgID, nullIfEmpty(email)).Scan)
}

func (s *orgCommitSignerStore) StagePendingKey(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error) {
	if publicKey == "" {
		return domain.OrgCommitSigner{}, fmt.Errorf("%w: no public key", db.ErrInvalidOrgCommitSigner)
	}
	return db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET pending_public_key = $2
		 WHERE org_id = $1
		RETURNING `+db.OrgCommitSignerColumns, orgID, publicKey).Scan)
}

func (s *orgCommitSignerStore) Activate(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error) {
	// Every SET expression reads the row as it was before the statement, so
	// status_since compares against the old status and the pending key is
	// cleared only when it is the one being promoted.
	row, err := db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET
			public_key         = $2,
			pending_public_key = CASE WHEN pending_public_key = $2 THEN NULL ELSE pending_public_key END,
			status_since       = CASE WHEN status = $3 THEN status_since ELSE now() END,
			status             = $3,
			status_reason      = NULL,
			checked_at         = now()
		 WHERE org_id = $1 AND (public_key = $2 OR pending_public_key = $2)
		RETURNING `+db.OrgCommitSignerColumns, orgID, publicKey, string(domain.CommitSignerActive)).Scan)
	if !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
		return row, err
	}
	current, getErr := getOrgCommitSigner(ctx, s.q, orgID)
	if getErr != nil {
		return domain.OrgCommitSigner{}, getErr
	}
	return domain.OrgCommitSigner{}, db.ActivateMiss(current, publicKey)
}

func (s *orgCommitSignerStore) Delete(ctx context.Context, orgID string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM org_commit_signers WHERE org_id = $1`, orgID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrNoSuchOrgCommitSigner
	}
	return nil
}

// commitSignatureStore is the Postgres impl of db.CommitSignatureStore.
// Admin-pool only: every caller is a run's daemon, its orchestrator or a
// control pod, and the executor's tf_system role holds the grants these
// statements need.
type commitSignatureStore struct{ q queryer }

func newCommitSignatureStore(q queryer) db.CommitSignatureStore {
	return &commitSignatureStore{q: q}
}

// NewCommitSignatureStore exposes the admin-pool store for the tf_system
// conformance run, which binds it to the executor's own role.
func NewCommitSignatureStore(q *sql.DB) db.CommitSignatureStore {
	return newCommitSignatureStore(q)
}

var _ db.CommitSignatureStore = (*commitSignatureStore)(nil)

func (s *commitSignatureStore) InsertSystem(ctx context.Context, sig domain.CommitSignature) (domain.CommitSignature, error) {
	sig.SignerHost = db.NormalizeGitHubHost(sig.SignerHost)
	if err := db.ValidateNewCommitSignature(sig); err != nil {
		return domain.CommitSignature{}, err
	}
	var signedAt any
	if sig.SignedAt != nil {
		signedAt = sig.SignedAt.UTC()
	}
	return db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		INSERT INTO commit_signatures (org_id, conversation_id, claim_id, boot_epoch, signer, signer_host,
			signer_user_id, public_key, digest, state, signature, signed_at, commit_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+db.CommitSignatureColumns,
		sig.OrgID, sig.ConversationID, sig.ClaimID, sig.BootEpoch, string(sig.Signer), sig.SignerHost,
		sig.SignerUserID, nullIfEmpty(sig.PublicKey), sig.Digest, string(sig.State), nullIfEmpty(sig.Signature),
		signedAt, nullIfEmpty(sig.CommitSHA)).Scan)
}

func (s *commitSignatureStore) GetSystem(ctx context.Context, orgID, id string) (*domain.CommitSignature, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx,
		`SELECT `+db.CommitSignatureColumns+` FROM commit_signatures WHERE org_id = $1 AND id = $2`,
		orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *commitSignatureStore) SetCommitSHASystem(ctx context.Context, orgID, id, commitSHA string) (domain.CommitSignature, error) {
	if !isValidUUID(id) || commitSHA == "" {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET commit_sha = $3
		 WHERE org_id = $1 AND id = $2 AND state = $4
		   AND (commit_sha IS NULL OR commit_sha = $3)
		RETURNING `+db.CommitSignatureColumns,
		orgID, id, commitSHA, string(domain.CommitSignatureSigned)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

func (s *commitSignatureStore) OrgSignaturesSinceSystem(ctx context.Context, orgID string, since time.Time) (db.SignatureWindow, error) {
	var (
		out    db.SignatureWindow
		oldest sql.NullTime
	)
	if err := s.q.QueryRowContext(ctx, `
		SELECT count(*), min(signed_at) FROM commit_signatures
		 WHERE org_id = $1 AND state = $2 AND signed_at > $3`,
		orgID, string(domain.CommitSignatureSigned), since.UTC()).Scan(&out.Count, &oldest); err != nil {
		return db.SignatureWindow{}, err
	}
	if oldest.Valid {
		t := oldest.Time.UTC()
		out.Oldest = &t
	}
	return out, nil
}

func (s *commitSignatureStore) ClaimSignaturesSystem(ctx context.Context, orgID, claimID string) (int, error) {
	if !isValidUUID(claimID) {
		return 0, nil
	}
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT count(*) FROM commit_signatures
		 WHERE org_id = $1 AND claim_id = $2 AND state = $3`,
		orgID, claimID, string(domain.CommitSignatureSigned)).Scan(&n)
	return n, err
}

func (s *commitSignatureStore) NewestFailedVerificationSystem(ctx context.Context, orgID, host string, userID int64) (*domain.CommitSignature, error) {
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		SELECT `+db.CommitSignatureColumns+` FROM commit_signatures
		 WHERE org_id = $1 AND state = $2 AND signer = $3 AND signer_host = $4 AND signer_user_id = $5
		   AND verification_reason IS NOT NULL AND verification_reason <> $6
		 ORDER BY signed_at DESC, id DESC
		 LIMIT 1`,
		orgID, string(domain.CommitSignatureSigned), string(domain.CommitSigningOrg),
		db.NormalizeGitHubHost(host), userID, domain.CommitVerificationValid).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *commitSignatureStore) ClaimRequestSystem(ctx context.Context, orgID, id, claimedBy string) (*domain.CommitSignature, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = $3, claimed_by = $4
		 WHERE org_id = $1 AND id = $2 AND state = $5
		RETURNING `+db.CommitSignatureColumns,
		orgID, id, string(domain.CommitSignatureSigning), claimedBy, string(domain.CommitSignatureRequested)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *commitSignatureStore) SignSystem(ctx context.Context, orgID, id, claimedBy, publicKey, signature string, signedAt time.Time) (domain.CommitSignature, error) {
	if err := db.ValidateCommitSignatureSign(publicKey, signature, signedAt); err != nil {
		return domain.CommitSignature{}, err
	}
	if !isValidUUID(id) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = $5, public_key = $6, signature = $7, signed_at = $8
		 WHERE org_id = $1 AND id = $2 AND state = $3 AND claimed_by = $4
		RETURNING `+db.CommitSignatureColumns,
		orgID, id, string(domain.CommitSignatureSigning), claimedBy,
		string(domain.CommitSignatureSigned), publicKey, signature, signedAt.UTC()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

func (s *commitSignatureStore) RefuseSystem(ctx context.Context, orgID, id, claimedBy, reason string, retryAfterSeconds int) (domain.CommitSignature, error) {
	if err := db.ValidateCommitSignatureRefusal(reason, retryAfterSeconds); err != nil {
		return domain.CommitSignature{}, err
	}
	if !isValidUUID(id) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	var retryAfter any
	if retryAfterSeconds > 0 {
		retryAfter = retryAfterSeconds
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = $5, refusal_reason = $6, retry_after_seconds = $7
		 WHERE org_id = $1 AND id = $2 AND state = $3 AND claimed_by = $4
		RETURNING `+db.CommitSignatureColumns,
		orgID, id, string(domain.CommitSignatureSigning), claimedBy,
		string(domain.CommitSignatureRefused), reason, retryAfter).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

func (s *commitSignatureStore) ListRequestedSystem(ctx context.Context, minAge time.Duration, limit int) ([]domain.CommitSignature, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+db.CommitSignatureColumns+` FROM commit_signatures
		 WHERE state = $1 AND created_at <= statement_timestamp() - make_interval(secs => $2)
		 ORDER BY created_at, id
		 LIMIT $3`,
		string(domain.CommitSignatureRequested), minAge.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CommitSignature
	for rows.Next() {
		row, err := db.ScanCommitSignature(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *commitSignatureStore) PendingRequestsSystem(ctx context.Context) (int, time.Duration, error) {
	var (
		count  int
		oldest float64
	)
	if err := s.q.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM statement_timestamp() - min(created_at)), 0)::float8
		  FROM commit_signatures WHERE state = $1`,
		string(domain.CommitSignatureRequested)).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	return count, max(time.Duration(oldest*float64(time.Second)), 0), nil
}
