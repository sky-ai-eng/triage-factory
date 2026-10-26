package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgCommitSignerStore is the SQLite impl of db.OrgCommitSignerStore. Local
// mode is N=1 — one process, one org — so the app/admin split the Postgres
// impl draws collapses here and GetSystem shares the one connection, with the
// org asserted rather than gated.
type orgCommitSignerStore struct{ q queryer }

func newOrgCommitSignerStore(q queryer) db.OrgCommitSignerStore {
	return &orgCommitSignerStore{q: q}
}

var _ db.OrgCommitSignerStore = (*orgCommitSignerStore)(nil)

func (s *orgCommitSignerStore) Get(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row, err := db.ScanOrgCommitSigner(s.q.QueryRowContext(ctx,
		`SELECT `+db.OrgCommitSignerColumns+` FROM org_commit_signers WHERE org_id = ?`, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *orgCommitSignerStore) GetSystem(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error) {
	return s.Get(ctx, orgID)
}

func (s *orgCommitSignerStore) Create(ctx context.Context, signer domain.OrgCommitSigner) (domain.OrgCommitSigner, error) {
	if err := assertLocalOrg(signer.OrgID); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	host := db.NormalizeGitHubHost(signer.Host)
	if err := db.ValidateNewOrgCommitSigner(signer, host); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	// DO NOTHING rather than an error-code match: the one conflict this
	// table can have is the org's own row, so zero rows back is exactly
	// "the org already has a signer".
	now := time.Now().UTC()
	row, err := db.ScanOrgCommitSigner(s.q.QueryRowContext(ctx, `
		INSERT INTO org_commit_signers (org_id, host, github_user_id, login, name, email_override, public_key,
			status, status_since, created_at, updated_at, created_by)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9, ?9, ?10)
		ON CONFLICT (org_id) DO NOTHING
		RETURNING `+db.OrgCommitSignerColumns,
		signer.OrgID, host, signer.GitHubUserID, signer.Login, nullIfEmpty(signer.Name),
		nullIfEmpty(signer.EmailOverride), signer.PublicKey, string(domain.CommitSignerPending), now,
		nullIfEmpty(signer.CreatedBy)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.OrgCommitSigner{}, db.ErrOrgCommitSignerExists
	}
	return row, err
}

func (s *orgCommitSignerStore) SetEmailOverride(ctx context.Context, orgID, email string) (domain.OrgCommitSigner, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	return db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET email_override = ?2, updated_at = ?3
		 WHERE org_id = ?1
		RETURNING `+db.OrgCommitSignerColumns, orgID, nullIfEmpty(email), time.Now().UTC()).Scan)
}

func (s *orgCommitSignerStore) StagePendingKey(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	if publicKey == "" {
		return domain.OrgCommitSigner{}, fmt.Errorf("%w: no public key", db.ErrInvalidOrgCommitSigner)
	}
	return db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET pending_public_key = ?2, updated_at = ?3
		 WHERE org_id = ?1
		RETURNING `+db.OrgCommitSignerColumns, orgID, publicKey, time.Now().UTC()).Scan)
}

func (s *orgCommitSignerStore) Activate(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	// Every SET expression reads the row as it was before the statement, so
	// status_since compares against the old status and the pending key is
	// cleared only when it is the one being promoted.
	row, err := db.ScanUpdatedOrgCommitSigner(s.q.QueryRowContext(ctx, `
		UPDATE org_commit_signers SET
			public_key         = ?2,
			pending_public_key = CASE WHEN pending_public_key = ?2 THEN NULL ELSE pending_public_key END,
			status_since       = CASE WHEN status = ?3 THEN status_since ELSE ?4 END,
			status             = ?3,
			status_reason      = NULL,
			checked_at         = ?4,
			updated_at         = ?4
		 WHERE org_id = ?1 AND (public_key = ?2 OR pending_public_key = ?2)
		RETURNING `+db.OrgCommitSignerColumns, orgID, publicKey, string(domain.CommitSignerActive), time.Now().UTC()).Scan)
	if !errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
		return row, err
	}
	current, getErr := s.Get(ctx, orgID)
	if getErr != nil {
		return domain.OrgCommitSigner{}, getErr
	}
	return domain.OrgCommitSigner{}, db.ActivateMiss(current, publicKey)
}

func (s *orgCommitSignerStore) Delete(ctx context.Context, orgID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `DELETE FROM org_commit_signers WHERE org_id = ?`, orgID)
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

// commitSignatureStore is the SQLite impl of db.CommitSignatureStore. The
// local daemon is its only writer, and records each signature as it makes
// it.
type commitSignatureStore struct{ q queryer }

func newCommitSignatureStore(q queryer) db.CommitSignatureStore {
	return &commitSignatureStore{q: q}
}

var _ db.CommitSignatureStore = (*commitSignatureStore)(nil)

func (s *commitSignatureStore) InsertSystem(ctx context.Context, sig domain.CommitSignature) (domain.CommitSignature, error) {
	if err := assertLocalOrg(sig.OrgID); err != nil {
		return domain.CommitSignature{}, err
	}
	sig.SignerHost = db.NormalizeGitHubHost(sig.SignerHost)
	if err := db.ValidateNewCommitSignature(sig); err != nil {
		return domain.CommitSignature{}, err
	}
	var signedAt any
	if sig.SignedAt != nil {
		signedAt = sig.SignedAt.UTC()
	}
	return db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		INSERT INTO commit_signatures (id, org_id, conversation_id, claim_id, boot_epoch, signer, signer_host,
			signer_user_id, public_key, digest, state, signature, signed_at, commit_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING `+db.CommitSignatureColumns,
		uuid.New().String(), sig.OrgID, sig.ConversationID, sig.ClaimID, sig.BootEpoch, string(sig.Signer),
		sig.SignerHost, sig.SignerUserID, nullIfEmpty(sig.PublicKey), sig.Digest, string(sig.State),
		nullIfEmpty(sig.Signature), signedAt, nullIfEmpty(sig.CommitSHA), time.Now().UTC()).Scan)
}

func (s *commitSignatureStore) GetSystem(ctx context.Context, orgID, id string) (*domain.CommitSignature, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx,
		`SELECT `+db.CommitSignatureColumns+` FROM commit_signatures WHERE org_id = ? AND id = ?`,
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
	if err := assertLocalOrg(orgID); err != nil {
		return domain.CommitSignature{}, err
	}
	if commitSHA == "" {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET commit_sha = ?3
		 WHERE org_id = ?1 AND id = ?2 AND state = ?4
		   AND (commit_sha IS NULL OR commit_sha = ?3)
		RETURNING `+db.CommitSignatureColumns,
		orgID, id, commitSHA, string(domain.CommitSignatureSigned)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

func (s *commitSignatureStore) OrgSignaturesSinceSystem(ctx context.Context, orgID string, since time.Time) (db.SignatureWindow, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return db.SignatureWindow{}, err
	}
	// min() over the stored text rather than over a parsed time: every
	// signed_at is bound from a UTC Go time, so the text orders as the
	// instants do, and the oldest is parsed once below.
	var (
		out    db.SignatureWindow
		oldest sql.NullString
	)
	if err := s.q.QueryRowContext(ctx, `
		SELECT count(*), min(signed_at) FROM commit_signatures
		 WHERE org_id = ? AND state = ? AND signed_at > ?`,
		orgID, string(domain.CommitSignatureSigned), since.UTC()).Scan(&out.Count, &oldest); err != nil {
		return db.SignatureWindow{}, err
	}
	if oldest.Valid {
		t, err := parseDBDatetime(oldest.String)
		if err != nil {
			return db.SignatureWindow{}, fmt.Errorf("parse oldest signed_at %q: %w", oldest.String, err)
		}
		t = t.UTC()
		out.Oldest = &t
	}
	return out, nil
}

func (s *commitSignatureStore) ClaimSignaturesSystem(ctx context.Context, orgID, claimID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT count(*) FROM commit_signatures
		 WHERE org_id = ? AND claim_id = ? AND state = ?`,
		orgID, claimID, string(domain.CommitSignatureSigned)).Scan(&n)
	return n, err
}

func (s *commitSignatureStore) NewestFailedVerificationSystem(ctx context.Context, orgID, host string, userID int64) (*domain.CommitSignature, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// signed_at orders as text for the reason OrgSignaturesSinceSystem's
	// min() does.
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		SELECT `+db.CommitSignatureColumns+` FROM commit_signatures
		 WHERE org_id = ? AND state = ? AND signer = ? AND signer_host = ? AND signer_user_id = ?
		   AND verification_reason IS NOT NULL AND verification_reason <> ?
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

// The queue transitions below are multi mode's: the local daemon records each
// signature as it makes it, so a local database never holds a request. They
// are implemented for the interface's sake, with the same semantics.

func (s *commitSignatureStore) ClaimRequestSystem(ctx context.Context, orgID, id, claimedBy string) (*domain.CommitSignature, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = ?, claimed_by = ?
		 WHERE org_id = ? AND id = ? AND state = ?
		RETURNING `+db.CommitSignatureColumns,
		string(domain.CommitSignatureSigning), claimedBy, orgID, id, string(domain.CommitSignatureRequested)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *commitSignatureStore) SignSystem(ctx context.Context, orgID, id, claimedBy, publicKey, signature string, signedAt time.Time) (domain.CommitSignature, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.CommitSignature{}, err
	}
	if err := db.ValidateCommitSignatureSign(publicKey, signature, signedAt); err != nil {
		return domain.CommitSignature{}, err
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = ?, public_key = ?, signature = ?, signed_at = ?
		 WHERE org_id = ? AND id = ? AND state = ? AND claimed_by = ?
		RETURNING `+db.CommitSignatureColumns,
		string(domain.CommitSignatureSigned), publicKey, signature, signedAt.UTC(),
		orgID, id, string(domain.CommitSignatureSigning), claimedBy).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

func (s *commitSignatureStore) RefuseSystem(ctx context.Context, orgID, id, claimedBy, reason string, retryAfterSeconds int) (domain.CommitSignature, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.CommitSignature{}, err
	}
	if err := db.ValidateCommitSignatureRefusal(reason, retryAfterSeconds); err != nil {
		return domain.CommitSignature{}, err
	}
	var retryAfter any
	if retryAfterSeconds > 0 {
		retryAfter = retryAfterSeconds
	}
	row, err := db.ScanCommitSignature(s.q.QueryRowContext(ctx, `
		UPDATE commit_signatures SET state = ?, refusal_reason = ?, retry_after_seconds = ?
		 WHERE org_id = ? AND id = ? AND state = ? AND claimed_by = ?
		RETURNING `+db.CommitSignatureColumns,
		string(domain.CommitSignatureRefused), reason, retryAfter,
		orgID, id, string(domain.CommitSignatureSigning), claimedBy).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CommitSignature{}, db.ErrNoSuchCommitSignature
	}
	return row, err
}

// ListRequestedSystem compares against a Go time, the way every created_at
// here is bound, so both sides of the comparison are the same text format.
func (s *commitSignatureStore) ListRequestedSystem(ctx context.Context, minAge time.Duration, limit int) ([]domain.CommitSignature, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+db.CommitSignatureColumns+` FROM commit_signatures
		 WHERE state = ? AND created_at <= ?
		 ORDER BY created_at, id
		 LIMIT ?`,
		string(domain.CommitSignatureRequested), time.Now().UTC().Add(-minAge), limit)
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
		oldest sql.NullString
	)
	if err := s.q.QueryRowContext(ctx, `
		SELECT count(*), min(created_at) FROM commit_signatures WHERE state = ?`,
		string(domain.CommitSignatureRequested)).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if !oldest.Valid {
		return count, 0, nil
	}
	t, err := parseDBDatetime(oldest.String)
	if err != nil {
		return 0, 0, fmt.Errorf("parse oldest created_at %q: %w", oldest.String, err)
	}
	return count, max(time.Since(t), 0), nil
}
