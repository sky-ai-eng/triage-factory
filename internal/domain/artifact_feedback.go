package domain

import (
	"fmt"
	"strings"
)

// Artifact-change agent feedback (TFAC-493). When a human resolves an artifact
// a run produced — approving or dismissing a draft PR, submitting or dismissing
// a pending review — the agent that drafted it gets a concise, agent-facing note
// referencing the id it already knows (the PR number / the review handle it spoke
// to start-review/finalize-review). The note is delivered live (steered into a
// warm process) or, for a terminal/paused run, bundled into the next resume from
// the derived artifact-change ledger — but the *copy* lives here, in one place,
// so both delivery paths and every resolve handler render the same four strings.
//
// The four shapes are inferred from (Kind, State) so the live path (which has the
// just-flipped artifact in hand) and the ledger path (which re-derives from the
// artifact row's terminal state) agree without a second source of truth.

// systemNoteOpen / systemNoteClose wrap an agent-facing note. The agent's
// position prompt treats a <system-note>…</system-note> block as an out-of-band
// system message (a human acted on its work), distinct from the user talking to
// it. Exported indirectly via WrapSystemNote.
const (
	systemNoteOpen  = "<system-note>"
	systemNoteClose = "</system-note>"
)

// WrapSystemNote wraps body in <system-note>…</system-note> tags. Used for both
// a single live-injected resolution note and the bundled ledger block delivered
// ahead of a resume's user message.
func WrapSystemNote(body string) string {
	return systemNoteOpen + "\n" + body + "\n" + systemNoteClose
}

// ArtifactResolutionNote returns the one-line, agent-facing note for an artifact
// a human just resolved, or "" if the artifact is not in one of the four reported
// human-resolution states. The four strings are deliberately distinct so the
// agent can tell which of (PR | review) × (approved | dismissed) happened:
//
//   - pull_request → open      : approved, marked ready for review
//   - pull_request → closed    : dismissed, draft closed
//   - review       → submitted : approved, submitted to GitHub
//   - review       → dismissed : dismissed, discarded unsubmitted
//
// The note is bare (no <system-note> wrapper) — callers wrap it: the live path
// wraps one note, the ledger path wraps the joined block.
func ArtifactResolutionNote(a Artifact) string {
	switch a.Kind {
	case ArtifactKindPullRequest:
		ref := prResolutionRef(a)
		switch a.State {
		case ArtifactStatePROpen:
			return fmt.Sprintf("A human approved your draft pull request %s and marked it ready for review.", ref)
		case ArtifactStatePRClosed:
			return fmt.Sprintf("A human dismissed your draft pull request %s and closed it (its branch is kept).", ref)
		}
	case ArtifactKindReview:
		switch a.State {
		case ArtifactStateReviewSubmitted:
			return fmt.Sprintf("A human approved your pending review %s on %s and submitted it to GitHub.", reviewResolutionHandle(a), a.Target)
		case ArtifactStateReviewDismissed:
			return fmt.Sprintf("A human dismissed your pending review %s on %s without submitting it.", reviewResolutionHandle(a), a.Target)
		}
	}
	return ""
}

// IsResolutionNoteState reports whether (Kind, State) is one of the four human-
// resolution states ArtifactResolutionNote renders copy for. The ledger
// derivation uses it to filter a run's artifacts down to the resolved ones
// before formatting (equivalent to ArtifactResolutionNote(a) != "", named for
// the predicate's intent).
func IsResolutionNoteState(a Artifact) bool {
	return ArtifactResolutionNote(a) != ""
}

// ArtifactLedgerBlock renders the bundled <system-note> block prepended ahead of
// a resume's user message: every resolved artifact in arts, one bullet each, in
// the order given (the caller passes them oldest→newest). Returns "" when none of
// arts is in a reported resolution state, so a resume with no pending changes
// prepends nothing.
//
// The lead-in frames these as out-of-band human actions taken while the agent was
// not running, and points at the natural-failure contract: a later step that
// depended on a now-dismissed artifact just fails and reports it — this block is
// the explanation, not a guard (TFAC-493).
func ArtifactLedgerBlock(arts []Artifact) string {
	lines := make([]string, 0, len(arts))
	for i := range arts {
		if note := ArtifactResolutionNote(arts[i]); note != "" {
			lines = append(lines, "- "+note)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	body := "While you were not running, a human resolved artifacts you produced:\n" +
		strings.Join(lines, "\n") +
		"\nThese are informational. A later step that relied on a dismissed artifact will simply fail and report it."
	return WrapSystemNote(body)
}

// prResolutionRef is the PR reference the agent already knows — its
// owner/repo#number Target. Falls back to ExternalID (the bare PR number) if the
// Target is malformed/absent, so the note always carries some recognizable id.
func prResolutionRef(a Artifact) string {
	if a.Target != "" {
		return a.Target
	}
	if a.ExternalID != "" {
		return "#" + a.ExternalID
	}
	return "this PR"
}

// reviewResolutionHandle is the review id the agent already knows — the artifact
// id returned by start-review and spoken to add-review-comment/finalize-review
// (gh's <review_id>). Falls back to the submitted GitHub review id, then to a
// generic phrase, so the note never renders an empty handle.
func reviewResolutionHandle(a Artifact) string {
	if a.ID != "" {
		return a.ID
	}
	if a.ExternalID != "" {
		return "#" + a.ExternalID
	}
	return "the draft"
}
