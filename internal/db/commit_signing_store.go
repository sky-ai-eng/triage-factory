package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

var (
	// ErrNoSuchOrgCommitSigner is a write's answer for an org with no signer.
	ErrNoSuchOrgCommitSigner = errors.New("db: the org has no commit signer")

	// ErrOrgCommitSignerExists is Create's answer for an org that already has
	// a signer. An org has one; replacing it is a delete and a fresh
	// registration.
	ErrOrgCommitSignerExists = errors.New("db: the org already has a commit signer")

	// ErrCommitSignerKeyChanged is Activate's answer when the key the caller
	// found on the account is no longer either of the signer's keys: it was
	// rotated again after the caller read it.
	ErrCommitSignerKeyChanged = errors.New("db: the commit signer's keys changed after they were read")

	// ErrInvalidOrgCommitSigner wraps a write refused for a missing host,
	// account or key.
	ErrInvalidOrgCommitSigner = errors.New("db: invalid commit signer")

	// ErrNoSuchCommitSignature is a write's answer for a signature row that
	// does not exist in the org, or is not in the state the write needs.
	ErrNoSuchCommitSignature = errors.New("db: no such commit signature")

	// ErrInvalidCommitSignature wraps an InsertSystem refused for a row that
	// could not describe a real signature.
	ErrInvalidCommitSignature = errors.New("db: invalid commit signature")
)

// OrgCommitSignerStore owns org_commit_signers: the GitHub user account an
// org's agent commits are authored, committed and signed as, one row per org.
// The private signing keys are org secrets the caller writes beside the row
// (integrations.KeyGitHubCommitSigningKey and its pending twin); this store
// never sees them.
//
// Split-pool in Postgres. Get and the writes serve the commit-signer routes,
// under RLS on the app pool (member SELECT, org-admin write); GetSystem is for
// the readers with no request claims, which resolve a run's commit identity
// (credprovision, the spawner, a run's signing daemon, a control pod about to
// sign).
type OrgCommitSignerStore interface {
	// Get returns the org's signer, or (nil, nil) when it has none.
	Get(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error)

	// GetSystem is Get on the admin pool.
	GetSystem(ctx context.Context, orgID string) (*domain.OrgCommitSigner, error)

	// Create registers the org's signer and returns the stored row. It reads
	// OrgID, Host, GitHubUserID, Login, Name, EmailOverride, PublicKey and
	// CreatedBy from signer and nothing else: a new signer is pending, with
	// no rotation under way and no check made, whatever the caller passed.
	// Host is stored in ghbase.CanonicalBaseURL form. An org that already has
	// a signer gets ErrOrgCommitSignerExists; a missing host, account or key
	// gets ErrInvalidOrgCommitSigner.
	Create(ctx context.Context, signer domain.OrgCommitSigner) (domain.OrgCommitSigner, error)

	// SetEmailOverride sets the committer address the signer's commits carry
	// and returns the stored row. An empty email clears it, restoring the
	// derived noreply address; whether the host has one is the caller's
	// check.
	SetEmailOverride(ctx context.Context, orgID, email string) (domain.OrgCommitSigner, error)

	// StagePendingKey records a rotation's new public key and returns the
	// stored row. The current key keeps signing until Activate finds the new
	// one; a second rotation before then replaces the pending key.
	StagePendingKey(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error)

	// Activate records that publicKey was found on the signer's account and
	// returns the stored row: the signer is active with no reason, and, when
	// publicKey is the pending key, it becomes the signer's key and the
	// rotation ends. status_since moves only when the status changes;
	// checked_at is stamped either way. A publicKey that is neither of the
	// signer's keys gets ErrCommitSignerKeyChanged, so a rotation staged
	// after the caller read the row is never activated unverified.
	Activate(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error)

	// Delete removes the org's signer. ErrNoSuchOrgCommitSigner when it has
	// none.
	Delete(ctx context.Context, orgID string) error
}

// CommitSignatureStore owns commit_signatures: one row per commit signature a
// run asked for. It is the signing log in both modes and, in multi mode, the
// queue the control pods sign from; the signing budgets are counts over it.
//
// Admin-pool only in Postgres. Every writer is a run's signing daemon, its
// orchestrator or a control pod, none of them acting for a signed-in user, and
// the executor's tf_system role holds the INSERT and the commit-id UPDATE
// these methods need.
type CommitSignatureStore interface {
	// InsertSystem records one signature and returns the stored row: a
	// request (state requested, with no key, signature, signing time or
	// commit id yet) or a signature already made (state signed, with its key,
	// signature and signing time, and the commit id when the caller has
	// computed it). It reads the org, conversation, claim, boot epoch,
	// signing account, digest and those four from sig and nothing else: a
	// new row has no refusal, no claiming pod, no push and no verification,
	// and the store mints its id and creation time. A row that is neither
	// shape, or that names no conversation, claim, signer or digest, gets
	// ErrInvalidCommitSignature.
	InsertSystem(ctx context.Context, sig domain.CommitSignature) (domain.CommitSignature, error)

	// GetSystem returns one row, or (nil, nil) when the org has no row with
	// that id.
	GetSystem(ctx context.Context, orgID, id string) (*domain.CommitSignature, error)

	// SetCommitSHASystem records the commit id a signature produced and
	// returns the stored row. A row is given its commit id once: setting the
	// same id again is a no-op, which is what a resent relay record does,
	// and a row that is not signed or already holds a different id gets
	// ErrNoSuchCommitSignature.
	SetCommitSHASystem(ctx context.Context, orgID, id, commitSHA string) (domain.CommitSignature, error)

	// OrgSignaturesSinceSystem counts the org's signatures made after since,
	// for the per-minute budget, with the signing time of the oldest one
	// counted: the budget admits another once that one is a minute old.
	// Requests not yet signed and refusals are not signatures and are not
	// counted.
	OrgSignaturesSinceSystem(ctx context.Context, orgID string, since time.Time) (SignatureWindow, error)

	// ClaimSignaturesSystem counts the signatures made for one claim, for the
	// per-claim budget.
	ClaimSignaturesSystem(ctx context.Context, orgID, claimID string) (int, error)
}

// SignatureWindow is OrgSignaturesSinceSystem's answer. Oldest is nil when
// Count is zero.
type SignatureWindow struct {
	Count  int
	Oldest *time.Time
}

// OrgCommitSignerColumns is the projection both dialects read an
// org_commit_signers row through: the point reads and every write's RETURNING,
// so no two of them can drift into different shapes.
const OrgCommitSignerColumns = `org_id, host, github_user_id, login, name, email_override, public_key,
	pending_public_key, status, status_reason, status_since, checked_at, created_at, updated_at, created_by`

// ScanOrgCommitSigner decodes one row of OrgCommitSignerColumns. Every column
// is a plain type both backends read the same way, so the scan has no dialect
// content; NULL text reads as "".
func ScanOrgCommitSigner(scan func(...any) error) (domain.OrgCommitSigner, error) {
	var (
		out                                     domain.OrgCommitSigner
		name, email, pending, reason, createdBy sql.NullString
		status                                  string
		checkedAt                               sql.NullTime
	)
	if err := scan(&out.OrgID, &out.Host, &out.GitHubUserID, &out.Login, &name, &email, &out.PublicKey,
		&pending, &status, &reason, &out.StatusSince, &checkedAt, &out.CreatedAt, &out.UpdatedAt, &createdBy); err != nil {
		return domain.OrgCommitSigner{}, err
	}
	out.Name = name.String
	out.EmailOverride = email.String
	out.PendingPublicKey = pending.String
	out.Status = domain.CommitSignerStatus(status)
	out.StatusReason = reason.String
	out.StatusSince = out.StatusSince.UTC()
	out.CheckedAt = utcTimePtr(checkedAt)
	out.CreatedAt = out.CreatedAt.UTC()
	out.UpdatedAt = out.UpdatedAt.UTC()
	out.CreatedBy = createdBy.String
	return out, nil
}

// ValidateNewOrgCommitSigner is Create's refusal for a signer that names no
// host, account or key, shared so both dialects refuse the same rows. host is
// the canonicalized host the row would store.
func ValidateNewOrgCommitSigner(s domain.OrgCommitSigner, host string) error {
	switch {
	case s.OrgID == "":
		return fmt.Errorf("%w: no org", ErrInvalidOrgCommitSigner)
	case host == "":
		return fmt.Errorf("%w: no host", ErrInvalidOrgCommitSigner)
	case s.GitHubUserID <= 0:
		return fmt.Errorf("%w: no account id", ErrInvalidOrgCommitSigner)
	case s.Login == "":
		return fmt.Errorf("%w: no login", ErrInvalidOrgCommitSigner)
	case s.PublicKey == "":
		return fmt.Errorf("%w: no public key", ErrInvalidOrgCommitSigner)
	}
	return nil
}

// CommitSignatureColumns is the projection both dialects read a
// commit_signatures row through, for the same reason as OrgCommitSignerColumns.
const CommitSignatureColumns = `id, org_id, conversation_id, claim_id, boot_epoch, signer, signer_host,
	signer_user_id, public_key, digest, state, refusal_reason, retry_after_seconds, signature, claimed_by,
	signed_at, commit_sha, repository_id, pushed_at, verification_reason, checked_at, created_at`

// ScanCommitSignature decodes one row of CommitSignatureColumns.
func ScanCommitSignature(scan func(...any) error) (domain.CommitSignature, error) {
	var (
		out                                                          domain.CommitSignature
		conversationID, claimID, publicKey, refusalReason, signature sql.NullString
		claimedBy, commitSHA, repositoryID, verificationReason       sql.NullString
		signer, state                                                string
		retryAfter                                                   sql.NullInt64
		signedAt, pushedAt, checkedAt                                sql.NullTime
	)
	if err := scan(&out.ID, &out.OrgID, &conversationID, &claimID, &out.BootEpoch, &signer, &out.SignerHost,
		&out.SignerUserID, &publicKey, &out.Digest, &state, &refusalReason, &retryAfter, &signature, &claimedBy,
		&signedAt, &commitSHA, &repositoryID, &pushedAt, &verificationReason, &checkedAt, &out.CreatedAt); err != nil {
		return domain.CommitSignature{}, err
	}
	out.ConversationID = conversationID.String
	out.ClaimID = claimID.String
	out.Signer = domain.CommitSigningMode(signer)
	out.PublicKey = publicKey.String
	out.State = domain.CommitSignatureState(state)
	out.RefusalReason = refusalReason.String
	out.RetryAfterSeconds = int(retryAfter.Int64)
	out.Signature = signature.String
	out.ClaimedBy = claimedBy.String
	out.SignedAt = utcTimePtr(signedAt)
	out.CommitSHA = commitSHA.String
	out.RepositoryID = repositoryID.String
	out.PushedAt = utcTimePtr(pushedAt)
	out.VerificationReason = verificationReason.String
	out.CheckedAt = utcTimePtr(checkedAt)
	out.CreatedAt = out.CreatedAt.UTC()
	return out, nil
}

// ValidateNewCommitSignature is InsertSystem's refusal, shared so both
// dialects refuse the same rows. A row enters the table as a request with
// nothing signed yet, or as a signature already made with everything that
// makes it one; the states between and after are written by the
// control-plane queue's own transitions.
func ValidateNewCommitSignature(sig domain.CommitSignature) error {
	switch {
	case sig.OrgID == "":
		return fmt.Errorf("%w: no org", ErrInvalidCommitSignature)
	case sig.ConversationID == "" || sig.ClaimID == "":
		return fmt.Errorf("%w: no conversation or claim", ErrInvalidCommitSignature)
	case !sig.Signer.Signs():
		return fmt.Errorf("%w: signer %q is not org or deployment", ErrInvalidCommitSignature, sig.Signer)
	case sig.SignerHost == "" || sig.SignerUserID <= 0:
		return fmt.Errorf("%w: no signing account", ErrInvalidCommitSignature)
	case sig.Digest == "":
		return fmt.Errorf("%w: no digest", ErrInvalidCommitSignature)
	}
	switch sig.State {
	case domain.CommitSignatureRequested:
		if sig.PublicKey != "" || sig.Signature != "" || sig.SignedAt != nil || sig.CommitSHA != "" {
			return fmt.Errorf("%w: a request carries no key, signature, signing time or commit id", ErrInvalidCommitSignature)
		}
	case domain.CommitSignatureSigned:
		if sig.PublicKey == "" || sig.Signature == "" || sig.SignedAt == nil {
			return fmt.Errorf("%w: a signature needs its key, signature and signing time", ErrInvalidCommitSignature)
		}
	default:
		return fmt.Errorf("%w: a row is inserted requested or signed, not %q", ErrInvalidCommitSignature, sig.State)
	}
	return nil
}

func utcTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}
