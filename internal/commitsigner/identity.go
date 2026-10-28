// Package commitsigner decides who a delegated run's commits are authored and
// committed as, and whether and how they are signed, and holds the operator
// limits on how many signatures TF makes.
//
// It also holds multi mode's signing queue, whose two halves never share a
// process: an executor's Requester queues a sandboxed run's digest and waits
// for the answer, and every control pod's ControlPlane claims, checks and
// signs it with a key no executor holds.
package commitsigner

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var signerLog = logging.Component("commitsigner")

// Identity is a run's commit identity, the author and committer of every
// commit it makes, and how those commits are signed.
//
// Name is empty when no identity resolves; the caller then stamps none, as
// it does for an org with no GitHub identity today.
type Identity struct {
	Name  string
	Email string
	// AccountID is GitHub's id for the account Name and Email belong to, in
	// text, "" when it is not known. The manual-run co-author trailer
	// compares the delegating person against it.
	AccountID string
	// Login is the signer's GitHub login when the identity is a signer's,
	// which is what GitHub shows its commits under; "" otherwise.
	Login string

	// Mode is how the run's commits are signed. It is never empty.
	Mode domain.CommitSigningMode

	// Status and StatusReason are the signer's health when the identity is
	// a signer's, in any status, and empty otherwise: a refusal because the
	// signer is not active says why in a sentence (signerOffReason).
	Status       domain.CommitSignerStatus
	StatusReason string

	// Host, PublicKey and PendingPublicKey describe the signer the run signs
	// as, and are set only when Mode signs: the GitHub its account is on, the
	// key that signs, and a rotation's new key, which a run that started
	// before the rotation finishes still has to be able to check its later
	// commits against.
	Host             string
	PublicKey        string
	PendingPublicKey string
}

// Resolver resolves a run's Identity. Signers is read on the admin pool, so it
// answers for callers with no request claims: credprovision sealing a bundle,
// the spawner starting a local run, and every signature, which resolves again
// rather than trusting what the run started with. GitHub resolves the
// identity an org without a signer commits as; nil resolves none. Deployment
// resolves an org on the deployment App to the deployment's signer; nil,
// as in local mode, on an executor and on a control pod configured with none,
// resolves no deployment arm.
type Resolver struct {
	Signers    db.OrgCommitSignerStore
	GitHub     ghclient.Resolver
	Deployment *DeploymentArm
}

// DeploymentArm is what resolving the deployment signer reads: this control
// pod's configured signer, the store its health is kept in, and the org
// settings that say whether an org is on the deployment App.
type DeploymentArm struct {
	Signer *DeploymentSigner
	Health db.DeploymentCommitSignerStore
	Orgs   db.OrgsStore
}

// Resolve returns orgID's commit identity. In order, it is:
//
//   - the org's signer, in whatever status: the identity does not change with
//     the key's health, so a pending or broken signer still authors the
//     commits, unsigned. Only an active one signs. An org's own signer always
//     wins, and one that cannot sign never falls back to the deployment's.
//   - for an org on the deployment App, the deployment's signer, by the
//     same rule: once a check has read its account it authors the commits
//     in any status, and signs while it is active.
//   - the identity the org commits as without a signer: the App's bot or the
//     PAT's owner, unsigned.
//
// An error is a failed read of a signer, which the caller must not answer
// with the identity a signer would have replaced.
func (r Resolver) Resolve(ctx context.Context, orgID string) (Identity, error) {
	signer, err := r.Signers.GetSystem(ctx, orgID)
	if err != nil {
		return Identity{}, fmt.Errorf("read the org's commit signer: %w", err)
	}
	if signer != nil {
		if id, ok := signerIdentity(*signer); ok {
			return id, nil
		}
		// Off github.com there is no noreply address to derive, so a signer
		// there needs an address of its own. Without one its account cannot
		// author a commit, and the org commits as it would without a signer.
		signerLog.Warn("commit signer has no committer address; committing as the org's GitHub identity",
			"org", orgID, "host", signer.Host, "login", signer.Login)
		return r.withoutSigner(ctx, orgID), nil
	}
	id, ok, err := r.Deployment.identity(ctx, orgID)
	if err != nil {
		return Identity{}, err
	}
	if ok {
		return id, nil
	}
	return r.withoutSigner(ctx, orgID), nil
}

// identity is the deployment signer's identity for orgID, or false when the
// org does not resolve to it: no deployment signer is configured, the org is
// not on the deployment App, its GitHub is not the one the signer's account is
// on, or no check has read the account yet, so there is no account to commit
// as. An error is a failed read of the org or of the signer's health.
func (d *DeploymentArm) identity(ctx context.Context, orgID string) (Identity, bool, error) {
	if d == nil || !d.Signer.Configured() {
		return Identity{}, false, nil
	}
	set, err := d.Orgs.GetSettingsSystem(ctx, orgID)
	if err != nil {
		return Identity{}, false, fmt.Errorf("read the org's GitHub credential class: %w", err)
	}
	if set.GitHubCredentialClass != domain.GitHubCredentialClassManagedApp ||
		db.NormalizeGitHubHost(set.GitHubBaseURL) != db.NormalizeGitHubHost(d.Signer.Host) {
		return Identity{}, false, nil
	}
	row, err := d.Health.GetSystem(ctx, d.Signer.Host, d.Signer.PublicKey)
	if err != nil {
		return Identity{}, false, fmt.Errorf("read the deployment commit signer: %w", err)
	}
	if row == nil || row.GitHubUserID <= 0 {
		return Identity{}, false, nil
	}
	name := commitsign.IdentName(row.CommitName())
	if name == "" {
		name = row.Login
	}
	id := Identity{
		Name:         name,
		Email:        d.Signer.CommitEmail(*row),
		AccountID:    strconv.FormatInt(row.GitHubUserID, 10),
		Login:        row.Login,
		Mode:         domain.CommitSigningNone,
		Status:       row.Status,
		StatusReason: row.StatusReason,
	}
	if row.Status == domain.CommitSignerActive {
		id.Mode = domain.CommitSigningDeployment
		id.Host = row.Host
		id.PublicKey = row.PublicKey
	}
	return id, true, nil
}

// signerIdentity is the identity an org's signer gives its runs, or false when
// the signer has no committer address: it is off github.com, where there is
// no noreply address to derive, and has no override.
//
// The name is the one git writes into the committer header for it
// (commitsign.IdentName), so the user.name a run is given and the committer
// every signature is checked against are the same string. A profile name git
// would reduce to nothing falls back to the login, which has no characters git
// strips.
func signerIdentity(s domain.OrgCommitSigner) (Identity, bool) {
	email := s.CommitEmail()
	if email == "" {
		return Identity{}, false
	}
	name := commitsign.IdentName(s.CommitName())
	if name == "" {
		name = s.Login
	}
	id := Identity{
		Name:         name,
		Email:        email,
		AccountID:    strconv.FormatInt(s.GitHubUserID, 10),
		Login:        s.Login,
		Mode:         domain.CommitSigningNone,
		Status:       s.Status,
		StatusReason: s.StatusReason,
	}
	if s.Status == domain.CommitSignerActive {
		id.Mode = domain.CommitSigningOrg
		id.Host = s.Host
		id.PublicKey = s.PublicKey
		id.PendingPublicKey = s.PendingPublicKey
	}
	return id, true
}

// withoutSigner is the identity orgID commits as with no signer, unsigned.
func (r Resolver) withoutSigner(ctx context.Context, orgID string) Identity {
	id := Identity{Mode: domain.CommitSigningNone}
	if r.GitHub == nil {
		return id
	}
	org, ok := ghclient.OrgIdentityAccount(ctx, r.GitHub, orgID)
	if !ok {
		return id
	}
	id.Name, id.Email, id.AccountID = org.Name, org.Email, org.AccountID
	return id
}
