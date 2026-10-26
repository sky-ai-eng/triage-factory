package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The org's commit signer: the GitHub user account its agents' commits are
// authored, committed and signed as, and the SSH key pair TF signs with
// (docs/for-agents/specs/commit-signing). TF generates the pair; an org admin
// adds the public half to the account as a signing key, and Verify finds it
// there before the signer is active.
//
// Every write takes githubAppRegMu, the lock every GitHub credential
// transition takes, because the signer is one: the org's GitHub host cannot
// move while it exists (githubCredentialConnected), and the registration
// checks the host it read is still the org's under the lock. A write that
// stores a key in local mode does so in the keychain, outside the
// transaction, so it snapshots the signing keys first and puts them back when
// the transaction fails (guardLocalSecretWrite).
//
// Every write records an access-log row under credential kind
// github_commit_signer: register, verify, rotate and an email change as
// credential_set with the change named, removal as credential_removed.

// commitSignerKeyTitle is the title the routes suggest for the key on the
// account. Key titles are public, so it names the product and nothing about
// the org.
const commitSignerKeyTitle = "Triage Factory commit signing"

// maxCommitEmailLen bounds a committer address at the longest an address can
// be.
const maxCommitEmailLen = 254

// commitSigningKeys are the secrets a signer owns: the private half of its key
// and of a rotation's pending key.
var commitSigningKeys = []string{integrations.KeyGitHubCommitSigningKey, integrations.KeyGitHubCommitSigningKeyPending}

// commitSignerJSON is the signer as the routes answer it.
type commitSignerJSON struct {
	// Host is the GitHub the account is on: the org's host when the signer
	// was registered.
	Host         string `json:"host"`
	GitHubUserID string `json:"github_user_id"`
	Login        string `json:"login"`
	// Name is the account's profile name, "" when it has none.
	Name string `json:"name"`
	// Email is the committer address the signer was given, null when its
	// commits carry the derived noreply address. PATCH writes it.
	Email *string `json:"email"`
	// CommitName and CommitEmail are what the signer's commits carry.
	CommitName  string `json:"commit_name"`
	CommitEmail string `json:"commit_email"`
	// PublicKey is the key TF signs with; PendingPublicKey is a rotation's
	// new key, null outside one. KeyTitle is the title to give whichever of
	// them is being added to the account.
	PublicKey        string  `json:"public_key"`
	PendingPublicKey *string `json:"pending_public_key"`
	KeyTitle         string  `json:"key_title"`
	Status           string  `json:"status"`
	// StatusReason says why a broken signer is broken: GitHub's verification
	// reason for a pushed commit, or key_missing.
	StatusReason *string   `json:"status_reason"`
	StatusSince  time.Time `json:"status_since"`
	// CheckedAt is when a check last found the key on the account.
	CheckedAt *time.Time `json:"checked_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// CreatedBy is the user who registered the signer, null once that user
	// is deleted.
	CreatedBy *string `json:"created_by"`
}

func toCommitSignerJSON(s domain.OrgCommitSigner) commitSignerJSON {
	return commitSignerJSON{
		Host:             s.Host,
		GitHubUserID:     strconv.FormatInt(s.GitHubUserID, 10),
		Login:            s.Login,
		Name:             s.Name,
		Email:            nilIfEmpty(s.EmailOverride),
		CommitName:       s.CommitName(),
		CommitEmail:      s.CommitEmail(),
		PublicKey:        s.PublicKey,
		PendingPublicKey: nilIfEmpty(s.PendingPublicKey),
		KeyTitle:         commitSignerKeyTitle,
		Status:           string(s.Status),
		StatusReason:     nilIfEmpty(s.StatusReason),
		StatusSince:      s.StatusSince,
		CheckedAt:        s.CheckedAt,
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
		CreatedBy:        nilIfEmpty(s.CreatedBy),
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// handleCommitSignerGet answers the org's signer, 404 when it has none.
//
// GET /api/orgs/{org_id}/github/commit-signer
func (s *Server) handleCommitSignerGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	var signer *domain.OrgCommitSigner
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var err error
		signer, err = tx.OrgCommitSigners.Get(r.Context(), orgID)
		return err
	}); err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	if signer == nil {
		notFound(w, "commit signer")
		return
	}
	writeJSON(w, http.StatusOK, toCommitSignerJSON(*signer))
}

// commitSignerCreateRequest is the registration body: the account's login,
// and the committer address its commits carry when they are not to carry the
// derived one. Absent and null both mean the derived address.
type commitSignerCreateRequest struct {
	Login string  `json:"login"`
	Email *string `json:"email"`
}

// errGitHubHostMovedDuringSigner refuses a registration whose account was
// resolved on a GitHub the workspace no longer has.
var errGitHubHostMovedDuringSigner = errors.New("this workspace's GitHub URL changed while the account was being checked — register the signer again")

// handleCommitSignerCreate registers the org's signer: it resolves the login
// on the org's GitHub, which must name a user account (a bot or an
// organization cannot hold a signing key) on a GitHub that serves SSH signing
// keys, generates the key pair, stores the private half as a secret, and
// creates the signer pending. The answer carries the public key and the title
// to add it to the account under; Verify then looks for it there.
//
// POST /api/orgs/{org_id}/github/commit-signer
func (s *Server) handleCommitSignerCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req commitSignerCreateRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	switch {
	case req.Login == "":
		v.Missing("login")
	case !gitHubLoginPattern.MatchString(req.Login):
		v.Invalid("login", "login is not a GitHub login")
	}
	email := ""
	if req.Email != nil {
		email = *req.Email
		if msg := commitEmailProblem(email); msg != "" {
			v.Invalid("email", msg)
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	host, ok := s.requireOrgGitHubHost(ctx, w, orgID, "commit-signer")
	if !ok {
		return
	}
	if email == "" && !domain.DerivesCommitEmail(host) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Field: "email",
			Message: "email is required on " + host + ": only github.com has a noreply address to commit under, so name an address verified on the account",
		})
		return
	}

	// Advisory, so a second registration is refused before a GitHub round
	// trip; Create's own conflict is the authoritative answer.
	if existing, err := s.allStores.OrgCommitSigners.GetSystem(ctx, orgID); err != nil {
		internalError(w, "commit-signer", err)
		return
	} else if existing != nil {
		writeCommitSignerExists(w, existing.Login)
		return
	}

	client, ok := s.commitSignerClient(w, ctx, orgID)
	if !ok {
		return
	}
	account, err := client.GetAccountByLogin(ctx, req.Login)
	if err != nil {
		if ghclient.IsHTTP404(err) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Field: "login",
				Message: fmt.Sprintf("no GitHub account is called %q on %s", req.Login, host),
			})
			return
		}
		writeUpstreamGitHub(w, "could not read the account from GitHub", err)
		return
	}
	if account.Type != ghclient.AccountTypeUser {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Field: "login",
			Message: fmt.Sprintf("%s is a GitHub %s account; only a user account can hold a signing key", account.Login, strings.ToLower(account.Type)),
		})
		return
	}
	accountID, err := strconv.ParseInt(account.ID, 10, 64)
	if err != nil {
		writeUpstreamGitHub(w, "GitHub answered the account with an id that is not a number", err)
		return
	}
	// The listing is fetched only to learn the GitHub serves it: the key
	// cannot be there yet. A 404 for an account that exists is a GitHub
	// without SSH signing, which could verify nothing TF signs.
	if _, err := client.ListSSHSigningKeys(ctx, account.Login); err != nil {
		if ghclient.IsHTTP404(err) {
			writeSSHSigningUnsupported(w, host)
			return
		}
		writeUpstreamGitHub(w, "could not read the account's signing keys from GitHub", err)
		return
	}

	privateKey, publicKey, err := newCommitSigningKey()
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}

	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer release()
	restore, unlock, err := s.guardLocalSecretWrite(ctx, nil, orgID, commitSigningKeys...)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer unlock()

	var created domain.OrgCommitSigner
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// The account was resolved on host before the lock was taken, and a
		// settings save could have moved the URL in that window. Under the
		// lock it cannot move again, so this read is the one that holds.
		set, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if db.EffectiveGitHubHost(set.GitHubBaseURL) != host {
			return errGitHubHostMovedDuringSigner
		}
		created, err = tx.OrgCommitSigners.Create(ctx, domain.OrgCommitSigner{
			OrgID:         orgID,
			Host:          host,
			GitHubUserID:  accountID,
			Login:         account.Login,
			Name:          account.Name,
			EmailOverride: email,
			PublicKey:     publicKey,
			CreatedBy:     userID,
		})
		if err != nil {
			return err
		}
		if err := tx.Secrets.Put(ctx, orgID, integrations.KeyGitHubCommitSigningKey, privateKey, "Commit signing key"); err != nil {
			return fmt.Errorf("store signing key: %w", err)
		}
		// A new signer has no rotation under way, whatever an earlier
		// signer's failed teardown may have left behind.
		if _, err := tx.Secrets.Delete(ctx, orgID, integrations.KeyGitHubCommitSigningKeyPending); err != nil {
			return fmt.Errorf("clear pending signing key: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCommitSigner(commitSignerRegistered, host, account.Login, publicKey, email),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		switch {
		case errors.Is(err, db.ErrOrgCommitSignerExists):
			writeCommitSignerExists(w, "")
		case errors.Is(err, errGitHubHostMovedDuringSigner):
			conflict(w, err.Error())
		default:
			internalError(w, "commit-signer", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, toCommitSignerJSON(created))
}

// writeCommitSignerExists answers a registration for an org that has a
// signer: one per org, and replacing it is a removal and a new registration.
func writeCommitSignerExists(w http.ResponseWriter, login string) {
	msg := "this workspace already has a commit signer"
	if login != "" {
		msg += ", " + login
	}
	conflict(w, msg+"; remove it before registering another")
}

// writeSSHSigningUnsupported answers a GitHub that does not serve the signing
// key listing for an account that exists: TF detects the feature rather than
// checking versions, the way it treats a GHES without hook deliveries.
func writeSSHSigningUnsupported(w http.ResponseWriter, host string) {
	httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
		Reason:  httpx.ReasonSSHSigningUnsupported,
		Message: host + " does not serve SSH signing keys, so it cannot verify a commit TF signs; commit signing needs a GitHub that supports SSH commit signature verification",
	})
}

// commitEmailProblem is why email cannot be a committer address, "" when it
// can: a bare address, as git writes it between the angle brackets of the
// committer header, with no display name, surrounding space or brackets, and
// one git writes unchanged. A bare address can begin with an apostrophe, which
// git drops, and the commit would then name an address the signer never set.
func commitEmailProblem(email string) string {
	if email == "" {
		return "email must not be empty; send null to commit under the derived noreply address"
	}
	if len(email) > maxCommitEmailLen {
		return fmt.Sprintf("email must be at most %d characters", maxCommitEmailLen)
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Name != "" || addr.Address != email {
		return "email must be a bare email address, such as agent@example.com"
	}
	if commitsign.IdentName(email) != email {
		return "email must not begin with an apostrophe, which git drops from a committer address"
	}
	return ""
}

// commitSignerClient resolves the org's GitHub client for the signer's reads,
// writing the answer when it cannot: 409 NOT_CONFIGURED for an org with no
// GitHub credential, 500 otherwise.
func (s *Server) commitSignerClient(w http.ResponseWriter, ctx context.Context, orgID string) (*ghclient.Client, bool) {
	client, err := s.gitHubAccountClient(ctx, orgID)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeNotConfigured(w, "GitHub is not connected for this workspace")
			return nil, false
		}
		internalError(w, "commit-signer", err)
		return nil, false
	}
	return client, true
}

// newCommitSigningKey generates a signing key pair and returns the private
// half in the OpenSSH form the secret holds and the public half as GitHub
// lists it.
func newCommitSigningKey() (privateKey, publicKey string, err error) {
	key, err := commitsign.GenerateKey()
	if err != nil {
		return "", "", fmt.Errorf("generate signing key: %w", err)
	}
	pem, err := commitsign.MarshalPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encode signing key: %w", err)
	}
	pub, err := commitsign.PublicKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encode public key: %w", err)
	}
	return string(pem), pub, nil
}

// signerCheckFault is a Verify that did not pass, as the response it owes.
type signerCheckFault struct {
	status int
	item   httpx.ErrorItem
}

func upstreamFault(msg string, err error) *signerCheckFault {
	return &signerCheckFault{status: http.StatusBadGateway, item: httpx.ErrorItem{
		Reason: httpx.ReasonUpstreamUnavailable, Message: msg + httpx.LocalDetail(err),
	}}
}

// handleCommitSignerVerify checks the signer against GitHub and, when it
// passes, makes it active. The key it looks for is a rotation's pending key
// while one is under way, else the signer's own; finding the pending key
// promotes it, and the old private half is destroyed.
//
// POST /api/orgs/{org_id}/github/commit-signer/verify
func (s *Server) handleCommitSignerVerify(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	signer, err := s.allStores.OrgCommitSigners.GetSystem(ctx, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	if signer == nil {
		notFound(w, "commit signer")
		return
	}
	key := signer.PendingPublicKey
	if key == "" {
		key = signer.PublicKey
	}
	client, ok := s.commitSignerClient(w, ctx, orgID)
	if !ok {
		return
	}
	result, fault, err := s.checkCommitSigner(ctx, client, orgID, *signer, key)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	commitsigner.CountCheck(ctx, commitsigner.CheckVerify, result)
	if fault != nil {
		httpx.WriteErrors(w, fault.status, fault.item)
		return
	}

	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer release()
	restore, unlock, err := s.guardLocalSecretWrite(ctx, nil, orgID, commitSigningKeys...)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer unlock()

	var updated domain.OrgCommitSigner
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		cur, err := tx.OrgCommitSigners.Get(ctx, orgID)
		if err != nil {
			return err
		}
		if cur == nil {
			return db.ErrNoSuchOrgCommitSigner
		}
		promotes := cur.PendingPublicKey == key
		if updated, err = tx.OrgCommitSigners.Activate(ctx, orgID, key); err != nil {
			return err
		}
		if promotes {
			if err := promotePendingSigningKey(ctx, tx, orgID); err != nil {
				return err
			}
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCommitSigner(commitSignerVerified, updated.Host, updated.Login, key, ""),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		switch {
		case errors.Is(err, db.ErrNoSuchOrgCommitSigner):
			notFound(w, "commit signer")
		case errors.Is(err, db.ErrCommitSignerKeyChanged):
			conflict(w, "the signer's key was rotated while it was being verified; add the new key to the account and verify again")
		default:
			internalError(w, "commit-signer", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, toCommitSignerJSON(updated))
}

// promotePendingSigningKey makes a rotation's private half the signer's and
// destroys the one it replaces.
func promotePendingSigningKey(ctx context.Context, tx db.TxStores, orgID string) error {
	pending, err := tx.Secrets.Get(ctx, orgID, integrations.KeyGitHubCommitSigningKeyPending)
	if err != nil {
		return fmt.Errorf("read pending signing key: %w", err)
	}
	if pending == "" {
		return errors.New("the signer's pending key has no private half stored")
	}
	if err := tx.Secrets.Put(ctx, orgID, integrations.KeyGitHubCommitSigningKey, pending, "Commit signing key"); err != nil {
		return fmt.Errorf("store signing key: %w", err)
	}
	if _, err := tx.Secrets.Delete(ctx, orgID, integrations.KeyGitHubCommitSigningKeyPending); err != nil {
		return fmt.Errorf("clear pending signing key: %w", err)
	}
	return nil
}

// checkCommitSigner is Verify's reading of GitHub: whether key is among the
// signing keys of the signer's account, and, for a signer the post-push check
// broke, whether the commit that broke it verifies now. It returns the check's
// result for tf_commit_signer_checks_total and the fault a check that did not
// pass answers with; err is a failure of TF's own, which is neither.
//
// The account is read by its id, so a login renamed since registration is
// read under its new name, and a login someone else has since claimed is
// never read at all.
//
// The failing commit is re-read only when it can still say something about
// the signer as it is now. GitHub evaluates a failing result again on every
// read, so it reads valid once its fault is fixed: the key is back on the
// account, or the address is verified on it. A commit signed with a key other
// than the one being verified, or committed under an address the signer no
// longer commits as, fails for a fault the rotation or the email change has
// already replaced, and holding the signer on it would keep it broken for
// good; the next push is checked instead. So is a commit GitHub no longer
// has.
func (s *Server) checkCommitSigner(ctx context.Context, client *ghclient.Client, orgID string, signer domain.OrgCommitSigner, key string) (string, *signerCheckFault, error) {
	account, err := client.GetAccountByID(ctx, strconv.FormatInt(signer.GitHubUserID, 10))
	if err != nil {
		if ghclient.IsHTTP404(err) {
			return commitsigner.CheckResultKeyMissing, &signerCheckFault{status: http.StatusConflict, item: httpx.ErrorItem{
				Reason:  httpx.ReasonKeyNotFound,
				Message: fmt.Sprintf("GitHub has no account with id %d, the account %s was registered as; remove the signer and register the account that should sign", signer.GitHubUserID, signer.Login),
			}}, nil
		}
		return commitsigner.CheckResultError, upstreamFault("could not read the signer's account from GitHub", err), nil
	}
	keys, err := client.ListSSHSigningKeys(ctx, account.Login)
	if err != nil {
		if ghclient.IsHTTP404(err) {
			return commitsigner.CheckResultError, &signerCheckFault{status: http.StatusUnprocessableEntity, item: httpx.ErrorItem{
				Reason:  httpx.ReasonSSHSigningUnsupported,
				Message: signer.Host + " does not serve SSH signing keys, so it cannot verify a commit TF signs",
			}}, nil
		}
		return commitsigner.CheckResultError, upstreamFault("could not read the account's signing keys from GitHub", err), nil
	}
	if !slices.Contains(keys, key) {
		which := "the signer's key"
		if key == signer.PendingPublicKey {
			which = "the new key"
		}
		return commitsigner.CheckResultKeyMissing, &signerCheckFault{status: http.StatusConflict, item: httpx.ErrorItem{
			Reason: httpx.ReasonKeyNotFound,
			Message: fmt.Sprintf("%s is not among %s's signing keys on GitHub; add it in Settings → SSH and GPG keys → New SSH key with key type Signing key, then verify again. GitHub's listing can take a minute to show a key just added",
				which, account.Login),
		}}, nil
	}

	if signer.Status != domain.CommitSignerBroken || signer.StatusReason == "" || signer.StatusReason == domain.CommitSignerReasonKeyMissing {
		return domain.CommitVerificationValid, nil, nil
	}
	failed, err := s.allStores.CommitSignatures.NewestFailedVerificationSystem(ctx, orgID, signer.Host, signer.GitHubUserID)
	if err != nil {
		return "", nil, fmt.Errorf("read the signer's failing commit: %w", err)
	}
	if failed == nil || failed.PublicKey != key || failed.CommitSHA == "" || failed.RepositoryID == "" {
		return domain.CommitVerificationValid, nil, nil
	}
	repo, err := s.allStores.Repos.GetSystem(ctx, orgID, failed.RepositoryID)
	if err != nil {
		return "", nil, fmt.Errorf("read the failing commit's repository: %w", err)
	}
	if repo == nil {
		return domain.CommitVerificationValid, nil, nil
	}
	repoClient, err := ghclient.ClientForOwner(ctx, s.ghResolver, orgID, repo.OwnerExternalID, repo.Owner)
	if err != nil {
		return commitsigner.CheckResultError, upstreamFault("could not reach "+repo.Slug()+" on GitHub", err), nil
	}
	verdict, err := repoClient.GetCommitVerification(ctx, repo.Owner, repo.Repo, failed.CommitSHA)
	if err != nil {
		if ghclient.IsHTTP404(err) {
			return domain.CommitVerificationValid, nil, nil
		}
		return commitsigner.CheckResultError, upstreamFault("could not re-read commit "+failed.CommitSHA+" from GitHub", err), nil
	}
	if verdict.Verified || !strings.EqualFold(verdict.CommitterEmail, signer.CommitEmail()) {
		return domain.CommitVerificationValid, nil, nil
	}
	return verdict.Reason, &signerCheckFault{status: http.StatusConflict, item: httpx.ErrorItem{
		Reason: httpx.ReasonCommitUnverified,
		Message: fmt.Sprintf("GitHub still reports commit %s in %s as not verified (%s): %s",
			failed.CommitSHA, repo.Slug(), verdict.Reason, unverifiedCommitAdvice(verdict.Reason, verdict.CommitterEmail, account.Login)),
	}}, nil
}

// unverifiedCommitAdvice says what to do about the verification reasons an
// admin can fix on the account.
func unverifiedCommitAdvice(reason, email, login string) string {
	switch reason {
	case "unknown_key":
		return "the key that signed it is not on " + login + "'s signing keys"
	case "no_user":
		return email + " belongs to no GitHub account; add and verify it on " + login + ", or change the signer's email"
	case "unverified_email":
		return email + " is not verified on " + login + "; verify it there, then verify the signer again"
	default:
		return "GitHub gave this reason for the commit's signature; fix it on " + login + ", then verify again"
	}
}

// handleCommitSignerRotate starts a key rotation: it generates a new key pair
// and stores it as the signer's pending key. The current key keeps signing
// until Verify finds the new one on the account, which then replaces it. A
// second rotation before then replaces the pending key.
//
// POST /api/orgs/{org_id}/github/commit-signer/rotate
func (s *Server) handleCommitSignerRotate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	privateKey, publicKey, err := newCommitSigningKey()
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer release()
	restore, unlock, err := s.guardLocalSecretWrite(ctx, nil, orgID, integrations.KeyGitHubCommitSigningKeyPending)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer unlock()

	var updated domain.OrgCommitSigner
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if updated, err = tx.OrgCommitSigners.StagePendingKey(ctx, orgID, publicKey); err != nil {
			return err
		}
		if err := tx.Secrets.Put(ctx, orgID, integrations.KeyGitHubCommitSigningKeyPending, privateKey, "Pending commit signing key"); err != nil {
			return fmt.Errorf("store pending signing key: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCommitSigner(commitSignerRotated, updated.Host, updated.Login, publicKey, ""),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		if errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			notFound(w, "commit signer")
			return
		}
		internalError(w, "commit-signer", err)
		return
	}
	writeJSON(w, http.StatusOK, toCommitSignerJSON(updated))
}

// commitSignerPatch is the PATCH body: the committer address, null restoring
// the derived one.
type commitSignerPatch struct {
	Email json.RawMessage `json:"email"`
}

// errCommitEmailNotDerivable refuses clearing the address of a signer whose
// host has no noreply address to derive in its place.
var errCommitEmailNotDerivable = errors.New("only github.com has a noreply address to commit under")

// handleCommitSignerPatch changes the committer address the signer's commits
// carry. TF cannot read another account's verified addresses, so a wrong one
// shows up when a pushed commit fails GitHub's check, and Verify confirms the
// fix.
//
// PATCH /api/orgs/{org_id}/github/commit-signer
func (s *Server) handleCommitSignerPatch(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req commitSignerPatch
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if !httpx.PatchNamed(req.Email) {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: name email (null restores the derived noreply address)",
		})
		return
	}
	var v httpx.Validation
	email, state := httpx.PatchString(&v, req.Email, "email")
	if state == httpx.PatchSet {
		if msg := commitEmailProblem(email); msg != "" {
			v.Invalid("email", msg)
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer release()

	var updated domain.OrgCommitSigner
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		cur, err := tx.OrgCommitSigners.Get(ctx, orgID)
		if err != nil {
			return err
		}
		if cur == nil {
			return db.ErrNoSuchOrgCommitSigner
		}
		if email == "" && !domain.DerivesCommitEmail(cur.Host) {
			return errCommitEmailNotDerivable
		}
		if cur.EmailOverride == email {
			updated = *cur
			return nil
		}
		if updated, err = tx.OrgCommitSigners.SetEmailOverride(ctx, orgID, email); err != nil {
			return err
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCommitSigner(commitSignerEmailChanged, updated.Host, updated.Login, "", email),
		})
	}); err != nil {
		switch {
		case errors.Is(err, db.ErrNoSuchOrgCommitSigner):
			notFound(w, "commit signer")
		case errors.Is(err, errCommitEmailNotDerivable):
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Field: "email",
				Message: "this signer's GitHub needs an address of its own: " + err.Error(),
			})
		default:
			internalError(w, "commit-signer", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, toCommitSignerJSON(updated))
}

// handleCommitSignerDelete removes the signer and both of its private keys.
// The public keys stay on the account until someone removes them there:
// deleting one changes no commit GitHub has already verified.
//
// The PAT unbind and the App disconnect keep the org's GitHub host while a
// signer exists, so when nothing else is connected on it, this removal is the
// one that leaves the workspace with no GitHub, and it clears the host as they
// would have. Either order of the two removals ends in the same state.
//
// DELETE /api/orgs/{org_id}/github/commit-signer
func (s *Server) handleCommitSignerDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer release()
	restore, unlock, err := s.guardLocalSecretWrite(ctx, nil, orgID, commitSigningKeys...)
	if err != nil {
		internalError(w, "commit-signer", err)
		return
	}
	defer unlock()

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		cur, err := tx.OrgCommitSigners.Get(ctx, orgID)
		if err != nil {
			return err
		}
		if cur == nil {
			return db.ErrNoSuchOrgCommitSigner
		}
		if err := tx.OrgCommitSigners.Delete(ctx, orgID); err != nil {
			return err
		}
		for _, key := range commitSigningKeys {
			if _, err := tx.Secrets.Delete(ctx, orgID, key); err != nil {
				return fmt.Errorf("delete %s: %w", key, err)
			}
		}
		if err := clearGitHubHostIfUnheld(ctx, tx, orgID); err != nil {
			return err
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCommitSigner("", cur.Host, cur.Login, "", ""),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		if errors.Is(err, db.ErrNoSuchOrgCommitSigner) {
			notFound(w, "commit signer")
			return
		}
		internalError(w, "commit-signer", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearGitHubHostIfUnheld clears the org's GitHub host when no GitHub
// credential is connected on it. The caller holds githubAppRegMu and has
// already removed what it is removing in tx, so the read sees the org as the
// write leaves it.
func clearGitHubHostIfUnheld(ctx context.Context, tx db.TxStores, orgID string) error {
	set, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return fmt.Errorf("load org settings: %w", err)
	}
	host := db.EffectiveGitHubHost(set.GitHubBaseURL)
	if host == "" {
		return nil
	}
	connected, err := githubCredentialConnected(ctx, tx, orgID, host)
	if err != nil {
		return err
	}
	if connected != "" {
		return nil
	}
	if _, err := tx.Orgs.SetSourceBaseURL(ctx, orgID, eventsource.KindGitHub, ""); err != nil {
		return fmt.Errorf("clear github base url: %w", err)
	}
	return nil
}

// commitIdentityJSON is the identity an org's runs commit as, and how they
// sign.
type commitIdentityJSON struct {
	// Identity is the author and committer of every commit, null when the
	// org has none and its runs commit under the agent's own git config.
	Identity *commitIdentityAccountJSON `json:"identity"`
	// Signing is how the commits are signed: none, org (the org's own
	// signer) or deployment (the deployment's signer).
	Signing string `json:"signing"`
	// UnsignedReason says why an org whose signer is active still commits
	// unsigned, null when it does not: git_too_old (local mode, this host's
	// git predates SSH signing, 2.34) or git_unavailable (local mode, git
	// could not be run). GitVersion is the version the reason was read from,
	// null without one.
	UnsignedReason *string `json:"unsigned_reason"`
	GitVersion     *string `json:"git_version"`
}

type commitIdentityAccountJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	// GitHubUserID is the account the identity belongs to, null when it is
	// not known.
	GitHubUserID *string `json:"github_user_id"`
}

// handleCommitIdentityGet answers who this org's runs commit as and whether
// and how they sign, from the same resolution a run's commits take.
//
// GET /api/orgs/{org_id}/github/commit-identity
func (s *Server) handleCommitIdentityGet(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	id, err := commitsigner.Resolver{Signers: s.allStores.OrgCommitSigners, GitHub: s.ghResolver}.Resolve(r.Context(), orgID)
	if err != nil {
		internalError(w, "commit-identity", err)
		return
	}
	out := commitIdentityJSON{Signing: string(id.Mode)}
	// A local run signs with this host's git, which signs with an SSH key
	// only from 2.34; below that the run commits as the signer, unsigned.
	// Multi mode's runs use the jail image's git, well past it.
	if id.Mode.Signs() && runmode.Current() == runmode.ModeLocal {
		if git := commitsigner.ProbeLocalGit(r.Context()); git.UnsignedReason != "" {
			out.Signing = string(domain.CommitSigningNone)
			out.UnsignedReason = &git.UnsignedReason
			out.GitVersion = nilIfEmpty(git.Version)
		}
	}
	if id.Name != "" {
		out.Identity = &commitIdentityAccountJSON{Name: id.Name, Email: id.Email, GitHubUserID: nilIfEmpty(id.AccountID)}
	}
	writeJSON(w, http.StatusOK, out)
}
