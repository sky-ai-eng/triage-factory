package domain

import "fmt"

// Deliverable-change agent feedback. When a deliverable a conversation
// produced is resolved — a human approving or dismissing a draft PR or submitting or
// dismissing a pending review, GitHub showing a pull request resolved, or the pull request
// a pending review is about ending — the agent that drafted it gets a concise, agent-facing note
// referencing the id it already knows (the PR number / the review handle it spoke
// to start-review/finalize-review). Whoever resolves the deliverable stages
// the note on the conversation's staged-injection queue, which every runtime
// reads — but the *copy* lives here, in one place, so every producer renders
// the same strings.
//
// The shapes are inferred from (Kind, State) and the resolution the row's
// details record, so a producer needs only the row as the resolution stored it.

// systemNoteOpen / systemNoteClose wrap an agent-facing note. The agent's
// position prompt treats a <system-note>…</system-note> block as an out-of-band
// system message (a human acted on its work), distinct from the user talking to
// it. Exported indirectly via WrapSystemNote.
const (
	systemNoteOpen  = "<system-note>"
	systemNoteClose = "</system-note>"
)

// WrapSystemNote wraps body in <system-note>…</system-note> tags.
func WrapSystemNote(body string) string {
	return systemNoteOpen + "\n" + body + "\n" + systemNoteClose
}

// seenOnGitHub closes the note for a change the reconciler read from GitHub.
// Nothing records who made such a change, and the agent's own `gh pr ready`
// or `gh pr close` reaches the row the same way, a poll cycle later. So the
// note says how TF learned of the change and leaves the agent to recognize
// its own.
const seenOnGitHub = " Triage Factory saw this on GitHub some time after it happened, so it may be a change you made yourself."

// DeliverableResolutionNote returns the one-line, agent-facing note for a deliverable
// just resolved, or "" if the deliverable is not in one of the four reported
// resolution states. The strings are deliberately distinct so the agent can
// tell which of (PR | review) × (approved | dismissed) happened, and who or
// what did it:
//
//   - pull_request → open      : approved, marked ready for review — or, when
//     the details' resolution is github, marked ready on GitHub
//   - pull_request → closed    : dismissed, draft closed (branch kept) — or,
//     per the details' resolution, rejected (closed AND its branch deleted
//     from the upstream) or closed on GitHub
//   - review       → submitted : approved, submitted to GitHub
//   - review       → dismissed : dismissed, discarded unsubmitted — or, per the
//     details' resolution, discarded because the pull request it reviews
//     ended
//
// A change read from GitHub credits nobody (seenOnGitHub): the reconciler
// knows the pull request changed, not who changed it.
//
// A pull request settled foreign gets no note: no person resolved it, and its
// run did not open it.
//
// The note is bare (no <system-note> wrapper): it is staged as written, and
// whatever delivers the staged queue to the agent wraps it.
func DeliverableResolutionNote(a Deliverable) string {
	if a.Verification == DeliverableVerificationForeign {
		return ""
	}
	switch a.Kind {
	case DeliverableKindPullRequest:
		ref := prResolutionRef(a)
		d, _ := ParsePRDeliverableDetails(a.DetailsJSON)
		switch a.State {
		case DeliverableStatePROpen:
			if d.Resolution == PRResolutionGitHub {
				return fmt.Sprintf("Your draft pull request %s was marked ready for review on GitHub.%s", ref, seenOnGitHub)
			}
			return fmt.Sprintf("A human approved your draft pull request %s and marked it ready for review.", ref)
		case DeliverableStatePRClosed:
			switch d.Resolution {
			case PRResolutionRejected:
				return fmt.Sprintf("A human rejected your draft pull request %s: it was closed and its branch %s was deleted from the upstream, so the work on it is gone from the remote.", ref, prHeadBranchOrPlaceholder(d))
			case PRResolutionGitHub:
				return fmt.Sprintf("Your draft pull request %s was closed without merging on GitHub.%s", ref, seenOnGitHub)
			}
			return fmt.Sprintf("A human dismissed your draft pull request %s and closed it (its branch is kept).", ref)
		}
	case DeliverableKindReview:
		switch a.State {
		case DeliverableStateReviewSubmitted:
			return fmt.Sprintf("A human approved your pending review %s on %s and submitted it to GitHub.", reviewResolutionHandle(a), a.Target)
		case DeliverableStateReviewDismissed:
			d, _ := ParseReviewDeliverableDetails(a.DetailsJSON)
			if why := reviewEndedReason(d.Resolution); why != "" {
				return fmt.Sprintf("Your pending review %s on %s was discarded without being submitted: %s.", reviewResolutionHandle(a), a.Target, why)
			}
			return fmt.Sprintf("A human dismissed your pending review %s on %s without submitting it.", reviewResolutionHandle(a), a.Target)
		}
	}
	return ""
}

// prResolutionRef is the PR reference the agent already knows — its
// owner/repo#number Target, which NewPullRequestDeliverable always stamps. Falls
// back to ExternalID (the bare PR number) if the Target is malformed, then to a
// neutral placeholder, so the note never renders "pull request " with an empty
// id (the placeholder reads grammatically; both fallbacks are unreachable for a
// well-formed PR deliverable).
func prResolutionRef(a Deliverable) string {
	if a.Target != "" {
		return a.Target
	}
	if a.ExternalID != "" {
		return "#" + a.ExternalID
	}
	return "(reference unavailable)"
}

// reviewEndedReason is the clause saying how the pull request a dismissed
// review was about ended, for a review its resolution says that dismissed; ""
// for one a person dismissed.
func reviewEndedReason(resolution string) string {
	switch resolution {
	case ReviewResolutionPullRequestMerged:
		return "the pull request was merged"
	case ReviewResolutionPullRequestClosed:
		return "the pull request was closed without merging"
	case ReviewResolutionPullRequestUnreachable:
		return "Triage Factory can no longer read the pull request on GitHub"
	case ReviewResolutionPullRequestEnded:
		return "the pull request was merged or closed"
	}
	return ""
}

// reviewResolutionHandle is the review id the agent already knows — the deliverable's
// own id (its PK), returned by start-review and spoken to
// add-review-comment/finalize-review (gh's <review_id>). It is deliberately NOT
// the GitHub review id (ExternalID, stamped only at submit): the agent never saw
// that, so surfacing it would name an id the agent can't recognize. The PK is
// always set on a persisted review deliverable, so there's no empty-handle case to
// guard.
func reviewResolutionHandle(a Deliverable) string {
	return a.ID
}

// prHeadBranchOrPlaceholder names the head branch a rejection deleted, from the
// PR details; a placeholder when the row never recorded one, so the note reads
// grammatically rather than naming an empty branch.
func prHeadBranchOrPlaceholder(d PRDeliverableDetails) string {
	if d.HeadBranch != "" {
		return d.HeadBranch
	}
	return "(name unavailable)"
}
