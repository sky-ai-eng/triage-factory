package commitsigner

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The lines are the spec's §4.2 table, word for word: an agent reads one and
// nothing else, so a reworded line is a changed instruction.
func TestRefusalLine(t *testing.T) {
	signer := Identity{Name: "Triage Factory", Email: "9+tf@users.noreply.github.com", Mode: domain.CommitSigningOrg}
	refused := func(r Refusal) Result { return Result{Outcome: OutcomeRefused, Refusal: r} }
	for _, tc := range []struct {
		name string
		res  Result
		want string
	}{
		{
			name: "committer mismatch",
			res: func() Result {
				r := refused(RefusalCommitterMismatch)
				r.Committer = commitsign.Identity{Name: "Mallory", Email: "mallory@example.com"}
				return r
			}(),
			want: "triagefactory: this run signs commits only as Triage Factory <9+tf@users.noreply.github.com>, " +
				"and this commit's committer is Mallory <mallory@example.com>. Nothing was committed. " +
				"Run the command again without setting the committer: no GIT_COMMITTER_NAME or GIT_COMMITTER_EMAIL, " +
				"and no -c user.name or -c user.email. --author is fine.",
		},
		{
			name: "a tag",
			res:  func() Result { r := refused(RefusalNotACommit); r.Object = commitsign.ObjectTag; return r }(),
			want: "triagefactory: this run signs commits only, and this is a tag. Nothing was created. " +
				"Create it without signing; for a tag, drop -s.",
		},
		{
			name: "a push certificate",
			res: func() Result {
				r := refused(RefusalNotACommit)
				r.Object = commitsign.ObjectPushCertificate
				return r
			}(),
			want: "triagefactory: this run signs commits only, and this is a push certificate. Nothing was created. " +
				"Create it without signing; for a tag, drop -s.",
		},
		{
			name: "no object git signs",
			res:  refused(RefusalNotACommit),
			want: "triagefactory: this run signs commits only, and this is not a commit. Nothing was created. " +
				"Create it without signing; for a tag, drop -s.",
		},
		{
			name: "the org's per-minute budget",
			res: func() Result {
				r := refused(RefusalOrgMinuteBudget)
				r.Limit, r.RetryAfterSeconds = 600, 12
				return r
			}(),
			want: "triagefactory: this org has made 600 signed commits in the last minute, its limit, " +
				"so signing resumes in 12s. Nothing was committed. Wait 12s, then run the same git command again. " +
				"A rebase that stopped here resumes with git rebase --continue; if that refuses because of staged changes, " +
				"run git reset --hard first, and the rebase applies this commit's changes again.",
		},
		{
			name: "the claim's budget",
			res:  func() Result { r := refused(RefusalClaimBudget); r.Limit = 5000; return r }(),
			want: "triagefactory: this run has made 5000 signed commits, the most one run may make, " +
				"so signing is off for the rest of this run. Nothing was committed. " +
				"Run this and every later commit with git -c commit.gpgsign=false. " +
				"A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false.",
		},
		{
			name: "a removed signer",
			res:  refused(RefusalSignerRemoved),
			want: "triagefactory: commit signing is off for the rest of this run: an org admin removed the signer. " +
				"Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. " +
				"A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false. " +
				"Commits are still authored as Triage Factory.",
		},
		{
			name: "another signer",
			res:  refused(RefusalSignerChanged),
			want: "triagefactory: commit signing is off for the rest of this run: the org now signs as a different account. " +
				"Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. " +
				"A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false. " +
				"Commits are still authored as Triage Factory.",
		},
		{
			name: "a key gone from the account",
			res: func() Result {
				r := refused(RefusalSignerInactive)
				r.SignerStatus, r.SignerReason = domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing
				return r
			}(),
			want: "triagefactory: commit signing is off for the rest of this run: the signer's key is no longer on its GitHub account. " +
				"Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. " +
				"A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false. " +
				"Commits are still authored as Triage Factory.",
		},
		{
			name: "an unverified email",
			res: func() Result {
				r := refused(RefusalSignerInactive)
				r.SignerStatus, r.SignerReason = domain.CommitSignerBroken, "unverified_email"
				return r
			}(),
			want: "triagefactory: commit signing is off for the rest of this run: GitHub reported a recent commit's email as not verified on the signer's account. " +
				"Nothing was committed. Run this and every later commit with git -c commit.gpgsign=false. " +
				"A rebase that stopped here still signs: run git rebase --abort, then the rebase again with git -c commit.gpgsign=false. " +
				"Commits are still authored as Triage Factory.",
		},
		{
			name: "no answer",
			res:  Result{Outcome: OutcomeUnanswered},
			want: "triagefactory: commit signing did not answer within 30s. Nothing was committed. " +
				"Run the same git command again. A rebase that stopped here resumes with git rebase --continue; " +
				"if that refuses because of staged changes, run git reset --hard first, and the rebase applies this commit's changes again. " +
				"If it fails three times in a row, commit with git -c commit.gpgsign=false; for a rebase, abort it and run it again that way.",
		},
		{
			name: "a released claim",
			res:  refused(RefusalClaimInactive),
			want: "triagefactory: this run no longer holds its engagement, so commit signing refused. Nothing was committed.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefusalLine(tc.res, signer); got != tc.want {
				t.Errorf("RefusalLine =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	if got := RefusalLine(Result{Outcome: OutcomeSigned, Signature: "sig"}, signer); got != "" {
		t.Errorf("a signed result has the line %q", got)
	}
}

// Every reason a signer is not active reads as a sentence, never a code.
func TestRefusalLine_SignerReasonsAreSentences(t *testing.T) {
	for _, reason := range []string{"key_missing", "unknown_key", "unverified_email", "no_user", "bad_email", "gpgverify_error", ""} {
		r := Result{Outcome: OutcomeRefused, Refusal: RefusalSignerInactive, SignerStatus: domain.CommitSignerBroken, SignerReason: reason}
		sentence := signerOffReason(r)
		if reason != "" && strings.Contains(sentence, reason) {
			t.Errorf("reason %q reads as %q", reason, sentence)
		}
	}
	if s := signerOffReason(Result{Refusal: RefusalSignerInactive, SignerStatus: domain.CommitSignerPending}); strings.Contains(s, "pending") {
		t.Errorf("a pending signer reads as %q", s)
	}
}

func TestParseGitVersion(t *testing.T) {
	for out, want := range map[string]LocalGit{
		"git version 2.43.0":                   {Version: "2.43.0"},
		"git version 2.34.0":                   {Version: "2.34.0"},
		"git version 2.39.3 (Apple Git-146)":   {Version: "2.39.3 (Apple Git-146)"},
		"git version 2.41.0.windows.1":         {Version: "2.41.0.windows.1"},
		"git version 2.33.8":                   {Version: "2.33.8", UnsignedReason: UnsignedGitTooOld},
		"git version 1.9.5":                    {Version: "1.9.5", UnsignedReason: UnsignedGitTooOld},
		"git version 3.0.0":                    {Version: "3.0.0"},
		"something that is not git's output":   {Version: "something that is not git's output", UnsignedReason: UnsignedGitUnavailable},
		"git version not-a-number.and-no-dots": {Version: "not-a-number.and-no-dots", UnsignedReason: UnsignedGitUnavailable},
	} {
		if got := parseGitVersion(out); got != want {
			t.Errorf("parseGitVersion(%q) = %+v, want %+v", out, got, want)
		}
	}
}
