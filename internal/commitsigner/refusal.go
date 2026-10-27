package commitsigner

// Outcome is what one signature request came to, as the run that asked sees
// it: the label tf_commit_signatures_total counts it under.
type Outcome string

const (
	// OutcomeSigned is a signature the run can use.
	OutcomeSigned Outcome = "signed"
	// OutcomeRefused is a refusal, with the Refusal saying why.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnanswered is a request no answer reached in time. The run is
	// told to try the same command again.
	OutcomeUnanswered Outcome = "unanswered"
)

// Refusal names why a signature request was refused. It is a refused
// commit_signatures row's refusal_reason and is handed back to the run, which
// turns it into the line its agent reads, so the three values that refuse for
// the signer say which sentence that line carries.
type Refusal string

const (
	// RefusalSignerRemoved: the org has no signer now, so its runs commit
	// unsigned for the rest of their engagement.
	RefusalSignerRemoved Refusal = "signer_removed"
	// RefusalSignerChanged: the org signs as a different account, or a
	// different kind of signer, than the one the run was started with. A
	// run's identity is fixed when its sandbox starts, so it keeps
	// committing as the old signer, unsigned.
	RefusalSignerChanged Refusal = "signer_changed"
	// RefusalSignerInactive: the run's signer is still the one the org
	// resolves to, its own or the deployment's, and is not active.
	RefusalSignerInactive Refusal = "signer_inactive"
	// RefusalOrgMinuteBudget: the org made its per-minute budget of
	// signatures in the last minute. It carries the seconds until the budget
	// admits another.
	RefusalOrgMinuteBudget Refusal = "org_minute_budget"
	// RefusalClaimBudget: the engagement has spent its per-claim budget, so
	// signing is off for the rest of it.
	RefusalClaimBudget Refusal = "claim_budget"
	// RefusalClaimInactive: the request came from an engagement that no
	// longer holds its claim, or from an executor boot that no longer holds
	// it.
	RefusalClaimInactive Refusal = "claim_inactive"
	// RefusalUnanswered closes a request no answer could be given for: a pod
	// reached it only after the run had stopped waiting, or could not read
	// what it needed to decide. The run reads it as OutcomeUnanswered.
	RefusalUnanswered Refusal = "unanswered"

	// RefusalCommitterMismatch: the commit's committer is not the run's
	// signer. The daemon that holds the commit refuses it before anything is
	// signed, so no row records it.
	RefusalCommitterMismatch Refusal = "committer_mismatch"
	// RefusalNotACommit: what git asked to have signed is a tag, a push
	// certificate or no object at all. Refused by the daemon, like a
	// committer mismatch.
	RefusalNotACommit Refusal = "not_a_commit"
	// RefusalNotSigning: the run was started without a signer to sign as, so
	// nothing it asks for is signed. Git is configured not to ask in such a
	// run; this answers anything that asks anyway.
	RefusalNotSigning Refusal = "not_signing"
)

// metricReason is the refusal's value on tf_commit_signatures_total's reason
// label. The three signer refusals share one: the metric counts that signing
// stopped for the signer, and the signer's own status says why.
func (r Refusal) metricReason() string {
	switch r {
	case RefusalSignerRemoved, RefusalSignerChanged, RefusalNotSigning:
		return string(RefusalSignerInactive)
	}
	return string(r)
}

// Commit-sign-denied reasons, the detail_json reason of the commit_sign_denied
// audit row a refusal writes when it is a security signal (spec §5.6). The
// control plane records the first two; a local signature can record all three,
// and a multi-mode run's orchestrator records the third for its sidecar
// (RecordDaemonRefusal).
const (
	deniedClaimInactive     = "claim_inactive"
	deniedBudgetExhausted   = "budget_exhausted"
	deniedCommitterMismatch = "committer_mismatch"
)
