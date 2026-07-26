package review

import (
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// PostureSubmits reports whether a finalized review draft should be submitted
// to GitHub immediately (true) or staged for a human to approve (false), given
// the team's configured posture, the identity of the credential that would
// post, and the review itself.
//
// The identity arm is the whole point of the setting. Under a GitHub App the
// review lands as a bot: readers calibrate to that, and someone who configured
// an automated review bot has already decided they want it to act — a per-run
// approval click there degrades into rubber-stamping, which launders unreviewed
// output as reviewed. Under a borrowed PAT the review posts as the lending
// user, where a wrong comment is that person being wrong in front of their
// colleagues. IdentityUnknown stages, per that type's documented contract:
// callers branching on identity for a safety decision treat it as the
// conservative case.
//
// An unrecognized posture stages. The handler validates the enum on write, so a
// value that reaches here off the closed set is corrupt state, and staging is
// the outcome that loses nothing.
func PostureSubmits(posture string, identity ghclient.Identity, event string, comments []domain.ReviewArtifactComment) bool {
	switch posture {
	case domain.ReviewPostureAuto:
		return true
	case domain.ReviewPostureAutoUnlessBlocking:
		return !consequential(event, comments)
	case domain.ReviewPostureIdentity:
		return identity == ghclient.IdentityApp
	default: // ReviewPostureDraft, "" (an unwritten column), anything unknown
		return false
	}
}

// consequential reports whether a review is one a human should see before it
// posts: a REQUEST_CHANGES verdict (it blocks the PR and is addressed to a
// person), or any inline comment the agent itself rated BLOCKER. Severity is
// read back out of the comment body, where add-review-comment baked the badge —
// the same inverse the review overlay's chip uses, so the UI and this gate
// agree on what "blocker" means.
func consequential(event string, comments []domain.ReviewArtifactComment) bool {
	if strings.EqualFold(event, "REQUEST_CHANGES") {
		return true
	}
	for _, c := range comments {
		if sev, _ := domain.ParseSeverityBadge(c.Body); sev == domain.SeverityBlocker {
			return true
		}
	}
	return false
}
