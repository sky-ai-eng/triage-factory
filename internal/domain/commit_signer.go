package domain

import (
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
)

// CommitSignerStatus is an org signer's health: pending until its key is
// found on the account, active while it signs, broken once a check fails.
// Only an active signer signs; one in any status is the identity a run commits
// as.
type CommitSignerStatus string

const (
	CommitSignerPending CommitSignerStatus = "pending"
	CommitSignerActive  CommitSignerStatus = "active"
	CommitSignerBroken  CommitSignerStatus = "broken"
)

// CommitSignerReasonKeyMissing is a broken signer's StatusReason when its key
// is no longer on the account. Every other reason a signer is broken for is
// the verification reason GitHub gave for a pushed commit.
const CommitSignerReasonKeyMissing = "key_missing"

// CommitVerificationValid is GitHub's verification reason for a verified
// commit. Any other reason on a pushed commit is a failure.
const CommitVerificationValid = "valid"

// CommitVerificationUnknownKey is GitHub's verification reason for a commit
// signed with a key that is not on the account its committer address belongs
// to: what a pushed commit reads when the signer's key went missing.
const CommitVerificationUnknownKey = "unknown_key"

// OrgCommitSigner is one org_commit_signers row: the GitHub user account an
// org's agent commits are authored, committed and signed as, and the public
// half of the key TF signs with. The private half is a secret
// (integrations.KeyGitHubCommitSigningKey), never a column.
//
// The account is identified by its id. Login and Name are display values, so
// a rename of the account changes neither which account signs nor whether its
// commits verify.
//
// Empty strings stand for the nullable columns' NULL.
type OrgCommitSigner struct {
	OrgID string `json:"org_id"`
	// Host is the GitHub the account is on, in ghbase.CanonicalBaseURL form:
	// the org's host when the signer was registered.
	Host         string `json:"host"`
	GitHubUserID int64  `json:"github_user_id"`
	Login        string `json:"login"`
	// Name is the account's profile name, "" when the profile has none.
	Name string `json:"name,omitempty"`
	// EmailOverride is the committer address the signer was given, "" when
	// commits use the derived noreply address.
	EmailOverride string `json:"email_override,omitempty"`
	// PublicKey is the key TF signs with, "ssh-ed25519 AAAA…".
	PublicKey string `json:"public_key"`
	// PendingPublicKey is a rotation's new key, "" outside a rotation. It
	// replaces PublicKey once Verify finds it on the account.
	PendingPublicKey string             `json:"pending_public_key,omitempty"`
	Status           CommitSignerStatus `json:"status"`
	// StatusReason says why the signer is broken: GitHub's verification
	// reason for a pushed commit, or key_missing. "" otherwise.
	StatusReason string    `json:"status_reason,omitempty"`
	StatusSince  time.Time `json:"status_since"`
	// CheckedAt is when a check last read the account's signing keys: a
	// Verify that found the key, or the periodic check, whatever it found.
	// The periodic check runs again once it is 15 minutes old.
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// CreatedBy is the user who registered the signer, "" once that user is
	// deleted.
	CreatedBy string `json:"created_by,omitempty"`
}

// CommitName is the name the signer's commits carry: the account's profile
// name, or its login when the profile has none. A login cannot contain a
// space, so a name like "Triage Factory" exists only as the profile name.
func (s OrgCommitSigner) CommitName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Login
}

// CommitEmail is the committer address the signer's commits carry: the
// override when one is set, else on github.com the id form of the account's
// noreply address, which keeps verifying across a rename of the login. Off
// github.com there is no address to derive, so it is "" without an override.
func (s OrgCommitSigner) CommitEmail() string {
	if s.EmailOverride != "" {
		return s.EmailOverride
	}
	if !DerivesCommitEmail(s.Host) || s.GitHubUserID <= 0 || s.Login == "" {
		return ""
	}
	return strconv.FormatInt(s.GitHubUserID, 10) + "+" + s.Login + "@users.noreply.github.com"
}

// DerivesCommitEmail reports whether a signer on host, in
// ghbase.CanonicalBaseURL form, has a committer address without an override.
// Only github.com does: GHES has no noreply address, and GHE.com's is not
// documented.
func DerivesCommitEmail(host string) bool {
	return host == ghbase.GitHubCom
}

// CommitSigningMode is how a run's commits are signed: not at all, as the
// org's own signer, or as the deployment's.
//
// A commit_signatures row's signer column holds the two signing values; none
// never names a signature.
type CommitSigningMode string

const (
	CommitSigningNone       CommitSigningMode = "none"
	CommitSigningOrg        CommitSigningMode = "org"
	CommitSigningDeployment CommitSigningMode = "deployment"
)

// Signs reports whether m is a mode that signs, and so a value a signature's
// signer column can hold.
func (m CommitSigningMode) Signs() bool {
	return m == CommitSigningOrg || m == CommitSigningDeployment
}

// CommitSignatureState is where one signature is: requested and signing are
// the control-plane queue (multi mode), signed and refused are outcomes.
// Local mode records a signature already made, so its rows start signed.
type CommitSignatureState string

const (
	CommitSignatureRequested CommitSignatureState = "requested"
	CommitSignatureSigning   CommitSignatureState = "signing"
	CommitSignatureSigned    CommitSignatureState = "signed"
	CommitSignatureRefused   CommitSignatureState = "refused"
)

// CommitSignature is one commit_signatures row: one signature request, the
// queue entry the control pods sign from in multi mode and the signing log in
// both modes.
//
// Empty strings and zero numbers stand for the nullable columns' NULL.
type CommitSignature struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// ConversationID is "" once the conversation is purged; the row outlives
	// it, as the signing log.
	ConversationID string `json:"conversation_id,omitempty"`
	// ClaimID and BootEpoch name the engagement that asked for the
	// signature, which the per-claim budget counts by. ClaimID is "" once the
	// conversation's purge takes the claim with it.
	ClaimID   string            `json:"claim_id,omitempty"`
	BootEpoch int64             `json:"boot_epoch"`
	Signer    CommitSigningMode `json:"signer"`
	// SignerHost and SignerUserID name the account that signs. PublicKey is
	// the key that signed, "" until the signature is made, because each
	// signature reads the signer's current key.
	SignerHost   string `json:"signer_host"`
	SignerUserID int64  `json:"signer_user_id"`
	PublicKey    string `json:"public_key,omitempty"`
	// Digest is the hex SHA-512 of the commit payload: what the signature
	// covers, and all the control plane ever sees of the commit.
	Digest string               `json:"digest"`
	State  CommitSignatureState `json:"state"`
	// RefusalReason names why a refused request was refused.
	// RetryAfterSeconds is how long until the org's per-minute budget admits
	// another signature, set only on that refusal.
	RefusalReason     string `json:"refusal_reason,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	// Signature is the armored SSH signature.
	Signature string `json:"signature,omitempty"`
	// ClaimedBy is the control pod that took the request.
	ClaimedBy string     `json:"claimed_by,omitempty"`
	SignedAt  *time.Time `json:"signed_at,omitempty"`
	// CommitSHA is the commit id the signature produced, set once the daemon
	// has computed it.
	CommitSHA string `json:"commit_sha,omitempty"`
	// RepositoryID and PushedAt are set when a push lands the commit as a
	// ref's new tip; VerificationReason and CheckedAt record GitHub's answer
	// for it.
	RepositoryID       string     `json:"repository_id,omitempty"`
	PushedAt           *time.Time `json:"pushed_at,omitempty"`
	VerificationReason string     `json:"verification_reason,omitempty"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}
