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
	// after the caller read the row is never activated unverified; a caller
	// the write policy refuses gets ErrNoSuchOrgCommitSigner, as from the
	// other writes.
	Activate(ctx context.Context, orgID, publicKey string) (domain.OrgCommitSigner, error)

	// Delete removes the org's signer. ErrNoSuchOrgCommitSigner when it has
	// none.
	Delete(ctx context.Context, orgID string) error

	// RecordKeyCheckSystem records a periodic check of the signer's key on its
	// account and returns the stored row, or (nil, nil) when the org's signer
	// is no longer the account and key the check read: it was removed,
	// registered again, or had a rotation promoted while the check was out.
	//
	// It refreshes the login and profile name, stamps checked_at, and moves
	// the status only between active and broken for key_missing: a key gone
	// from the account breaks an active signer, and a key back on it restores
	// one broken for its absence. A pending signer waits for an admin's
	// Verify, and one broken by a pushed commit keeps that reason until a
	// Verify. status_since moves only with the status.
	RecordKeyCheckSystem(ctx context.Context, orgID string, check SignerKeyCheck) (*domain.OrgCommitSigner, error)

	// BreakSystem records that GitHub reported a pushed commit signed with
	// publicKey as not verified, for reason, and returns the stored row: an
	// active signer holding that key becomes broken with that reason, which
	// stays until a Verify. A signer broken because its key was missing takes
	// the reason too, unless it is unknown_key: that is the commit's view of
	// the same fault, which the periodic check clears once the key is back.
	// Any other signer, or none holding publicKey, is left as it is and
	// (nil, nil) is returned. status_since moves only with the status. An
	// empty reason, or valid, gets ErrInvalidOrgCommitSigner.
	BreakSystem(ctx context.Context, orgID, publicKey, reason string) (*domain.OrgCommitSigner, error)

	// ListHealthSystem returns every org's signer status and reason, ordered
	// by org, for the signer gauges. Cross-org: the gauges are a system job
	// serving every org.
	ListHealthSystem(ctx context.Context) ([]SignerHealth, error)
}

// SignerKeyCheck is one periodic check of a signer's key, as
// RecordKeyCheckSystem stores it.
type SignerKeyCheck struct {
	// GitHubUserID and PublicKey are the account and key the check read. The
	// write misses a signer that is no longer that account or no longer signs
	// with that key.
	GitHubUserID int64
	PublicKey    string
	// Login and Name are the account as GitHub answered for it by its id; an
	// empty Name is a profile with no name. An empty Login is an account the
	// check could not read, which keeps both as stored.
	Login string
	Name  string
	// KeyFound is whether PublicKey is among the account's signing keys.
	KeyFound bool
}

// KeyCheckTransition is the one status move a key check makes: from the status
// and reason a signer must hold to the ones it takes. Both dialects render
// their UPDATE from it, so they move the same signers.
func KeyCheckTransition(keyFound bool) (fromStatus domain.CommitSignerStatus, fromReason string, toStatus domain.CommitSignerStatus, toReason string) {
	if keyFound {
		return domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing, domain.CommitSignerActive, ""
	}
	return domain.CommitSignerActive, "", domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing
}

// ValidateBreakReason is BreakSystem's refusal of a reason that is no failure,
// shared so both dialects refuse the same writes.
func ValidateBreakReason(reason string) error {
	if reason == "" || reason == domain.CommitVerificationValid {
		return fmt.Errorf("%w: a signer is broken for a failing verification reason, not %q", ErrInvalidOrgCommitSigner, reason)
	}
	return nil
}

// SignerHealth is one org signer's status, as ListHealthSystem reads it.
// Reason is "" for a signer that is not broken.
type SignerHealth struct {
	OrgID  string
	Status domain.CommitSignerStatus
	Reason string
}

// CommitSignatureStore owns commit_signatures: one row per commit signature a
// run asked for. It is the signing log in both modes and, in multi mode, the
// queue the control pods sign from; the signing budgets are counts over it.
//
// Admin-pool only in Postgres. Every writer is a run's signing daemon, its
// orchestrator or a control pod, none of them acting for a signed-in user. The
// executor's tf_system role holds the INSERT and the commit-id and push UPDATEs
// the orchestrator's methods need; the queue's transitions (ClaimRequestSystem,
// SignSystem, RefuseSystem) and the post-push check's verdict
// (SetVerificationSystem) are the control plane's, which tf_system cannot
// make.
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

	// ForCommitSystem returns the signature that produced the commit
	// commitSHA, the newest when more than one did (signing the same commit
	// twice makes the same commit), or (nil, nil) when none of the org's did.
	// A push's audit row reads it for the account that signed the pushed tip.
	ForCommitSystem(ctx context.Context, orgID, commitSHA string) (*domain.CommitSignature, error)

	// NewestFailedVerificationSystem returns the most recently signed of the
	// org's signatures made as its own signer on the account (host, userID)
	// whose pushed commit GitHub last reported as not verified, or (nil, nil)
	// when there is none. Verify re-reads that commit: GitHub evaluates a
	// failing result again on every read, so it reads valid once the fault is
	// fixed.
	NewestFailedVerificationSystem(ctx context.Context, orgID, host string, userID int64) (*domain.CommitSignature, error)

	// MarkPushedSystem records that a push landed signature id's commit as a
	// ref's new tip in repositoryID, stamping pushed_at, and returns the
	// stored row. The first push is the one kept, so the post-push check
	// reads the commit once: a row already stamped is left as it is, and it
	// and a row that is not a signature of the org's get (nil, nil). An empty
	// repositoryID gets ErrInvalidCommitSignature.
	MarkPushedSystem(ctx context.Context, orgID, id, repositoryID string) (*domain.CommitSignature, error)

	// UncheckedPushesSystem returns up to limit of the org's signatures made
	// as signer whose commit a push landed after since and whose verification
	// GitHub has not yet been asked for: the post-push check's work. Newest
	// push first, so a commit GitHub keeps failing to answer for cannot hold
	// back the ones pushed after it; since bounds how long one is retried.
	UncheckedPushesSystem(ctx context.Context, orgID string, signer domain.CommitSigningMode, since time.Time, limit int) ([]domain.CommitSignature, error)

	// SetVerificationSystem records GitHub's verification reason for a pushed
	// signature's commit, stamps checked_at, and returns the stored row. An
	// empty reason records that GitHub had no answer to give: the commit or
	// its repository is gone. A row that is not a pushed signature of the
	// org's gets ErrNoSuchCommitSignature.
	//
	// A control-plane write: the executor's tf_system role cannot make it.
	SetVerificationSystem(ctx context.Context, orgID, id, reason string) (domain.CommitSignature, error)

	// ClaimRequestSystem takes one request for the control pod claimedBy to
	// decide: requested → signing, stamping the pod, and returns the stored
	// row. The update is conditional on the row still being a request, so
	// when several pods race for one row exactly one gets it; the others,
	// and a caller naming a row that is not a request in the org, get
	// (nil, nil).
	//
	// A row whose pod dies before it decides stays signing. Nothing waits on
	// it by then: the run that asked gives up within its own deadline, and a
	// row that is not signed counts toward no budget.
	ClaimRequestSystem(ctx context.Context, orgID, id, claimedBy string) (*domain.CommitSignature, error)

	// SignSystem stores the signature claimedBy made for a request it
	// claimed, with the key that made it and the signing time, and returns
	// the stored row: signing → signed. A row that is not signing under that
	// pod gets ErrNoSuchCommitSignature; a missing key or signature gets
	// ErrInvalidCommitSignature.
	SignSystem(ctx context.Context, orgID, id, claimedBy, publicKey, signature string, signedAt time.Time) (domain.CommitSignature, error)

	// RefuseSystem stores claimedBy's refusal of a request it claimed and
	// returns the stored row: signing → refused, with the reason and, for a
	// refusal by the per-minute budget, the seconds until the budget admits
	// another (zero otherwise). A row that is not signing under that pod
	// gets ErrNoSuchCommitSignature; an empty reason or a negative wait gets
	// ErrInvalidCommitSignature.
	RefuseSystem(ctx context.Context, orgID, id, claimedBy, reason string, retryAfterSeconds int) (domain.CommitSignature, error)

	// ListRequestedSystem returns up to limit requests no pod has claimed
	// that are at least minAge old on the database's clock, oldest first:
	// a control pod's sweep for the requests whose doorbell it never heard.
	// minAge keeps the sweep off a request whose doorbell is still in
	// flight. Cross-org: the sweep is a system job serving every org.
	ListRequestedSystem(ctx context.Context, minAge time.Duration, limit int) ([]domain.CommitSignature, error)

	// PendingRequestsSystem counts the requests no pod has claimed, across
	// every org, and returns how long the oldest has waited on the
	// database's clock (zero when there are none).
	PendingRequestsSystem(ctx context.Context) (count int, oldestAge time.Duration, err error)
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

// ScanUpdatedOrgCommitSigner is ScanOrgCommitSigner for an UPDATE … RETURNING
// keyed on the org: no row back is the org having no signer the statement
// could reach, ErrNoSuchOrgCommitSigner.
func ScanUpdatedOrgCommitSigner(scan func(...any) error) (domain.OrgCommitSigner, error) {
	out, err := ScanOrgCommitSigner(scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.OrgCommitSigner{}, ErrNoSuchOrgCommitSigner
	}
	return out, err
}

// ActivateMiss is Activate's answer when its UPDATE matched no row, given the
// signer a follow-up read found. Only a signer that holds neither key is a
// rotation staged after the caller read it. A signer that still holds the key
// was out of the statement's reach, as RLS keeps it from a member who is not
// an org admin, and that reads as every other refused signer write does.
func ActivateMiss(current *domain.OrgCommitSigner, publicKey string) error {
	if current != nil && current.PublicKey != publicKey && current.PendingPublicKey != publicKey {
		return ErrCommitSignerKeyChanged
	}
	return ErrNoSuchOrgCommitSigner
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

// ValidateCommitSignatureSign is SignSystem's refusal of a signature that
// could not verify, shared so both dialects refuse the same writes.
func ValidateCommitSignatureSign(publicKey, signature string, signedAt time.Time) error {
	if publicKey == "" || signature == "" || signedAt.IsZero() {
		return fmt.Errorf("%w: a signature needs its key, signature and signing time", ErrInvalidCommitSignature)
	}
	return nil
}

// ValidateCommitSignatureRefusal is RefuseSystem's refusal of a refusal that
// names no reason, shared so both dialects refuse the same writes.
func ValidateCommitSignatureRefusal(reason string, retryAfterSeconds int) error {
	if reason == "" {
		return fmt.Errorf("%w: a refusal names its reason", ErrInvalidCommitSignature)
	}
	if retryAfterSeconds < 0 {
		return fmt.Errorf("%w: a negative wait", ErrInvalidCommitSignature)
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
