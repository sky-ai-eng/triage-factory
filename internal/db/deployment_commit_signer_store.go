package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrNoSuchDeploymentCommitSigner is a write's answer for a key the
// deployment signer has no row for on that host.
var ErrNoSuchDeploymentCommitSigner = errors.New("db: no deployment commit signer row for that key")

// DeploymentCommitSignerStore owns deployment_commit_signer: the health of the
// deployment's commit signer, one row per key it is configured with on one
// GitHub. The signer's login, key and committer address are control-pod
// config; a row holds what the checks learn from GitHub about the account the
// login names and whether GitHub verifies what the key signs. Every control
// pod reads its own key's row before it signs, so a key that stops verifying
// stops signing on every pod at once.
//
// Postgres only, because the deployment signer is multi mode's, as the
// deployment App is: the SQLite impl answers ErrNotApplicableInLocal from
// every method. Admin pool only: every reader and writer is a control pod (the
// resolution that seals a bundle or decides a signature, the brain's checks,
// the operator-gated fleet console), none acting under an org's RLS.
type DeploymentCommitSignerStore interface {
	// GetSystem returns the row for publicKey on host, or (nil, nil) when
	// there is none.
	GetSystem(ctx context.Context, host, publicKey string) (*domain.DeploymentCommitSigner, error)

	// RegisterSystem returns the row for publicKey on host, creating it
	// pending under login when there is none. An existing row is returned as
	// stored: once a check has read the account the login named, the row
	// follows the account by its id, and a login changed in config since names
	// nothing it keys on.
	RegisterSystem(ctx context.Context, host, publicKey, login string) (domain.DeploymentCommitSigner, error)

	// RecordKeyCheckSystem records one read of the account and its signing
	// keys, made against before, and returns the stored row, or (nil, nil)
	// when the row no longer holds before's status, reason and account: a
	// concurrent check, a Verify or a break moved it while this one was out.
	//
	// It stamps checked_at and, for an account the check read, records its id,
	// login and profile name. The status moves by
	// DeploymentKeyCheckTransition; status_since moves only with it.
	RecordKeyCheckSystem(ctx context.Context, before domain.DeploymentCommitSigner, check DeploymentSignerKeyCheck) (*domain.DeploymentCommitSigner, error)

	// ActivateSystem records a Verify that found publicKey on the account
	// userID names and returns the stored row: active with no reason, the
	// account's id, login and name recorded, checked_at stamped, and
	// status_since moved only when the status changes. A row whose account is
	// another one gets ErrNoSuchDeploymentCommitSigner, and so does a key with
	// no row.
	ActivateSystem(ctx context.Context, host, publicKey string, userID int64, login, name string) (domain.DeploymentCommitSigner, error)

	// BreakSystem records that GitHub reported a pushed commit signed with
	// publicKey as not verified, for reason, and returns the stored row, with
	// OrgCommitSignerStore.BreakSystem's precedence: an active signer becomes
	// broken with that reason, which stays until a Verify, and one broken for
	// key_missing takes the reason unless it is unknown_key. Any other row is
	// left as it is and (nil, nil) is returned. An empty reason, or valid, gets
	// ErrInvalidOrgCommitSigner.
	BreakSystem(ctx context.Context, host, publicKey, reason string) (*domain.DeploymentCommitSigner, error)

	// NewestFailedVerificationSystem returns the most recently signed
	// signature made as the deployment signer on the account (host, userID),
	// in any org, whose pushed commit GitHub last reported as not verified, or
	// (nil, nil) when there is none. Verify re-reads that commit with its
	// org's credential. Cross-org: the deployment signer serves every managed
	// workspace.
	NewestFailedVerificationSystem(ctx context.Context, host string, userID int64) (*domain.CommitSignature, error)

	// UsageSystem counts the signatures made as the deployment signer in
	// [since, until), by the hour they were signed in (UTC, oldest first) and
	// by org (most first, then by org id). A zero since or until leaves that
	// side open. Cross-org, for the fleet console.
	UsageSystem(ctx context.Context, since, until time.Time) (DeploymentSignerUsage, error)
}

// DeploymentSignerKeyCheck is one read of the deployment signer's account and
// its signing keys, as RecordKeyCheckSystem stores it.
type DeploymentSignerKeyCheck struct {
	// GitHubUserID, Login and Name are the account as GitHub answered for it.
	// A zero GitHubUserID is an account the check could not find, which keeps
	// what the row holds.
	GitHubUserID int64
	Login        string
	Name         string
	// KeyFound is whether the row's key is among the account's signing keys.
	KeyFound bool
}

// DeploymentKeyCheckTransition is the status a key check leaves the deployment
// signer in, from the status and reason it holds. Finding the key activates a
// pending signer as well as one broken for its absence: the operator adds the
// key to the account before configuring it, so there is no admin step between
// the two to wait for. Not finding it breaks an active signer (key_missing).
// A pending signer stays pending, and one broken by a pushed commit keeps that
// reason until a Verify.
func DeploymentKeyCheckTransition(status domain.CommitSignerStatus, reason string, keyFound bool) (domain.CommitSignerStatus, string) {
	switch {
	case keyFound && (status == domain.CommitSignerPending ||
		(status == domain.CommitSignerBroken && reason == domain.CommitSignerReasonKeyMissing)):
		return domain.CommitSignerActive, ""
	case !keyFound && status == domain.CommitSignerActive:
		return domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing
	}
	return status, reason
}

// DeploymentSignerUsage is UsageSystem's answer.
type DeploymentSignerUsage struct {
	Total  int
	ByHour []HourCount
	ByOrg  []OrgCount
}

// HourCount is the signatures made in the hour starting at Hour.
type HourCount struct {
	Hour  time.Time
	Count int
}

// OrgCount is the signatures made for one org.
type OrgCount struct {
	OrgID string
	Count int
}

// SortDeploymentSignerUsage puts u's cuts in the order UsageSystem promises:
// hours oldest first, orgs by most signatures and then by id.
func SortDeploymentSignerUsage(u *DeploymentSignerUsage) {
	slices.SortFunc(u.ByHour, func(a, b HourCount) int { return a.Hour.Compare(b.Hour) })
	slices.SortFunc(u.ByOrg, func(a, b OrgCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.OrgID, b.OrgID)
	})
}

// DeploymentCommitSignerColumns is the projection deployment_commit_signer is
// read through: the point read and every write's RETURNING.
const DeploymentCommitSignerColumns = `host, public_key, github_user_id, login, name, status, status_reason,
	status_since, checked_at, created_at, updated_at`

// ScanDeploymentCommitSigner decodes one row of DeploymentCommitSignerColumns.
func ScanDeploymentCommitSigner(scan func(...any) error) (domain.DeploymentCommitSigner, error) {
	var (
		out          domain.DeploymentCommitSigner
		userID       sql.NullInt64
		name, reason sql.NullString
		status       string
		checkedAt    sql.NullTime
	)
	if err := scan(&out.Host, &out.PublicKey, &userID, &out.Login, &name, &status, &reason,
		&out.StatusSince, &checkedAt, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return domain.DeploymentCommitSigner{}, err
	}
	out.GitHubUserID = userID.Int64
	out.Name = name.String
	out.Status = domain.CommitSignerStatus(status)
	out.StatusReason = reason.String
	out.StatusSince = out.StatusSince.UTC()
	out.CheckedAt = utcTimePtr(checkedAt)
	out.CreatedAt = out.CreatedAt.UTC()
	out.UpdatedAt = out.UpdatedAt.UTC()
	return out, nil
}

// ValidateDeploymentSignerKey is RegisterSystem's refusal of a row naming no
// host, key or login.
func ValidateDeploymentSignerKey(host, publicKey, login string) error {
	switch {
	case host == "":
		return fmt.Errorf("%w: no host", ErrInvalidOrgCommitSigner)
	case publicKey == "":
		return fmt.Errorf("%w: no public key", ErrInvalidOrgCommitSigner)
	case login == "":
		return fmt.Errorf("%w: no login", ErrInvalidOrgCommitSigner)
	}
	return nil
}
