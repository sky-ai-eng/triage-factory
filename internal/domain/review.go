package domain

import (
	"fmt"
	"strings"
)

// Review-finding severity levels. These mirror the four levels the
// pr-review prompt assigns to every finding (see
// internal/ai/prompts/pr-review.txt). They are surfaced two ways: as a
// native chip in the local pre-render diff UI, and as a shields.io badge
// prepended to the comment body when the review is posted to GitHub.
//
// The canonical form is uppercase. NormalizeSeverity accepts any case so
// third-party review skills calling the gh tooling don't have to match
// exactly; SeverityBadgeMarkdown / the frontend chip key off the
// uppercase form.
const (
	SeverityBlocker = "BLOCKER"
	SeverityMajor   = "MAJOR"
	SeverityMinor   = "MINOR"
	SeverityClean   = "CLEAN"
)

// severityBadgeColor maps each level to a shields.io color. These are
// the Tailwind 500 hex values the frontend chip palette uses
// (ReviewComment.tsx), passed to shields as path-style hex so the GitHub
// badge tracks the in-app chip hue instead of drifting to shields'
// differently-shaded named colors (their named "yellow" #dfb317 vs
// Tailwind amber-500 #f59e0b, etc.).
var severityBadgeColor = map[string]string{
	SeverityBlocker: "ef4444", // red-500
	SeverityMajor:   "f97316", // orange-500
	SeverityMinor:   "f59e0b", // amber-500
	SeverityClean:   "3b82f6", // blue-500
}

// ValidSeverities is the ordered canonical set, for help text and error
// messages. Order is most-to-least severe.
var ValidSeverities = []string{SeverityBlocker, SeverityMajor, SeverityMinor, SeverityClean}

// NormalizeSeverity upper-cases and validates a severity string. An
// empty input is valid and returns "" (no badge — backward-compatible
// with callers that omit it). A non-empty value that isn't one of the
// four levels is an error so the agent gets a clear correction instead
// of silently posting an unrecognized label.
func NormalizeSeverity(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	up := strings.ToUpper(strings.TrimSpace(s))
	if _, ok := severityBadgeColor[up]; !ok {
		return "", fmt.Errorf("invalid severity %q; valid values: %s", s, strings.Join(ValidSeverities, ", "))
	}
	return up, nil
}

// SeverityBadgeMarkdown returns the shields.io badge line to prepend to a
// review comment body at GitHub-post time. Empty severity → empty string
// (the body is posted unchanged). The badge is wrapped in nested <sub>
// tags so it renders as a small inline chip ahead of the diagnosis,
// matching the convention other review bots (Codex, Copilot) use.
//
// This is the GitHub-render path only. The local pre-render UI renders a
// native chip from the stored Severity field instead — the badge markdown
// is never written into the stored comment body.
func SeverityBadgeMarkdown(severity string) string {
	if severity == "" {
		return ""
	}
	color, ok := severityBadgeColor[severity]
	if !ok {
		return ""
	}
	return fmt.Sprintf(
		"<sub><sub>![%s](https://img.shields.io/badge/%s-%s?style=flat)</sub></sub>\n\n",
		severity, severity, color,
	)
}

// PendingReview is a locally-managed review that hasn't been submitted to GitHub yet.
// DiffLines stores a JSON map of file -> line numbers that are valid comment targets.
// When ReviewEvent is set, the review has been "submitted" locally but is awaiting
// user approval before posting to GitHub.
//
// OriginalReviewBody / OriginalReviewEvent are write-once snapshots of the
// agent's first draft, captured by SetPendingReviewSubmission's COALESCE
// pattern. They survive any user edits via handleReviewUpdate and are read
// by the human-verdict writer (SKY-205) to compose the post-run
// `## Human feedback (post-run)` block.
//
// Pointer (rather than string + sentinel) so the formatter can tell apart
// "no snapshot exists" (nil — legacy row mid-flight when the columns were
// added) from "snapshot was a legitimate empty value" (non-nil pointer to
// ""). The body case matters because review bodies are commonly empty —
// agents that produced inline comments alone leave the top-level body
// blank — and we don't want a human-added body to silently suppress the
// diff just because the agent's draft was "".
type PendingReview struct {
	ID                  string
	PRNumber            int
	Owner               string
	Repo                string
	CommitSHA           string
	DiffLines           string  // JSON: {"file.go": [1,2,3,...], ...}
	DiffHunks           string  // JSON: {"file.go": [[start,end], ...], ...} — one [start,end] pair per hunk on the new side
	RunID               string  // agent run that created this review (empty for standalone CLI)
	ReviewBody          string  // deferred review body (set when awaiting approval)
	ReviewEvent         string  // deferred review event: APPROVE, COMMENT, REQUEST_CHANGES
	OriginalReviewBody  *string // agent's first draft body, write-once; nil = no snapshot
	OriginalReviewEvent *string // agent's first draft event, write-once; nil = no snapshot
}

// PendingReviewComment is a comment attached to a local pending review.
//
// OriginalBody is the write-once snapshot of the agent's drafted comment
// body, populated at INSERT in AddPendingReviewComment. UpdatePendingReviewComment
// mutates Body but never OriginalBody, giving SKY-205's writer a stable
// before/after pair for diff formatting. Pointer for the same reason as
// PendingReview's originals: nil = legacy row predating the column;
// non-nil pointer to "" = real snapshot of an empty drafted comment
// (rare but possible).
type PendingReviewComment struct {
	ID           string
	ReviewID     string
	Path         string
	Line         int
	StartLine    *int
	Body         string
	OriginalBody *string
	// Severity is one of the domain.Severity* constants, or "" when the
	// author (agent or third-party skill) didn't tag the finding. Stored
	// as data; rendered as a native chip in the diff UI and as a
	// shields.io badge when posted to GitHub. Never embedded in Body.
	Severity string
}
