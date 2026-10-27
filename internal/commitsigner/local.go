package commitsigner

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// modeLocal is the duration histogram's mode label for a signature a local
// run's daemon made in the TF process.
const modeLocal = "local"

// LocalRequest is one commit a local run's git asked to have signed. The org,
// conversation and engagement are the run's, bound by whatever serves the
// run's daemon; only Payload comes from git.
type LocalRequest struct {
	OrgID          string
	ConversationID string
	ClaimID        string
	BootEpoch      int64

	// Signer is the identity the run was started with: the user.name and
	// user.email its git config stamps, and the account and kind of signer it
	// signs as. A run's identity is fixed when it starts, so the committer is
	// checked against this one, and the org's signer now is compared with it.
	Signer Identity

	// Payload is the commit object git handed its signing program.
	Payload []byte
}

// LocalSigner signs a local run's commits in the TF process, with the org's
// key from the secret store. Local mode has no control plane: the run's
// daemon (agenthost.StartLocal, or the exec CLI itself with the local sandbox
// off) holds the commit and makes every check the two halves of a multi-mode
// signature share between them.
type LocalSigner struct {
	Stores  db.Stores
	Budgets Budgets

	// now replaces time.Now when set. Tests move the clock.
	now func() time.Time
}

func (l *LocalSigner) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Sign checks one commit and, when every check passes, signs it, records the
// signature with the commit id it produces, and returns both. In order:
//
//  1. The payload is a commit object, and its committer is the run's signer
//     (§4.3). A committer mismatch writes a commit_sign_denied row.
//  2. The engagement still holds its claim.
//  3. The org still resolves to the run's signer, and that signer is active.
//  4. The claim and the org are within their budgets.
//
// A refusal is the Result's, never an error: the caller turns it into the line
// its agent reads. A read that fails, a missing key or a signature that cannot
// be recorded answer OutcomeUnanswered, which tells the run to try again; a
// signature is never handed back unless its row is stored, because the
// budgets count rows.
func (l *LocalSigner) Sign(ctx context.Context, req LocalRequest) (res Result) {
	start := time.Now()
	ctx, span := tracer.Start(ctx, "commit_signing.sign", trace.WithAttributes(
		telemetry.OrgID(req.OrgID),
		telemetry.ConversationID(req.ConversationID),
		telemetry.Signer(string(req.Signer.Mode)),
		telemetry.SigningMode(modeLocal),
	))
	defer func() {
		span.SetAttributes(telemetry.Outcome(string(res.Outcome)))
		if res.Refusal != "" {
			span.SetAttributes(telemetry.Reason(string(res.Refusal)))
		}
		span.End()
		countRequest(req.Signer.Mode, req.OrgID, res.Outcome, res.Refusal)
		if res.Outcome == OutcomeSigned {
			observeDuration(req.Signer.Mode, modeLocal, time.Since(start))
		}
	}()

	if !req.Signer.Mode.Signs() {
		return refused(RefusalNotSigning)
	}
	accountID, err := strconv.ParseInt(req.Signer.AccountID, 10, 64)
	if err != nil || accountID <= 0 {
		return l.unanswered(ctx, req, "read the run's signer", errors.New("the run's signer has no account id"))
	}

	if res, ok, err := CheckCommit(req.Payload, req.Signer); err != nil {
		return l.unanswered(ctx, req, "read the commit", err)
	} else if !ok {
		if res.Refusal == RefusalCommitterMismatch {
			signerLog.WarnContext(ctx, "refused to sign a commit whose committer is not the run's signer",
				"org", req.OrgID, "conversation", req.ConversationID, "committer", formatIdent(res.Committer))
			recordDenied(ctx, l.Stores, denial{
				orgID:          req.OrgID,
				conversationID: req.ConversationID,
				signerHost:     req.Signer.Host,
				signerUserID:   accountID,
				reason:         deniedCommitterMismatch,
				committer:      formatIdent(res.Committer),
			})
		}
		return res
	}

	live, err := l.Stores.ConversationQueue.ClaimLiveSystem(ctx, req.OrgID, req.ClaimID, req.BootEpoch)
	if err != nil {
		return l.unanswered(ctx, req, "read the claim", err)
	}
	if !live {
		signerLog.WarnContext(ctx, "refused a signature for an engagement that no longer holds its claim",
			"org", req.OrgID, "conversation", req.ConversationID, "claim", req.ClaimID, "boot_epoch", req.BootEpoch)
		recordDenied(ctx, l.Stores, denial{
			orgID: req.OrgID, conversationID: req.ConversationID,
			signerHost: req.Signer.Host, signerUserID: accountID, reason: deniedClaimInactive,
		})
		return refused(RefusalClaimInactive)
	}

	now, err := Resolver{Signers: l.Stores.OrgCommitSigners}.Resolve(ctx, req.OrgID)
	if err != nil {
		return l.unanswered(ctx, req, "resolve the org's signer", err)
	}
	if refusal := signerRefusal(now, req.Signer.Mode, req.Signer.Host, req.Signer.AccountID); refusal != "" {
		res := refused(refusal)
		if refusal == RefusalSignerInactive {
			res.SignerStatus, res.SignerReason = now.Status, now.StatusReason
		}
		return res
	}

	at := l.clock()
	if res, ok, err := checkBudgets(ctx, l.Stores.CommitSignatures, l.Budgets, req.OrgID, req.ClaimID, at); err != nil {
		return l.unanswered(ctx, req, "count the signatures", err)
	} else if !ok {
		if res.Refusal == RefusalClaimBudget {
			signerLog.WarnContext(ctx, "refused a signature: the engagement spent its signing budget",
				"org", req.OrgID, "conversation", req.ConversationID, "claim", req.ClaimID, "limit", res.Limit)
			recordDenied(ctx, l.Stores, denial{
				orgID: req.OrgID, conversationID: req.ConversationID,
				signerHost: req.Signer.Host, signerUserID: accountID, reason: deniedBudgetExhausted,
			})
		}
		return res
	}

	key, err := signingKey(ctx, l.Stores.Secrets, req.OrgID, now.PublicKey)
	if err != nil {
		return l.unanswered(ctx, req, "read the signer's key", err)
	}
	sig, err := commitsign.Sign(key, req.Payload)
	if err != nil {
		return l.unanswered(ctx, req, "sign the commit", err)
	}
	sha := commitsign.CommitID(commitsign.SignedCommit(req.Payload, sig))
	row, err := l.Stores.CommitSignatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID:          req.OrgID,
		ConversationID: req.ConversationID,
		ClaimID:        req.ClaimID,
		BootEpoch:      req.BootEpoch,
		Signer:         now.Mode,
		SignerHost:     db.NormalizeGitHubHost(now.Host),
		SignerUserID:   accountID,
		PublicKey:      now.PublicKey,
		Digest:         commitsign.DigestOf(req.Payload).String(),
		State:          domain.CommitSignatureSigned,
		Signature:      string(sig),
		SignedAt:       &at,
		CommitSHA:      sha,
	})
	if err != nil {
		return l.unanswered(ctx, req, "record the signature", err)
	}
	return Result{Outcome: OutcomeSigned, SignatureID: row.ID, Signature: row.Signature, PublicKey: row.PublicKey, CommitSHA: sha}
}

// CheckCommit is the daemon's check on what git asked to have signed (spec
// §4.3), made by whatever holds the commit before any signer is asked: the
// payload is a commit object, and its committer is the run's signer, the
// identity the run was started with. The author is free, because a rebase or
// a cherry-pick re-signs commits that keep their original author; the
// committer's timestamp is free too.
//
// ok is true for a commit that may be signed. Otherwise the Result is the
// refusal: RefusalNotACommit with the object, or RefusalCommitterMismatch
// with the committer the commit named. An error is a payload that could not
// be read as an object at all.
func CheckCommit(payload []byte, signer Identity) (Result, bool, error) {
	committer, err := commitsign.ParseCommitter(payload)
	if err != nil {
		var nac *commitsign.NotACommitError
		if errors.As(err, &nac) {
			res := refused(RefusalNotACommit)
			res.Object = nac.Object
			return res, false, nil
		}
		return Result{}, false, err
	}
	if committer.Name != signer.Name || committer.Email != signer.Email {
		res := refused(RefusalCommitterMismatch)
		res.Committer = committer
		return res, false, nil
	}
	return Result{}, true, nil
}

// unanswered logs why a signature could not be made and answers the run to
// try again.
func (l *LocalSigner) unanswered(ctx context.Context, req LocalRequest, step string, err error) Result {
	signerLog.ErrorContext(ctx, "could not sign a local run's commit; answering it unanswered",
		"step", step, "org", req.OrgID, "conversation", req.ConversationID, "error", err)
	return Result{Outcome: OutcomeUnanswered}
}

func refused(r Refusal) Result {
	return Result{Outcome: OutcomeRefused, Refusal: r}
}

// checkBudgets counts the claim's and the org's signatures against their
// budgets at now. ok is true when both admit another; otherwise the Result is
// the refusal, with its limit and, for the org's, how long until the window
// admits one.
func checkBudgets(ctx context.Context, sigs db.CommitSignatureStore, b Budgets, orgID, claimID string, now time.Time) (Result, bool, error) {
	claimed, err := sigs.ClaimSignaturesSystem(ctx, orgID, claimID)
	if err != nil {
		return Result{}, false, err
	}
	if claimed >= b.PerClaim {
		res := refused(RefusalClaimBudget)
		res.Limit = b.PerClaim
		return res, false, nil
	}
	window, err := sigs.OrgSignaturesSinceSystem(ctx, orgID, now.Add(-OrgWindow))
	if err != nil {
		return Result{}, false, err
	}
	if window.Count >= b.PerOrgMinute {
		res := refused(RefusalOrgMinuteBudget)
		res.Limit, res.RetryAfterSeconds = b.PerOrgMinute, retryAfter(window.Oldest, now)
		return res, false, nil
	}
	return Result{}, true, nil
}

// formatIdent spells an identity the way a commit header does.
func formatIdent(id commitsign.Identity) string {
	return id.Name + " <" + id.Email + ">"
}
