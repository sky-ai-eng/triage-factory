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
// identity an org without a signer commits as; nil resolves none.
type Resolver struct {
	Signers db.OrgCommitSignerStore
	GitHub  ghclient.Resolver
}

// Resolve returns orgID's commit identity. In order, it is:
//
//   - the org's signer, in whatever status: the identity does not change with
//     the key's health, so a pending or broken signer still authors the
//     commits, unsigned. Only an active one signs.
//   - the identity the org commits as without a signer: the App's bot or the
//     PAT's owner, unsigned.
//
// An error is a failed read of the signer, which the caller must not answer
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
	// TODO(TFAC-1200): an org on the deployment App with no signer of its own
	// resolves to the deployment signer while that signer is active, which
	// reads the org's credential class and the deployment signer's status.
	// Until then every org without a signer commits as it does today,
	// unsigned.
	return r.withoutSigner(ctx, orgID), nil
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
		Name:      name,
		Email:     email,
		AccountID: strconv.FormatInt(s.GitHubUserID, 10),
		Login:     s.Login,
		Mode:      domain.CommitSigningNone,
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
