package commitsigner

import (
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner/gitsig"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// linePrefix starts every line a run's signing program prints. The agent sees
// it under git's "error: gpg failed to sign the data", and it names who is
// speaking.
const linePrefix = "triagefactory: "

// RefusalLine is the one line a run's signing program prints when a signature
// is not made, for the result res of a request made as signer, the identity
// the run was started with. Git aborts the commit and the agent reads this
// line and nothing else, so each says what happened, that nothing was
// committed, and the exact next step. It is "" for a signed result.
//
// A reason is always a sentence, never a code: the agent cannot look a code up.
//
// The lines that mention a rebase follow what git does when a rebase's commit
// cannot be signed. A commit that fails while the rebase picks it leaves its
// changes staged. Before git 2.56 the pick is rescheduled, and git rebase
// --continue then refuses until they are reset; the rescheduled pick applies
// them again. From 2.56 the rebase stops on the pick as on a conflict, and
// --continue commits them, so the reset is conditional on the refusal. A
// commit that fails inside git rebase --continue, after a conflict was
// resolved, is not rescheduled, and the next --continue commits the
// resolution, which a reset would throw away. And a rebase keeps signing
// whatever -c commit.gpgsign=false says once it has started, so a run whose
// signing is off aborts it and runs it again.
func RefusalLine(res Result, signer Identity) string {
	switch res.Outcome {
	case OutcomeSigned:
		return ""
	case OutcomeUnanswered:
		return UnansweredLine()
	}
	switch res.Refusal {
	case RefusalCommitterMismatch:
		return linePrefix + fmt.Sprintf("this run signs commits only as %s, and this commit's committer is %s. "+
			"Nothing was committed. Run the command again without setting the committer: "+
			"no GIT_COMMITTER_NAME or GIT_COMMITTER_EMAIL, and no -c user.name or -c user.email. --author is fine.",
			formatIdent(gitsig.Identity{Name: signer.Name, Email: signer.Email}), formatIdent(res.Committer))
	case RefusalNotACommit:
		return linePrefix + fmt.Sprintf("this run signs commits only, and this is %s. "+
			"Nothing was created. Create it without signing; for a tag, drop -s.", objectPhrase(res.Object))
	case RefusalOrgMinuteBudget:
		wait := strconv.Itoa(max(res.RetryAfterSeconds, 1)) + "s"
		return linePrefix + fmt.Sprintf("this org has made %d signed commits in the last minute, its limit, so signing resumes in %s. "+
			"Nothing was committed. Wait %s, then run the same git command again. %s", res.Limit, wait, wait, rebaseResumes)
	case RefusalClaimBudget:
		return linePrefix + fmt.Sprintf("this run has made %d signed commits, the most one run may make, "+
			"so signing is off for the rest of this run. Nothing was committed. "+
			"Run this and every later commit with git -c commit.gpgsign=false. %s", res.Limit, rebaseRestarts)
	case RefusalSignerRemoved, RefusalSignerChanged, RefusalSignerInactive:
		return linePrefix + fmt.Sprintf("commit signing is off for the rest of this run: %s. Nothing was committed. "+
			"Run this and every later commit with git -c commit.gpgsign=false. %s Commits are still authored as %s.",
			signerOffReason(res, signer.Mode), rebaseRestarts, signer.Name)
	case RefusalClaimInactive:
		return linePrefix + "this run no longer holds its engagement, so commit signing refused. Nothing was committed."
	case RefusalNotSigning:
		return linePrefix + "this run does not sign commits. Nothing was committed. " +
			"Run the command again with git -c commit.gpgsign=false."
	}
	return UnansweredLine()
}

// UnansweredLine is the line for a signature nothing answered in time, and for
// any failure to ask: the next attempt may well succeed, and three that fail
// in a row mean the run should stop waiting for it.
func UnansweredLine() string {
	return linePrefix + fmt.Sprintf("commit signing did not answer within %ds. Nothing was committed. "+
		"Run the same git command again. %s "+
		"If it fails three times in a row, commit with git -c commit.gpgsign=false; for a rebase, abort it and run it again that way.",
		int(RequestTimeout.Seconds()), rebaseResumes)
}

// rebaseResumes is how a rebase stopped by a signature that may succeed next
// time carries on.
const rebaseResumes = "A rebase that stopped here resumes with git rebase --continue; " +
	"if that refuses because of staged changes, run git reset --hard first, and the rebase applies this commit's changes again."

// rebaseRestarts is how a rebase stopped once signing is off for the run
// finishes: a rebase keeps signing once started, so it is run again unsigned.
const rebaseRestarts = "A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false."

// objectPhrase names what a not-a-commit payload was, after "this is".
func objectPhrase(object string) string {
	switch object {
	case gitsig.ObjectTag:
		return "a tag"
	case gitsig.ObjectPushCertificate:
		return "a push certificate"
	}
	return "not a commit"
}

// signerOffReason is why signing stopped for the signer, as a sentence. A
// broken signer's reason is GitHub's verification reason for a pushed commit,
// or key_missing; a reason with no sentence of its own reads as GitHub having
// stopped verifying the signer's commits. mode is the kind of signer the run
// signs as: a pending deployment signer waits for a check on the control
// plane, not for an org admin, and no org admin removes it.
func signerOffReason(res Result, mode domain.CommitSigningMode) string {
	switch res.Refusal {
	case RefusalSignerRemoved:
		if mode == domain.CommitSigningDeployment {
			return "this organization no longer signs as the deployment's signing account"
		}
		return "an org admin removed the signer"
	case RefusalSignerChanged:
		return "the org now signs as a different account"
	}
	switch res.SignerStatus {
	case domain.CommitSignerPending:
		if mode == domain.CommitSigningDeployment {
			return "the deployment's signing key has not been found on its GitHub account yet"
		}
		return "the signer's key is waiting for an org admin to verify it on its GitHub account"
	case domain.CommitSignerBroken:
		switch res.SignerReason {
		case domain.CommitSignerReasonKeyMissing, domain.CommitVerificationUnknownKey:
			return "the signer's key is no longer on its GitHub account"
		case "unverified_email":
			return "GitHub reported a recent commit's email as not verified on the signer's account"
		case "no_user":
			return "GitHub reported a recent commit's email as belonging to no GitHub account"
		}
		return "GitHub stopped verifying the signer's commits"
	}
	return "the signer is no longer active"
}
