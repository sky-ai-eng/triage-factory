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
	// The miss is either no signer or keys that moved since the caller read
	// them; only the second is worth a verify again.
	current, getErr := getOrgCommitSigner(ctx, s.q, orgID)
	if getErr != nil {
		return domain.OrgCommitSigner{}, getErr
	}
	if current != nil {
		return domain.OrgCommitSigner{}, db.ErrCommitSignerKeyChanged
	}
	return domain.OrgCommitSigner{}, db.ErrNoSuchOrgCommitSigner
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
